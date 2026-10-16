package controlplane

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/protocol"
)

func claimRunningJob(t *testing.T, store *Store, worker string) (string, protocol.RunSpec) {
	t.Helper()
	jobID, err := store.CreateJob(t.Context(), "Ship it", "machinist", "build", testAgent("build", "{{machinist.prompt}}"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Poll(t.Context(), pollRequest(worker, []string{"codex"}, []string{"machinist"}))
	if err != nil || run == nil {
		t.Fatalf("poll = %#v, %v", run, err)
	}
	return jobID, *run
}

func TestMoveToQueueRequeuesRunningJobAsNextAttempt(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	jobID, _ := claimRunningJob(t, store, "worker-a")

	// An older queued job must be dispatched before the requeued attempt:
	// move to queue sends the new attempt to the back of the queue.
	behindID, err := store.CreateJob(t.Context(), "Behind it", "machinist", "build", testAgent("build", "{{machinist.prompt}}"))
	if err != nil {
		t.Fatal(err)
	}

	newID, err := store.MoveToQueue(t.Context(), jobID)
	if err != nil {
		t.Fatalf("move to queue: %v", err)
	}
	if newID == "" || newID == jobID || newID == behindID {
		t.Fatalf("move to queue returned %q", newID)
	}

	// The stopped attempt is cancelled with the move reason.
	moved, err := store.GetJob(t.Context(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.State != "cancelled" || moved.SupersededBy != newID {
		t.Fatalf("moved job = %#v", moved)
	}
	if len(moved.Runs) != 1 || moved.Runs[0].State != "cancelled" || moved.Runs[0].Error != "moved to queue" {
		t.Fatalf("moved runs = %#v", moved.Runs)
	}
	if moved.Runs[0].ExitCode == nil || *moved.Runs[0].ExitCode != 130 {
		t.Fatalf("moved run exit code = %#v", moved.Runs[0].ExitCode)
	}

	// The requeued attempt is the same job on the same card, attempt N+1.
	next, err := store.GetJob(t.Context(), newID)
	if err != nil {
		t.Fatal(err)
	}
	if next.State != "queued" || next.Attempt != 2 || next.PreviousJobID != jobID || next.IssueKey != moved.IssueKey {
		t.Fatalf("requeued job = %#v", next)
	}
	if next.Repository != moved.Repository || next.Command != moved.Command {
		t.Fatalf("requeued job = %#v, prior = %#v", next, moved)
	}
	if len(next.Runs) != 1 || next.Runs[0].State != "queued" {
		t.Fatalf("requeued runs = %#v", next.Runs)
	}
	if !strings.Contains(next.Prompt, "## Prior attempt") {
		t.Fatalf("requeued prompt has no prior attempt context: %q", next.Prompt)
	}

	// Exactly one requeued job: no duplicate.
	jobs, err := store.ListJobs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 3 {
		t.Fatalf("jobs after move = %d (%#v)", len(jobs), jobs)
	}

	// The requeued attempt waits behind the older queued job.
	first, err := store.Poll(t.Context(), pollRequest("worker-b", []string{"codex"}, []string{"machinist"}))
	if err != nil || first == nil {
		t.Fatalf("poll after move = %#v, %v", first, err)
	}
	if first.JobID != behindID {
		t.Fatalf("first poll claimed %q, want the older job %q", first.JobID, behindID)
	}
	second, err := store.Poll(t.Context(), pollRequest("worker-c", []string{"codex"}, []string{"machinist"}))
	if err != nil || second == nil {
		t.Fatalf("second poll = %#v, %v", second, err)
	}
	if second.JobID != newID {
		t.Fatalf("second poll claimed %q, want the requeued attempt %q", second.JobID, newID)
	}
}

func TestMoveToQueueSurvivesHungAgentAndDeadWorker(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))

	// A hung agent: the lease is still valid but the worker never answers.
	jobID, run := claimRunningJob(t, store, "worker-a")
	hung, err := store.MoveToQueue(t.Context(), jobID)
	if err != nil {
		t.Fatalf("move to queue with hung agent: %v", err)
	}

	// The next heartbeat gets the lease-revoked answer.
	if err := store.Heartbeat(t.Context(), run.ID, protocol.Heartbeat{InstanceID: "worker-a", LeaseToken: run.LeaseToken}); !errors.Is(err, ErrRunState) {
		t.Fatalf("heartbeat after move = %v, want ErrRunState", err)
	}

	// A late completion from the revoked worker changes nothing.
	if err := store.Complete(t.Context(), run.ID, protocol.Completion{InstanceID: "worker-a", LeaseToken: run.LeaseToken, State: "succeeded", ExitCode: 0}); err != nil {
		t.Fatalf("complete after move: %v", err)
	}
	moved, err := store.GetJob(t.Context(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.State != "cancelled" || moved.SupersededBy != hung {
		t.Fatalf("moved job after late completion = %#v", moved)
	}

	// A dead worker: the lease expired long ago and no worker will ever
	// confirm. The move still works without stopping anything. The job is
	// marked running directly, the way a worker that vanished mid-run leaves it.
	deadID, err := store.CreateJob(t.Context(), "Dead worker", "machinist", "build", testAgent("build", "{{machinist.prompt}}"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET state='running' WHERE id=?`, deadID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE runs SET state='running',worker_instance='worker-gone',worker_name='worker-gone',lease_token='lease-dead',lease_expires_at=?,started_at=? WHERE job_id=?`, time.Now().Add(-time.Hour).UnixNano(), time.Now().Add(-time.Hour).Format(time.RFC3339Nano), deadID); err != nil {
		t.Fatal(err)
	}
	dead, err := store.MoveToQueue(t.Context(), deadID)
	if err != nil {
		t.Fatalf("move to queue with dead worker: %v", err)
	}
	if dead == "" || dead == deadID {
		t.Fatalf("move to queue returned %q", dead)
	}
	// The reclaim never resurrects a moved attempt.
	if _, err := store.ReclaimExpiredLeases(t.Context()); err != nil {
		t.Fatal(err)
	}
	gone, err := store.GetJob(t.Context(), deadID)
	if err != nil {
		t.Fatal(err)
	}
	if gone.State != "cancelled" || gone.Runs[0].State != "cancelled" {
		t.Fatalf("dead-worker job after reclaim = %#v", gone)
	}
	next, err := store.GetJob(t.Context(), dead)
	if err != nil {
		t.Fatal(err)
	}
	if next.State != "queued" || next.Attempt != 2 {
		t.Fatalf("requeued job after dead worker = %#v", next)
	}
}

func TestMoveToQueueIsIdempotent(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	jobID, _ := claimRunningJob(t, store, "worker-a")

	first, err := store.MoveToQueue(t.Context(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.MoveToQueue(t.Context(), jobID)
	if err != nil {
		t.Fatalf("repeated move to queue: %v", err)
	}
	if first != second {
		t.Fatalf("repeated move returned %q, want %q", second, first)
	}
	jobs, err := store.ListJobs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("repeated move created jobs: %#v", jobs)
	}
}

// A running job whose superseded_by was set by the ticket-chain linker (a
// newer job exists for the same ticket) is not a completed move: the run must
// still be cancelled and requeued, and the workflow refusal must still apply.
func TestMoveToQueueRunningJobWithChainLinkStillMoves(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	jobID, err := store.CreateJobWithOptions(t.Context(), "first pass", "machinist", "build", testAgent("build", "{{machinist.prompt}}"), CreateJobOptions{Title: "MACH-65 first pass"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Poll(t.Context(), pollRequest("worker-a", []string{"codex"}, []string{"machinist"})); err != nil {
		t.Fatal(err)
	}
	// A newer job on the same ticket exists, so the attempt linker records it
	// as the chain successor of the still-running job.
	newerID, err := store.CreateJobWithOptions(t.Context(), "second pass", "machinist", "build", testAgent("build", "{{machinist.prompt}}"), CreateJobOptions{Title: "MACH-65 second pass"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.linkTicketAttempts(t.Context()); err != nil {
		t.Fatal(err)
	}
	linked, err := store.GetJob(t.Context(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if linked.SupersededBy != newerID {
		t.Fatalf("chain link = %q, want %q", linked.SupersededBy, newerID)
	}

	newID, err := store.MoveToQueue(t.Context(), jobID)
	if err != nil {
		t.Fatalf("move to queue: %v", err)
	}
	if newID == "" || newID == newerID {
		t.Fatalf("move to queue returned %q", newID)
	}
	moved, err := store.GetJob(t.Context(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if moved.State != "cancelled" || moved.SupersededBy != newID {
		t.Fatalf("moved job = %#v", moved)
	}
	if len(moved.Runs) != 1 || moved.Runs[0].State != "cancelled" || moved.Runs[0].Error != "moved to queue" {
		t.Fatalf("moved runs = %#v", moved.Runs)
	}
	next, err := store.GetJob(t.Context(), newID)
	if err != nil {
		t.Fatal(err)
	}
	if next.State != "queued" || next.Attempt != 2 || next.PreviousJobID != jobID {
		t.Fatalf("requeued job = %#v", next)
	}

	// The refusal still applies to a running workflow job with a chain link.
	steps := []config.WorkflowStep{{Command: testAgent("build", "{{task.spec}}")}}
	workflowID, err := store.CreateTaskJob(t.Context(), protocol.Task{Title: "MACH-70 workflow", Spec: "Build it."}, "other", "deliver", "", steps)
	if err != nil {
		t.Fatal(err)
	}
	worker := pollRequest("worker-b", []string{"codex"}, []string{"other"})
	worker.Workflows = true
	worker.Artifacts = true
	if _, err := store.Poll(t.Context(), worker); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateJobWithOptions(t.Context(), "follow-up", "other", "build", testAgent("build", "{{machinist.prompt}}"), CreateJobOptions{Title: "MACH-70 follow-up"}); err != nil {
		t.Fatal(err)
	}
	if err := store.linkTicketAttempts(t.Context()); err != nil {
		t.Fatal(err)
	}
	wjob, err := store.GetJob(t.Context(), workflowID)
	if err != nil {
		t.Fatal(err)
	}
	if wjob.State != "running" || wjob.SupersededBy == "" {
		t.Fatalf("workflow job = %#v", wjob)
	}
	if _, err := store.MoveToQueue(t.Context(), workflowID); !errors.Is(err, ErrMoveNotAvailable) {
		t.Fatalf("workflow job with chain link = %v, want ErrMoveNotAvailable", err)
	}
}

func TestMoveToQueueRefusesNonRunningAndWorkflowJobs(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))

	if _, err := store.MoveToQueue(t.Context(), "job_missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing job = %v, want sql.ErrNoRows", err)
	}

	// A queued job has no attempt to stop.
	queuedID, err := store.CreateJob(t.Context(), "Queued", "machinist", "build", testAgent("build", "{{machinist.prompt}}"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.MoveToQueue(t.Context(), queuedID); !errors.Is(err, ErrMoveNotAvailable) {
		t.Fatalf("queued job = %v, want ErrMoveNotAvailable", err)
	}

	// Workflow jobs have their own retry and cancel actions.
	steps := []config.WorkflowStep{{Command: testAgent("build", "{{task.spec}}")}}
	workflowID, err := store.CreateTaskJob(t.Context(), protocol.Task{Title: "Fix", SourceURL: "https://github.com/example/repo/issues/1", Spec: "Build it."}, "machinist", "deliver", "", steps)
	if err != nil {
		t.Fatal(err)
	}
	worker := pollRequest("worker-a", []string{"codex"}, []string{"machinist"})
	worker.Workflows = true
	worker.Artifacts = true
	if _, err := store.Poll(t.Context(), worker); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MoveToQueue(t.Context(), workflowID); !errors.Is(err, ErrMoveNotAvailable) {
		t.Fatalf("workflow job = %v, want ErrMoveNotAvailable", err)
	}

	// A failed job whose ticket has a newer submission carries a chain link
	// (superseded_by), not a completed move: still refused. Its own lane
	// keeps the claim below deterministic.
	failedID, err := store.CreateJobWithOptions(t.Context(), "first pass", "solo", "build", testAgent("build", "{{machinist.prompt}}"), CreateJobOptions{Title: "MACH-65 failed pass"})
	if err != nil {
		t.Fatal(err)
	}
	failedRun, err := store.Poll(t.Context(), pollRequest("worker-solo", []string{"codex"}, []string{"solo"}))
	if err != nil || failedRun == nil {
		t.Fatalf("poll = %#v, %v", failedRun, err)
	}
	if err := store.Complete(t.Context(), failedRun.ID, protocol.Completion{InstanceID: "worker-solo", LeaseToken: failedRun.LeaseToken, State: "failed", ExitCode: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateJobWithOptions(t.Context(), "retry pass", "solo", "build", testAgent("build", "{{machinist.prompt}}"), CreateJobOptions{Title: "MACH-65 failed pass again"}); err != nil {
		t.Fatal(err)
	}
	if err := store.linkTicketAttempts(t.Context()); err != nil {
		t.Fatal(err)
	}
	failed, err := store.GetJob(t.Context(), failedID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.State != "failed" || failed.SupersededBy == "" {
		t.Fatalf("failed job = %#v", failed)
	}
	if _, err := store.MoveToQueue(t.Context(), failedID); !errors.Is(err, ErrMoveNotAvailable) {
		t.Fatalf("failed job with chain link = %v, want ErrMoveNotAvailable", err)
	}

	// The same refusal covers a queued job with a chain link.
	var chainQueuedID string
	chainQueuedID, err = store.CreateJobWithOptions(t.Context(), "queued pass", "solo", "build", testAgent("build", "{{machinist.prompt}}"), CreateJobOptions{Title: "MACH-65 queued pass"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateJobWithOptions(t.Context(), "queued pass again", "solo", "build", testAgent("build", "{{machinist.prompt}}"), CreateJobOptions{Title: "MACH-65 queued pass again"}); err != nil {
		t.Fatal(err)
	}
	if err := store.linkTicketAttempts(t.Context()); err != nil {
		t.Fatal(err)
	}
	queuedLinked, err := store.GetJob(t.Context(), chainQueuedID)
	if err != nil {
		t.Fatal(err)
	}
	if queuedLinked.State != "queued" || queuedLinked.SupersededBy == "" {
		t.Fatalf("queued job = %#v", queuedLinked)
	}
	if _, err := store.MoveToQueue(t.Context(), chainQueuedID); !errors.Is(err, ErrMoveNotAvailable) {
		t.Fatalf("queued job with chain link = %v, want ErrMoveNotAvailable", err)
	}
}

func TestMoveToQueueHTTPRequiresWriteToken(t *testing.T) {
	server, webServer := newAuthTestServer(t, config.AuthTokens)
	defer webServer.Close()
	store := server.store

	jobID, _ := claimRunningJob(t, store, "worker-a")
	_, readSecret, err := store.CreateToken(t.Context(), "reader", TokenScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	_, writeSecret, err := store.CreateToken(t.Context(), "writer", TokenScopeWrite)
	if err != nil {
		t.Fatal(err)
	}

	request := func(token string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, webServer.URL+"/api/v1/jobs/"+jobID+"/move-to-queue", nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		server.Handler().ServeHTTP(recorder, req)
		return recorder
	}

	// Write endpoints fall back to the browser CSRF defense; a credential-less
	// non-browser caller is forbidden, and a bad credential is unauthorized.
	if code := request("").Code; code != http.StatusForbidden {
		t.Fatalf("no token status = %d, want 403", code)
	}
	if code := request("mmt_bogus").Code; code != http.StatusUnauthorized {
		t.Fatalf("bogus token status = %d, want 401", code)
	}
	if code := request(readSecret).Code; code != http.StatusForbidden {
		t.Fatalf("read token status = %d, want 403", code)
	}

	recorder := request(writeSecret)
	if recorder.Code != http.StatusOK {
		t.Fatalf("write token status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.JobID == "" || payload.JobID == jobID {
		t.Fatalf("response job id = %q", payload.JobID)
	}

	// The idempotent repeat returns the same attempt id.
	if again := request(writeSecret); again.Code != http.StatusOK {
		t.Fatalf("repeat status = %d", again.Code)
	}
	var repeat struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(request(writeSecret).Body.Bytes(), &repeat); err != nil {
		t.Fatal(err)
	}
	if repeat.JobID != payload.JobID {
		t.Fatalf("repeat returned %q, want %q", repeat.JobID, payload.JobID)
	}
}

func TestMoveToQueueMCPRequiresWriteToken(t *testing.T) {
	server, webServer := newAuthTestServer(t, config.AuthTokens)
	defer webServer.Close()
	store := server.store

	jobID, _ := claimRunningJob(t, store, "worker-a")
	_, readSecret, err := store.CreateToken(t.Context(), "reader", TokenScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	_, writeSecret, err := store.CreateToken(t.Context(), "writer", TokenScopeWrite)
	if err != nil {
		t.Fatal(err)
	}

	// A read token is rejected on the write tool.
	payload := mcpCall(t, webServer.URL+"/mcp", readSecret, "tools/call", map[string]any{
		"name": "move_to_queue", "arguments": map[string]any{"job_id": jobID},
	}, 1)
	if !strings.Contains(mcpText(payload), "write-scope token") {
		t.Fatalf("read-token move_to_queue = %v", payload)
	}

	// The write token moves the job and returns the new attempt id.
	payload = mcpCall(t, webServer.URL+"/mcp", writeSecret, "tools/call", map[string]any{
		"name": "move_to_queue", "arguments": map[string]any{"job_id": jobID},
	}, 2)
	text := mcpText(payload)
	if text == "" {
		t.Fatalf("write-token move_to_queue = %v", payload)
	}
	var result struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(text), &result); err != nil {
		t.Fatalf("move_to_queue result %q: %v", text, err)
	}
	if result.JobID == "" || result.JobID == jobID {
		t.Fatalf("move_to_queue returned %q", result.JobID)
	}

	// A repeated call returns the same attempt id, not a duplicate.
	text = mcpCallTool(t, webServer.URL+"/mcp", writeSecret, "move_to_queue", map[string]any{"job_id": jobID}, 3)
	var repeat struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(text), &repeat); err != nil {
		t.Fatalf("repeat result %q: %v", text, err)
	}
	if repeat.JobID != result.JobID {
		t.Fatalf("repeat returned %q, want %q", repeat.JobID, result.JobID)
	}
}

func TestMoveToQueueRecordsSavedWorkInProgress(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	jobID, run := claimRunningJob(t, store, "worker-a")

	if _, err := store.MoveToQueue(t.Context(), jobID); err != nil {
		t.Fatal(err)
	}

	// Only the worker that held the revoked lease may report.
	if err := store.RecordRevokedWIP(t.Context(), run.ID, protocol.LeaseRevoked{InstanceID: "worker-other", LeaseToken: run.LeaseToken, WIP: "somewhere"}); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("foreign worker ack = %v, want ErrLeaseConflict", err)
	}
	if err := store.RecordRevokedWIP(t.Context(), run.ID, protocol.LeaseRevoked{InstanceID: "worker-a", LeaseToken: "stale-token", WIP: "somewhere"}); !errors.Is(err, ErrLeaseConflict) {
		t.Fatalf("stale token ack = %v, want ErrLeaseConflict", err)
	}

	const wip = `stash "machinist-lane-wip-20261016-124559" in lane checkout /lanes/general-2; patch /data/wip/machinist-lane-wip-20261016-124559.patch`
	if err := store.RecordRevokedWIP(t.Context(), run.ID, protocol.LeaseRevoked{InstanceID: "worker-a", LeaseToken: run.LeaseToken, WIP: wip}); err != nil {
		t.Fatalf("record revoked wip: %v", err)
	}
	got, err := store.GetRun(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.WIP != wip {
		t.Fatalf("run wip = %q, want %q", got.WIP, wip)
	}
	if got.Error != "moved to queue" {
		t.Fatalf("run error = %q, want the move reason", got.Error)
	}
}
