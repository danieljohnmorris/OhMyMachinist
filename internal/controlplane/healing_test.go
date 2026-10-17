package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/issues"
)

func TestClassifyFailure(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		want  HealCause
		files int
	}{
		{"dead endpoint", "dial tcp 10.0.0.5:443: connect: connection refused", CauseInfra, 0},
		{"offline host", "resolve: no such host glm.internal", CauseInfra, 0},
		{"stale checkout", "error: failed to push some refs (non-fast-forward); your branch is behind", CauseStale, 0},
		{"test failure", "--- FAIL: TestSidebar (0.01s)\nFAIL\texample.com/app 0.4s", CauseTest, 0},
		{"spec problem", "cannot proceed: specification is ambiguous about export formats", CauseSpec, 0},
		{"secret guard", "secret-file guard refused: .env, prod.env look like real env files", CauseSecret, 2},
		{"default is test", "something went wrong", CauseTest, 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cause, files := classifyFailure(testCase.text)
			if cause != testCase.want {
				t.Fatalf("cause = %q, want %q", cause, testCase.want)
			}
			if len(files) != testCase.files {
				t.Fatalf("files = %v, want %d entries", files, testCase.files)
			}
		})
	}
}

func TestHealingBudgetDefaultAndOverride(t *testing.T) {
	healing := config.Healing{}
	if got := healing.DiagnoseBudget("MACH"); got != 2 {
		t.Fatalf("default budget = %d, want 2", got)
	}
	healing = config.Healing{MaxRetries: 5}
	if got := healing.DiagnoseBudget("MACH"); got != 5 {
		t.Fatalf("configured budget = %d, want 5", got)
	}
	healing = config.Healing{MaxRetries: 5, MaxRetriesPerProject: map[string]int{"MACH": 1}}
	if got := healing.DiagnoseBudget("MACH"); got != 1 {
		t.Fatalf("per-project budget = %d, want 1", got)
	}
	healing = config.Healing{MaxRetries: -1}
	if got := healing.DiagnoseBudget("MACH"); got != 0 {
		t.Fatalf("disabled budget = %d, want 0", got)
	}
}

