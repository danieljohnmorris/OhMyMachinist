package controlplane

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/protocol"
)

// A requeued ticket is a new attempt of the same task: it links to the previous
// job (previous_job_id/attempt/superseded_by) and carries the ticket key, so
// the board keeps one card and the attempt picker lists every attempt.
func TestRequeueLinksNextAttemptUnderTheSameTask(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "db"))
	now := time.Date(2026, time.October, 13, 9, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	options := CreateJobOptions{Title: "SPARK-7: Ship it"}

	first, err := store.CreateJobWithOptions(t.Context(), "Ship it", "machinist", "build", testAgent("build", "Ship it"), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.linkTicketAttempts(t.Context()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	second, err := store.CreateJobWithOptions(t.Context(), "Ship it", "machinist", "build", testAgent("build", "Ship it"), options)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.linkTicketAttempts(t.Context()); err != nil {
		t.Fatal(err)
	}

	older, retry := jobByID(t, store, first), jobByID(t, store, second)
	if retry.Attempt != 2 || retry.PreviousJobID != first || retry.SupersededBy != "" {
		t.Fatalf("retry = attempt %d previous %q superseded %q", retry.Attempt, retry.PreviousJobID, retry.SupersededBy)
	}
	if older.Attempt != 1 || older.PreviousJobID != "" || older.SupersededBy != second {
		t.Fatalf("superseded attempt = %#v", older)
	}
	if older.IssueKey != "SPARK-7" || retry.IssueKey != "SPARK-7" {
		t.Fatalf("issue keys = %q, %q", older.IssueKey, retry.IssueKey)
	}
}

// Claiming a job records when it started and which worker took it.
func TestClaimRecordsStartTimeAndWorker(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "db"))
	jobID, err := store.CreateJob(t.Context(), "Plan it", "machinist", "plan", testAgent("plan", "Plan it"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Poll(t.Context(), pollRequest("worker-a", []string{"codex"}, []string{"machinist"}))
	if err != nil || run == nil {
		t.Fatalf("poll = %#v, %v", run, err)
	}
	claimed := jobByID(t, store, jobID).Runs[0]
	if claimed.State != "running" || claimed.WorkerName != "test" || claimed.StartedAt.IsZero() {
		t.Fatalf("claimed run = %#v", claimed)
	}
}

// Cancelling records the actor and reason the attempt was stopped.
func TestCancelRecordsActorAndReason(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "db"))
	jobID, err := store.CreateJob(t.Context(), "Ship it", "machinist", "build", testAgent("build", "Ship it"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowActionAs(t.Context(), jobID, "", "cancel", false, "dan", "superseded by retry"); err != nil {
		t.Fatal(err)
	}
	cancelled := jobByID(t, store, jobID).Runs[0]
	if cancelled.State != "cancelled" || cancelled.CancelActor != "dan" || cancelled.CancelReason != "superseded by retry" {
		t.Fatalf("cancelled run = %#v", cancelled)
	}
}

// A cancel without an actor or reason still records a responsible principal.
func TestCancelDefaultsActorAndReason(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "db"))
	jobID, err := store.CreateJob(t.Context(), "Ship it", "machinist", "build", testAgent("build", "Ship it"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowAction(t.Context(), jobID, "", "cancel", false); err != nil {
		t.Fatal(err)
	}
	cancelled := jobByID(t, store, jobID).Runs[0]
	if cancelled.CancelActor != "operator" || cancelled.CancelReason != "Cancelled by operator" {
		t.Fatalf("default cancel = actor %q reason %q", cancelled.CancelActor, cancelled.CancelReason)
	}
}

// Cancelling a running attempt keeps the transcript the worker finishes with,
// instead of truncating at the last streamed chunk.
func TestCancelledRunKeepsFinalTranscript(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "db"))
	id := workflowJob(t, store, false)
	run, err := store.Poll(t.Context(), workflowWorker())
	if err != nil || run == nil {
		t.Fatalf("poll = %#v, %v", run, err)
	}
	if err := store.AppendLiveEvents(t.Context(), run.ID, protocol.LiveEvents{InstanceID: "worker-a", LeaseToken: run.LeaseToken, Events: "partial\n"}); err != nil {
		t.Fatal(err)
	}
	if err := store.WorkflowActionAs(t.Context(), id, run.ID, "cancel", false, "dan", "stopped by hand"); err != nil {
		t.Fatal(err)
	}
	const full = "partial\nfull second line\n"
	if err := store.Complete(t.Context(), run.ID, protocol.Completion{InstanceID: "worker-a", LeaseToken: run.LeaseToken, State: "cancelled", ExitCode: 130, Events: full}); err != nil {
		t.Fatal(err)
	}
	output, err := store.RunOutput(t.Context(), run.ID)
	if err != nil || output.Events != full {
		t.Fatalf("kept transcript = %q, %v", output.Events, err)
	}
	// The late completion never revives the stopped attempt.
	if state := jobByID(t, store, id).Runs[0].State; state != "cancelled" {
		t.Fatalf("run state = %q", state)
	}
}

