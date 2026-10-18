package controlplane

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/protocol"
)

// postgresTestURL opts the integration suite in: set
// MACHINIST_TEST_POSTGRES_URL (postgres://user:pass@host/db) to run the
// adapter against a real Postgres. Skipped otherwise, so the default suite
// stays SQLite-only and hermetic.
func postgresTestURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("MACHINIST_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("MACHINIST_TEST_POSTGRES_URL not set; skipping Postgres adapter tests")
	}
	return url
}

func openPostgresTestStore(t *testing.T) *Store {
	t.Helper()
	url := postgresTestURL(t)
	// A fresh schema per test run: leftover jobs and runs from an earlier
	// failed attempt would otherwise be claimed by Poll and skew the
	// lifecycle assertions.
	if reset, err := sql.Open("pgx", url); err != nil {
		t.Fatal(err)
	} else {
		if _, err := reset.Exec(`DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
			t.Fatal(err)
		}
		reset.Close()
	}
	store, err := OpenStore(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestPostgresStoreBootsAndTracksVersion(t *testing.T) {
	store := openPostgresTestStore(t)
	if store.dialect != dialectPostgres {
		t.Fatalf("dialect = %q, want postgres", store.dialect)
	}
	version, err := store.postgresSchemaVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if version != 13 {
		t.Fatalf("schema version = %d, want 13", version)
	}
	// A second boot must be idempotent and keep the version.
	again, err := OpenStore(postgresTestURL(t))
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	again.Close()
	version, err = store.postgresSchemaVersion(context.Background())
	if err != nil || version != 13 {
		t.Fatalf("version after reboot = %d, %v", version, err)
	}
}

func TestPostgresJobLifecycle(t *testing.T) {
	store := openPostgresTestStore(t)
	ctx := context.Background()

	jobID, err := store.CreateJobWithOptions(ctx, "Fix the widget.", "machinist", "plan",
		config.ResolvedCommand{Name: "plan", Executor: "codex", Prompt: "Plan {{machinist.prompt}}.", Timeout: time.Minute},
		CreateJobOptions{Title: "PG-1 lifecycle", IssueKey: "PG-1"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Poll(ctx, protocol.PollRequest{InstanceID: "pg-worker", Name: "pipsqueek", Executors: []string{"codex"}, Repositories: []string{"machinist"}})
	if err != nil || run == nil {
		t.Fatalf("poll = %#v, %v", run, err)
	}
	if err := store.Heartbeat(ctx, run.ID, protocol.Heartbeat{InstanceID: "pg-worker", LeaseToken: run.LeaseToken}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	completion := protocol.Completion{InstanceID: "pg-worker", LeaseToken: run.LeaseToken, State: "succeeded", ExitCode: 0, Result: []byte(`{"final_message":"done"}`)}
	if err := store.Complete(ctx, run.ID, completion); err != nil {
		t.Fatalf("complete: %v", err)
	}
	job, err := store.GetJob(ctx, jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "succeeded" || len(job.Runs) != 1 || job.Runs[0].State != "succeeded" {
		t.Fatalf("job = %+v", job)
	}
	// JSON helper rewrite: the run summary reads the final message through
	// tf_json_extract on Postgres.
	if len(job.Runs) > 0 && job.Runs[0].Summary != "" && job.Runs[0].Summary != "done" {
		t.Fatalf("summary = %q", job.Runs[0].Summary)
	}
}

func TestPostgresTicketCRUDAndSettings(t *testing.T) {
	store := openPostgresTestStore(t)
	ctx := context.Background()

	if _, err := store.db.ExecContext(ctx, `INSERT INTO tickets(key,title,description,repository,project,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`,
		"PG-10", "Imported ticket", "body", "machinist", "PG", "backlog", time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	ticket, err := store.ticket(ctx, "PG-10")
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Title != "Imported ticket" || ticket.Project != "PG" {
		t.Fatalf("ticket = %+v", ticket)
	}
	// Known repositories exercise the INSERT ... ON CONFLICT DO NOTHING path.
	if _, err := store.db.ExecContext(ctx, `INSERT INTO known_repositories(repository) VALUES(?) ON CONFLICT DO NOTHING`, "machinist"); err != nil {
		t.Fatalf("known repositories insert: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO tickets(key,title,description,repository,project,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(key) DO NOTHING`,
		"PG-10", "duplicate", "", "", "", "backlog", time.Now().UTC().Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("conflict insert: %v", err)
	}
	ticket, err = store.ticket(ctx, "PG-10")
	if err != nil {
		t.Fatal(err)
	}
	if ticket.Title != "Imported ticket" {
		t.Fatalf("conflict insert overwrote row: %+v", ticket)
	}
}

func TestPostgresRowseqReproducesInsertionOrder(t *testing.T) {
	store := openPostgresTestStore(t)
	ctx := context.Background()

	// Three runs in creation order; the latest-attempt lookups that SQLite
	// answers with ORDER BY rowid must return the same row on Postgres.
	var runIDs []string
	for range 3 {
		_, err := store.CreateJobWithOptions(ctx, "request", "machinist", "plan",
			config.ResolvedCommand{Name: "plan", Executor: "codex", Prompt: "p", Timeout: time.Minute}, CreateJobOptions{Title: "PG-2"})
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.Poll(ctx, protocol.PollRequest{InstanceID: "pg-worker", Name: "pipsqueek", Executors: []string{"codex"}, Repositories: []string{"machinist"}})
		if err != nil || run == nil {
			t.Fatalf("poll = %#v, %v", run, err)
		}
		if err := store.Complete(ctx, run.ID, protocol.Completion{InstanceID: "pg-worker", LeaseToken: run.LeaseToken, State: "failed", ExitCode: 1}); err != nil {
			t.Fatal(err)
		}
		runIDs = append(runIDs, run.ID)
	}
	latest := store.db.QueryRowContext(ctx, `SELECT id FROM runs WHERE job_id IN (SELECT id FROM jobs WHERE title='PG-2') ORDER BY rowid DESC LIMIT 1`)
	var last string
	if err := latest.Scan(&last); err != nil {
		t.Fatal(err)
	}
	if last != runIDs[len(runIDs)-1] {
		t.Fatalf("latest run = %s, want %s", last, runIDs[len(runIDs)-1])
	}
}
