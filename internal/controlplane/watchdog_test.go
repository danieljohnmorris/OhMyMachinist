package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/config"
)

func TestMaintainStateSweepsUnhealedFailure(t *testing.T) {
	server, store := newResolutionTestServer(t)
	jobID, _ := seedFailedRun(t, store, "", "go test: FAIL example.com/app", "")

	if err := server.maintainState(context.Background()); err != nil {
		t.Fatal(err)
	}

	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "queued" {
		t.Fatalf("job state = %q, want requeued", job.State)
	}
	if len(job.Runs) != 2 {
		t.Fatalf("runs = %d, want 2 after requeue", len(job.Runs))
	}
	if job.Metadata == nil || job.Metadata.Healing == nil {
		t.Fatalf("healing metadata missing after sweep: %+v", job.Metadata)
	}
}

func TestHealSweepGuards(t *testing.T) {
	server, store := newResolutionTestServer(t)

	// Already superseded: a later attempt exists, healing must not re-run.
	supersededID, _ := seedFailedRun(t, store, "", "go test: FAIL", "")
	_, err := store.db.Exec(`UPDATE jobs SET superseded_by='job_later' WHERE id=?`, supersededID)
	if err != nil {
		t.Fatal(err)
	}
	// Already requeued by hand: a newer job with the same repository and title.
	requeuedID, _ := seedFailedRun(t, store, "", "go test: FAIL", "")
	var title, repository string
	if err := store.db.QueryRow(`SELECT title, repository FROM jobs WHERE id=?`, requeuedID).Scan(&title, &repository); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateJobWithOptions(context.Background(), "Fix the sidebar.", repository, "plan", config.ResolvedCommand{Name: "plan", Timeout: time.Minute}, CreateJobOptions{Title: title}); err != nil {
		t.Fatal(err)
	}
	// Ancient failure: outside the sweep window.
	staleID, _ := seedFailedRun(t, store, "", "go test: FAIL", "")
	if _, err := store.db.Exec(`UPDATE jobs SET updated_at=? WHERE id=?`, time.Now().Add(-72*time.Hour).UTC().Format(time.RFC3339Nano), staleID); err != nil {
		t.Fatal(err)
	}

	if err := server.maintainState(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, jobID := range []string{supersededID, requeuedID, staleID} {
		job, err := store.GetJob(context.Background(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		if job.State != "failed" {
			t.Fatalf("job %s state = %q, want untouched failed", jobID, job.State)
		}
		if len(job.Runs) != 1 {
			t.Fatalf("job %s runs = %d, want 1 (no sweep requeue)", jobID, len(job.Runs))
		}
		if job.Metadata != nil && job.Metadata.Healing != nil {
			t.Fatalf("job %s gained healing metadata without a sweep", jobID)
		}
	}
}

func TestSupersededFailureRecordsResolutionKind(t *testing.T) {
	server, store := newResolutionTestServer(t)
	firstID, _ := seedFailedRun(t, store, "https://plane.example/MACH-6/", "go test: FAIL", "")
	secondID, _ := seedFailedRun(t, store, "https://plane.example/MACH-6/", "go test: FAIL", "")

	if err := store.linkTicketAttempts(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = server
	first, err := store.GetJob(context.Background(), firstID)
	if err != nil {
		t.Fatal(err)
	}
	if first.SupersededBy != secondID {
		t.Fatalf("superseded_by = %q, want %q", first.SupersededBy, secondID)
	}
	if first.ResolutionKind != "superseded" || !strings.Contains(first.ResolutionDetail, secondID) && first.ResolutionDetail == "" {
		t.Fatalf("resolution = %q/%q, want superseded with detail", first.ResolutionKind, first.ResolutionDetail)
	}
}

func TestMaintainStateWorkerStarvationAlert(t *testing.T) {
	server, webServer, alertsDirectory := newTestHTTPServerWithAlerts(t)
	defer webServer.Close()
	server.healSweepPending = false
	store := server.store

	// No queued runs: no alert file, and a stale one is cleared.
	if err := os.MkdirAll(alertsDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(alertsDirectory, "worker-starvation.json")
	if err := os.WriteFile(stale, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := server.maintainState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale starvation alert survived: %v", err)
	}

	// Queued run plus a fresh heartbeat: no alert.
	if _, err := store.CreateJob(context.Background(), "request", "machinist", "plan", testAgent("plan", "Plan request")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO workers(instance_id,name,last_seen_at) VALUES('w1','pipsqueek',?)`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := server.maintainState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("starvation alert written despite fresh heartbeat: %v", err)
	}

	// Heartbeat gone stale: the alert spools.
	if _, err := store.db.Exec(`UPDATE workers SET last_seen_at=? WHERE instance_id='w1'`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := server.maintainState(context.Background()); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(stale)
	if err != nil {
		t.Fatalf("starvation alert missing: %v", err)
	}
	if !strings.Contains(string(body), "queued_runs") {
		t.Fatalf("starvation alert body = %s", body)
	}
}