// seedFailedRun inserts a failed plain job with one failed run, the state a
// completion leaves behind.
func seedFailedRun(t *testing.T, store *Store, subject, errorText, events string) (string, string) {
	t.Helper()
	jobID, err := store.CreateJobWithOptions(context.Background(), "Fix the sidebar.", "sparkdash", "plan", config.ResolvedCommand{Name: "plan", Timeout: time.Minute}, CreateJobOptions{Title: "MACH-6 sidebar", IssueKey: "MACH-6"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET state='failed', trigger_subject=?, issue_key='MACH-6' WHERE id=?`, subject, jobID); err != nil {
		t.Fatal(err)
	}
	runID, err := randomID("run", 12)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO runs(id,job_id,command,command_hash,executor,model,repository,rendered_prompt,timeout_ms,state,exit_code,error,events,completed_at) VALUES(?,?,?,?,?,?,?,?,?, 'failed',1,?,?,?)`,
		runID, jobID, "plan", "", "test", "", "sparkdash", "Fix the sidebar.", int64(60000), errorText, events, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	// The seed replaces the placeholder run CreateJobWithOptions inserted.
	if _, err := store.db.Exec(`DELETE FROM runs WHERE job_id=? AND id<>?`, jobID, runID); err != nil {
		t.Fatal(err)
	}
	return jobID, runID
}

func newHealingTestServer(t *testing.T) (*Server, *Store, *[]planeRequest) {
	t.Helper()
	server, store := newResolutionTestServer(t)
	client, requests := newPlaneFixture(t)
	server.planeClients = map[string]*issues.PlaneClient{"": client}
	return server, store, requests
}

func TestHealRunRequeuesTestFailure(t *testing.T) {
	server, store, _ := newHealingTestServer(t)
	jobID, runID := seedFailedRun(t, store, "https://plane.example/MACH-6/", "go test: FAIL example.com/app", "")

	server.HealRun(context.Background(), runID)

	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "queued" {
		t.Fatalf("job state = %q, want queued", job.State)
	}
	if len(job.Runs) != 2 {
		t.Fatalf("runs = %d, want 2", len(job.Runs))
	}
	var requeued bool
	for _, run := range job.Runs {
		if run.State == "queued" && strings.Contains(job.Prompt, "amend the commit on the same branch") {
			requeued = true
		}
	}
	if !requeued {
		t.Fatalf("requeued run missing healing instruction; prompt: %s", job.Prompt)
	}
	if job.Metadata == nil || job.Metadata.Healing == nil || job.Metadata.Healing.Cause != CauseTest {
		t.Fatalf("healing metadata missing: %+v", job.Metadata)
	}
}

func TestHealRunInfraSchedulesBackoffThenRequeues(t *testing.T) {
	server, store, _ := newHealingTestServer(t)
	jobID, runID := seedFailedRun(t, store, "https://plane.example/MACH-6/", "dial tcp: connection refused", "")

	server.HealRun(context.Background(), runID)

	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "failed" {
		t.Fatalf("job state = %q, want failed while backing off", job.State)
	}
	if job.Metadata == nil || job.Metadata.Healing == nil || job.Metadata.Healing.RetryAfter == "" {
		t.Fatalf("backoff metadata missing: %+v", job.Metadata)
	}
	if len(job.Runs) != 1 {
		t.Fatalf("runs = %d, want 1 before backoff elapses", len(job.Runs))
	}

	// Once the backoff elapses the scheduler requeues the job.
	due := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := store.db.Exec(`UPDATE job_metadata SET metadata=json_set(metadata,'$.healing.retry_after',?) WHERE job_id=?`, due, jobID); err != nil {
		t.Fatal(err)
	}
	if err := server.healPendingRetries(context.Background()); err != nil {
		t.Fatal(err)
	}
	job, err = store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "queued" || len(job.Runs) != 2 {
		t.Fatalf("after backoff: state=%q runs=%d, want queued/2", job.State, len(job.Runs))
	}
}

func TestHealRunExhaustedBudgetSetsNeedsAttention(t *testing.T) {
	server, store, requests := newHealingTestServer(t)
	jobID, runID := seedFailedRun(t, store, "https://plane.example/MACH-6/", "FAIL", "")
	server.healing = config.Healing{MaxRetries: 0} // default budget: 2 retries

	// Three attempts already: the budget is spent.
	for _, state := range []string{"failed", "failed"} {
		extraID, err := randomID("run", 12)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.Exec(`INSERT INTO runs(id,job_id,command,command_hash,executor,model,repository,rendered_prompt,timeout_ms,state,error,completed_at) VALUES(?,?,?,'','test','','sparkdash','FAIL',60000,?,'FAIL',?)`, extraID, jobID, "plan", state, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}

	server.HealRun(context.Background(), runID)

	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "failed" {
		t.Fatalf("job state = %q, want failed (budget spent)", job.State)
	}
	if len(job.Runs) != 3 {
		t.Fatalf("runs = %d, want 3", len(job.Runs))
	}
	var commented, needsAttention bool
	for _, request := range *requests {
		if request.Method == "POST" && strings.Contains(request.Body, "Retry budget exhausted") {
			commented = true
		}
		if request.Method == "PATCH" && strings.Contains(request.Path, "/issues/") {
			needsAttention = true
		}
	}
	if !commented || !needsAttention {
		t.Fatalf("plane requests missing comment=%v needs-attention=%v: %+v", commented, needsAttention, *requests)
	}
}

func TestHealRunSpecAndSecretDoNotRetry(t *testing.T) {
	server, store, requests := newHealingTestServer(t)
	specJob, specRun := seedFailedRun(t, store, "https://plane.example/MACH-6/", "", "the specification is ambiguous about the export format")
	server.HealRun(context.Background(), specRun)
	spec, err := store.GetJob(context.Background(), specJob)
	if err != nil {
		t.Fatal(err)
	}
	if spec.State != "failed" || len(spec.Runs) != 1 {
		t.Fatalf("spec cause requeued: state=%q runs=%d", spec.State, len(spec.Runs))
	}

	secretJob, secretRun := seedFailedRun(t, store, "https://plane.example/MACH-6/", "", "secret-file guard refused: prod.env, staging.pem")
	server.HealRun(context.Background(), secretRun)
	secret, err := store.GetJob(context.Background(), secretJob)
	if err != nil {
		t.Fatal(err)
	}
	if secret.State != "failed" || len(secret.Runs) != 1 {
		t.Fatalf("secret cause requeued: state=%q runs=%d", secret.State, len(secret.Runs))
	}
	var stateSet bool
	for _, request := range *requests {
		if request.Method == "PATCH" && strings.Contains(request.Path, "/issues/") {
			stateSet = true
		}
		// The budget was not spent for these causes; the comment must not
		// claim it was.
		if strings.Contains(request.Body, "Retry budget exhausted") {
			t.Fatalf("plane comment claims budget exhausted: %s", request.Body)
		}
	}
	if !stateSet {
		t.Fatal("ticket was not moved to Needs attention")
	}
	var exhausted bool
	if err := store.db.QueryRow(`SELECT COALESCE(json_extract(metadata,'$.healing.exhausted'),0) FROM job_metadata WHERE job_id=?`, specJob).Scan(&exhausted); err != nil {
		t.Fatal(err)
	}
	if exhausted {
		t.Fatal("job metadata records exhausted for a spec cause with budget remaining")
	}
}

func TestHealRunDiagnoseCommand(t *testing.T) {
	server, store, _ := newHealingTestServer(t)
	stub := filepath.Join(t.TempDir(), "diagnose.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\ncat >/dev/null\necho '{\"cause\":\"stale\",\"summary\":\"checkout behind origin\"}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	server.healing = config.Healing{DiagnoseCommand: []string{stub}}

	jobID, runID := seedFailedRun(t, store, "https://plane.example/MACH-6/", "FAIL", "")
	server.HealRun(context.Background(), runID)

	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Metadata == nil || job.Metadata.Healing == nil || job.Metadata.Healing.Cause != CauseStale || job.Metadata.Healing.Summary != "checkout behind origin" {
		t.Fatalf("diagnose command result not recorded: %+v", job.Metadata)
	}
	if job.State != "queued" {
		t.Fatalf("job state = %q, want queued (stale fix requeues)", job.State)
	}
}

func TestHealRunSkipsWorkflowJobs(t *testing.T) {
	server, store, _ := newHealingTestServer(t)
	jobID, runID := seedFailedRun(t, store, "https://plane.example/MACH-6/", "FAIL", "")
	if _, err := store.db.Exec(`INSERT INTO workflow_jobs(job_id,name,plan) VALUES(?,?,?)`, jobID, "plan", `[]`); err != nil {
		t.Fatal(err)
	}
	server.HealRun(context.Background(), runID)
	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "failed" || len(job.Runs) != 1 {
		t.Fatalf("workflow job was healed: state=%q runs=%d", job.State, len(job.Runs))
	}
}