// A timed-out run keeps the transcript its completion delivered.
func TestTimedOutRunKeepsTranscript(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "db"))
	jobID, err := store.CreateJob(t.Context(), "Plan it", "machinist", "plan", testAgent("plan", "Plan it"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Poll(t.Context(), pollRequest("worker-a", []string{"codex"}, []string{"machinist"}))
	if err != nil || run == nil {
		t.Fatalf("poll = %#v, %v", run, err)
	}
	const log = "started\nstill running\ntimed out\n"
	if err := store.Complete(t.Context(), run.ID, protocol.Completion{InstanceID: "worker-a", LeaseToken: run.LeaseToken, State: "timed_out", ExitCode: 124, Error: "timed out", Events: log}); err != nil {
		t.Fatal(err)
	}
	output, err := store.RunOutput(t.Context(), run.ID)
	if err != nil || output.Events != log {
		t.Fatalf("kept transcript = %q, %v", output.Events, err)
	}
	if terminal := jobByID(t, store, jobID).Runs[0]; terminal.State != "timed_out" {
		t.Fatalf("run = %#v", terminal)
	}
}

// Killing a worker loses its streamed transcript only if reclaim clears it; the
// already-streamed chunks must survive an expired lease.
func TestKilledRunKeepsStreamedTranscript(t *testing.T) {
	clock := newTestClock(time.Date(2026, time.October, 13, 9, 0, 0, 0, time.UTC))
	store := openTestStore(t, filepath.Join(t.TempDir(), "db"))
	store.now = clock.Now
	if _, err := store.CreateJob(t.Context(), "Plan it", "machinist", "plan", testAgent("plan", "Plan it")); err != nil {
		t.Fatal(err)
	}
	run, err := store.Poll(t.Context(), pollRequest("worker-a", []string{"codex"}, []string{"machinist"}))
	if err != nil || run == nil {
		t.Fatalf("poll = %#v, %v", run, err)
	}
	const streamed = "streamed before the kill\n"
	if err := store.AppendLiveEvents(t.Context(), run.ID, protocol.LiveEvents{InstanceID: "worker-a", LeaseToken: run.LeaseToken, Events: streamed}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(leaseDuration + time.Second)
	if _, err := store.ReclaimExpiredLeases(t.Context()); err != nil {
		t.Fatal(err)
	}
	late := protocol.Completion{InstanceID: "worker-a", LeaseToken: run.LeaseToken, State: "failed", ExitCode: 1, Events: "late\n"}
	if err := store.Complete(t.Context(), run.ID, late); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("late completion = %v", err)
	}
	output, err := store.RunOutput(t.Context(), run.ID)
	if err != nil || output.Events != streamed {
		t.Fatalf("kept transcript = %q, %v", output.Events, err)
	}
}

// A control-plane restart must not cancel or orphan running jobs: the startup
// lease grace keeps them leased so the reclaim pass leaves them alone and the
// live worker can keep heartbeating.
func TestStartupGraceKeepsRunningJobsAlive(t *testing.T) {
	clock := newTestClock(time.Date(2026, time.October, 13, 9, 0, 0, 0, time.UTC))
	store := openTestStore(t, filepath.Join(t.TempDir(), "db"))
	store.now = clock.Now
	jobID, err := store.CreateJob(t.Context(), "Plan it", "machinist", "plan", testAgent("plan", "Plan it"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Poll(t.Context(), pollRequest("worker-a", []string{"codex"}, []string{"machinist"}))
	if err != nil || run == nil {
		t.Fatalf("poll = %#v, %v", run, err)
	}
	// The control plane was down past the worker's 30-second lease.
	clock.Advance(leaseDuration + time.Minute)
	if _, err := store.GraceRunningLeases(t.Context()); err != nil {
		t.Fatal(err)
	}
	reclaimed, err := store.ReclaimExpiredLeases(t.Context())
	if err != nil || reclaimed != 0 {
		t.Fatalf("reclaimed %d after restart, %v", reclaimed, err)
	}
	kept := jobByID(t, store, jobID)
	if kept.State != "running" || kept.Runs[0].State != "running" {
		t.Fatalf("restart changed running job: %#v", kept)
	}
	if err := store.Heartbeat(t.Context(), run.ID, protocol.Heartbeat{InstanceID: "worker-a", LeaseToken: run.LeaseToken}); err != nil {
		t.Fatalf("worker could not re-establish its lease after restart: %v", err)
	}
}
