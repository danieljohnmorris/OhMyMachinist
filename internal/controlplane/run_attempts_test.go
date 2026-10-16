package controlplane

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/protocol"
)

func namedPoll(instance, name string) protocol.PollRequest {
	request := pollRequest(instance, []string{"codex"}, []string{"machinist"})
	request.Name = name
	return request
}

func TestInPlaceRelaunchRecordsAttemptsTranscriptAndLane(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	now := time.Now()
	store.now = func() time.Time { return now }
	if _, err := store.CreateJob(t.Context(), "request", "machinist", "plan", testAgent("plan", "Plan request")); err != nil {
		t.Fatal(err)
	}
	laneA := namedPoll("worker-a", "lane-a")
	first, err := store.Poll(t.Context(), laneA)
	if err != nil || first == nil {
		t.Fatalf("poll first attempt: %v %v", first, err)
	}
	if first.Attempt != 1 {
		t.Fatalf("first attempt = %d, want 1", first.Attempt)
	}
	if strings.Contains(first.RenderedPrompt, "## Prior attempt") {
		t.Fatalf("first attempt prompt already carries prior context:\n%s", first.RenderedPrompt)
	}
	if err := store.AppendLiveEvents(t.Context(), first.ID, protocol.LiveEvents{
		InstanceID: "worker-a", LeaseToken: first.LeaseToken,
		Events: outputEvent("stdout", "hello from attempt 1\n"),
	}); err != nil {
		t.Fatal(err)
	}

	// The lease lapses. Another lane polls first but the run is pinned to lane-a,
	// so it stays queued until that lane returns.
	now = now.Add(leaseDuration + time.Second)
	laneB := namedPoll("worker-b", "lane-b")
	if stolen, err := store.Poll(t.Context(), laneB); err != nil || stolen != nil {
		t.Fatalf("competing lane took the pinned run: %v %v", stolen, err)
	}
	second, err := store.Poll(t.Context(), laneA)
	if err != nil || second == nil {
		t.Fatalf("poll relaunch: %v %v", second, err)
	}
	if second.ID != first.ID {
		t.Fatalf("relaunch changed the run ID: %s -> %s", first.ID, second.ID)
	}
	if second.Attempt != 2 {
		t.Fatalf("relaunch attempt = %d, want 2", second.Attempt)
	}
	for _, want := range []string{"## Prior attempt", endReasonLeaseExpired, "hello from attempt 1"} {
		if !strings.Contains(second.RenderedPrompt, want) {
			t.Fatalf("relaunch prompt missing %q:\n%s", want, second.RenderedPrompt)
		}
	}

	snapshot, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run := snapshot.Jobs[0].Runs[0]
	if len(run.Attempts) != 2 {
		t.Fatalf("recorded %d attempts, want 2: %#v", len(run.Attempts), run.Attempts)
	}
	if run.Attempts[0].Number != 1 || run.Attempts[1].Number != 2 {
		t.Fatalf("attempt numbers = %d,%d", run.Attempts[0].Number, run.Attempts[1].Number)
	}
	if run.Attempts[0].EndReason != endReasonLeaseExpired {
		t.Fatalf("attempt 1 end reason = %q, want %q", run.Attempts[0].EndReason, endReasonLeaseExpired)
	}
	if run.Attempts[0].WorkerName != "lane-a" || run.Attempts[1].WorkerName != "lane-a" {
		t.Fatalf("attempt workers = %q,%q, want lane-a", run.Attempts[0].WorkerName, run.Attempts[1].WorkerName)
	}

	// Completing attempt 2 keeps both attempts' transcripts on the run.
	if err := store.Complete(t.Context(), second.ID, protocol.Completion{
		InstanceID: "worker-a", LeaseToken: second.LeaseToken, State: "succeeded", ExitCode: 0,
		Events: outputEvent("stdout", "done in attempt 2\n"),
	}); err != nil {
		t.Fatal(err)
	}
	events, err := store.RunEvents(t.Context(), second.ID)
	if err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join(decodeEventTranscript(events.Events), "\n")
	for _, want := range []string{"hello from attempt 1", "done in attempt 2"} {
		if !strings.Contains(transcript, want) {
			t.Fatalf("run transcript missing %q:\n%s", want, transcript)
		}
	}
	snapshot, err = store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	run = snapshot.Jobs[0].Runs[0]
	if run.Attempts[1].EndReason != "exit code 0" {
		t.Fatalf("attempt 2 end reason = %q", run.Attempts[1].EndReason)
	}
}

func TestRelaunchLimitFailsTheRun(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	store.SetRelaunchLimit(1)
	now := time.Now()
	store.now = func() time.Time { return now }
	if _, err := store.CreateJob(t.Context(), "request", "machinist", "plan", testAgent("plan", "Plan request")); err != nil {
		t.Fatal(err)
	}
	worker := namedPoll("worker-a", "lane-a")
	if run, err := store.Poll(t.Context(), worker); err != nil || run == nil {
		t.Fatalf("poll first: %v %v", run, err)
	}
	// First relaunch is allowed.
	now = now.Add(leaseDuration + time.Second)
	if run, err := store.Poll(t.Context(), worker); err != nil || run == nil || run.Attempt != 2 {
		t.Fatalf("first relaunch: %v %v", run, err)
	}
	// Second expiry is past the cap: the run fails with the relaunch-limit reason.
	now = now.Add(leaseDuration + time.Second)
	if run, err := store.Poll(t.Context(), worker); err != nil || run != nil {
		t.Fatalf("poll past cap: %v %v", run, err)
	}
	snapshot, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	job := snapshot.Jobs[0]
	if job.State != "failed" {
		t.Fatalf("job state = %q, want failed", job.State)
	}
	if job.Runs[0].Error != endReasonRelaunchLimit {
		t.Fatalf("run error = %q, want %q", job.Runs[0].Error, endReasonRelaunchLimit)
	}
	attempts := job.Runs[0].Attempts
	if len(attempts) != 2 || attempts[1].EndReason != endReasonRelaunchLimit {
		t.Fatalf("attempts = %#v, want last end reason %q", attempts, endReasonRelaunchLimit)
	}
}
