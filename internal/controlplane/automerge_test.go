package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/issues"
)

// fakeAutoMergeGitHub drives the merge gate: one canned status and counters.
type fakeAutoMergeGitHub struct {
	status            PullRequestStatus
	mergeSHA          string
	mergeErr          error
	mergeCalls        int
	updateBranchCalls int
}

func (f *fakeAutoMergeGitHub) SearchRequestedIssues(context.Context, []string, string, int) ([]GitHubCandidate, error) {
	return nil, nil
}

func (f *fakeAutoMergeGitHub) IssueDetails(context.Context, string, int, string) (GitHubIssueDetails, error) {
	return GitHubIssueDetails{}, nil
}

func (f *fakeAutoMergeGitHub) Permission(context.Context, string, string) (string, error) {
	return "admin", nil
}

func (f *fakeAutoMergeGitHub) AcknowledgeRequest(context.Context, string, int, string, string, bool) error {
	return nil
}

func (f *fakeAutoMergeGitHub) MergedPullRequests(context.Context, string, int) ([]GitHubPullRequest, error) {
	return nil, nil
}

func (f *fakeAutoMergeGitHub) PullRequestStatus(context.Context, string, int) (PullRequestStatus, error) {
	return f.status, nil
}

func (f *fakeAutoMergeGitHub) UpdatePullRequestBranch(context.Context, string, int) error {
	f.updateBranchCalls++
	return nil
}

func (f *fakeAutoMergeGitHub) MergePullRequest(context.Context, string, int, string) (string, error) {
	f.mergeCalls++
	if f.mergeErr != nil {
		return "", f.mergeErr
	}
	return f.mergeSHA, nil
}

// recordingRunner captures shell commands instead of running them.
type recordingRunner struct {
	runs    []string
	results map[string]runnerResult
}

type runnerResult struct {
	output string
	err    error
}

func (r *recordingRunner) Run(_ context.Context, command string, _ time.Duration) (string, error) {
	r.runs = append(r.runs, command)
	if result, ok := r.results[command]; ok {
		return result.output, result.err
	}
	return "ok", nil
}

// newAutoMergeTestServer wires a server with fake GitHub, a recording shell
// runner and a fixture Plane client, plus one succeeded job for the ticket.
func newAutoMergeTestServer(t *testing.T) (*Server, *Store, *fakeAutoMergeGitHub, *recordingRunner, *[]planeRequest) {
	t.Helper()
	t.Setenv("MACHINIST_HOME", t.TempDir())
	server, store := newResolutionTestServer(t)
	github := &fakeAutoMergeGitHub{status: PullRequestStatus{
		Number: 7, State: "OPEN", Mergeable: "MERGEABLE", MergeState: "CLEAN", HeadRefOid: "head",
	}, mergeSHA: "abc123def456"}
	server.github = github
	runner := &recordingRunner{results: map[string]runnerResult{}}
	server.deployRunner = runner
	client, requests := newPlaneFixture(t)
	server.planeClients = map[string]*issues.PlaneClient{"": client}
	jobID, err := store.CreateJobWithOptions(context.Background(), "Fix the sidebar.", "sparkdash", "plan",
		config.ResolvedCommand{Name: "plan", Timeout: time.Minute}, CreateJobOptions{Title: "MACH-6 sidebar", IssueKey: "MACH-6"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET state='succeeded' WHERE id=?`, jobID); err != nil {
		t.Fatal(err)
	}
	entry := AutoMergeJob{
		JobID: jobID, RunID: "run_test", IssueKey: "MACH-6", ProjectID: "proj",
		Repo: "dan/sparkdash", PRURL: "https://github.com/dan/sparkdash/pull/7", PRNumber: 7,
	}
	if err := store.QueueAutoMerge(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	return server, store, github, runner, requests
}

func planeRequests(requests *[]planeRequest, method, needle string) []string {
	matched := []string{}
	for _, request := range *requests {
		if request.Method == method && strings.Contains(request.Body, needle) {
			matched = append(matched, request.Body)
		}
	}
	return matched
}

func autoMergeMetadata(t *testing.T, store *Store, jobID string) *AutoMergeRecord {
	t.Helper()
	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Metadata == nil {
		return nil
	}
	return job.Metadata.Automerge
}

func TestEvaluateChecks(t *testing.T) {
	ok, pending, failed := EvaluateChecks(nil)
	if !ok || pending || failed != "" {
		t.Fatalf("empty rollup: ok=%v pending=%v failed=%q", ok, pending, failed)
	}
	ok, pending, _ = EvaluateChecks([]PullRequestCheck{{Name: "build", Status: "IN_PROGRESS", Conclusion: ""}})
	if ok || !pending {
		t.Fatalf("in-progress rollup: ok=%v pending=%v", ok, pending)
	}
	ok, pending, failed = EvaluateChecks([]PullRequestCheck{{Name: "go test", Status: "COMPLETED", Conclusion: "FAILURE"}})
	if ok || pending || failed != "go test" {
		t.Fatalf("failed rollup: ok=%v pending=%v failed=%q", ok, pending, failed)
	}
	ok, _, _ = EvaluateChecks([]PullRequestCheck{
		{Name: "build", Status: "COMPLETED", Conclusion: "SUCCESS"},
		{Name: "lint", State: "SUCCESS"},
	})
	if !ok {
		t.Fatal("passing rollup not accepted")
	}
}

func TestAutoMergePolicyOffDropsEntry(t *testing.T) {
	server, store, github, runner, _ := newAutoMergeTestServer(t)
	// Unknown project: the queue entry is dropped, nothing else happens.
	server.planeIntake.testPolicies = map[string]config.MergePolicy{}
	if err := server.autoMergePass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if github.mergeCalls != 0 || len(runner.runs) != 0 {
		t.Fatalf("merged %d times, ran %d commands", github.mergeCalls, len(runner.runs))
	}
	entries, err := store.PendingAutoMergeJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("queue still holds %d entries", len(entries))
	}
}

func TestAutoMergeConflictDoesNotMerge(t *testing.T) {
	server, store, github, runner, requests := newAutoMergeTestServer(t)
	github.status.Mergeable = "CONFLICTING"
	github.status.MergeState = "DIRTY"

	if err := server.autoMergePass(context.Background()); err != nil {
		t.Fatal(err)
	}

	if github.mergeCalls != 0 {
		t.Fatalf("merged a conflicting PR %d times", github.mergeCalls)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("deploy ran %d commands", len(runner.runs))
	}
	if got := countRequests(requests, http.MethodPatch); got != 1 {
		t.Fatalf("expected one state patch, got %d", got)
	}
	if got := planeRequests(requests, "POST", "conflicts with the base"); len(got) != 1 {
		t.Fatalf("expected one failure comment, got %d", len(got))
	}
	record := autoMergeMetadata(t, store, jobIDOf(t, store))
	if record == nil || record.State != autoMergeFailed || !strings.Contains(record.Reason, "conflicts") {
		t.Fatalf("audit record = %+v", record)
	}
	entries, err := store.PendingAutoMergeJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("queue still holds %d entries", len(entries))
	}
	alerts, err := filepath.Glob(filepath.Join(os.Getenv("MACHINIST_HOME"), "alerts", "pending", "automerge-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("expected one alert file, found %v", alerts)
	}
}

func TestAutoMergeDeployFailureRollsBack(t *testing.T) {
	server, store, github, runner, requests := newAutoMergeTestServer(t)
	runner.results[config.DefaultMACHDeployCommand] = runnerResult{"", errors.New("systemctl restart failed")}

	if err := server.autoMergePass(context.Background()); err != nil {
		t.Fatal(err)
	}

	if github.mergeCalls != 1 {
		t.Fatalf("merge calls = %d, want 1", github.mergeCalls)
	}
	rolledBack := slices.ContainsFunc(runner.runs, func(command string) bool {
		return strings.Contains(command, "machinist.prev-before")
	})
	if !rolledBack {
		t.Fatalf("rollback never ran; commands: %v", runner.runs)
	}
	if got := countRequests(requests, http.MethodPatch); got != 1 {
		t.Fatalf("expected one state patch, got %d", got)
	}
	record := autoMergeMetadata(t, store, jobIDOf(t, store))
	if record == nil || !strings.Contains(record.Reason, "deploy failed") || !strings.Contains(record.Reason, "rolled back") {
		t.Fatalf("audit record = %+v", record)
	}
}

func TestAutoMergePendingChecksWait(t *testing.T) {
	server, store, github, runner, _ := newAutoMergeTestServer(t)
	github.status.Rollup = []PullRequestCheck{{Name: "build", Status: "IN_PROGRESS"}}

	if err := server.autoMergePass(context.Background()); err != nil {
		t.Fatal(err)
	}

	if github.mergeCalls != 0 || len(runner.runs) != 0 {
		t.Fatalf("acted while checks were pending: merged %d, ran %d", github.mergeCalls, len(runner.runs))
	}
	entries, err := store.PendingAutoMergeJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].State != autoMergeQueued {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestAutoMergeBehindBranchUpdatesThenRechecks(t *testing.T) {
	server, _, github, runner, _ := newAutoMergeTestServer(t)
	github.status.MergeState = "BEHIND"

	if err := server.autoMergePass(context.Background()); err != nil {
		t.Fatal(err)
	}

	if github.updateBranchCalls != 1 || github.mergeCalls != 0 {
		t.Fatalf("update-branch calls = %d, merge calls = %d", github.updateBranchCalls, github.mergeCalls)
	}
	if len(runner.runs) != 0 {
		t.Fatalf("deploy ran before checks: %v", runner.runs)
	}
}

func TestAutoMergeSuccessMovesTicketToDone(t *testing.T) {
	server, store, github, _, requests := newAutoMergeTestServer(t)
	// Override the resolved policy through the intake's test hook.
	server.planeIntake.testPolicies = map[string]config.MergePolicy{
		"MACH": {
			AutoMerge:     true,
			MergeMethod:   "squash",
			DeployTimeout: "1m",
		},
	}

	if err := server.autoMergePass(context.Background()); err != nil {
		t.Fatal(err)
	}

	if github.mergeCalls != 1 {
		t.Fatalf("merge calls = %d, want 1", github.mergeCalls)
	}
	record := autoMergeMetadata(t, store, jobIDOf(t, store))
	if record == nil || record.State != autoMergeDone || record.MergeSHA != "abc123def456" {
		t.Fatalf("audit record = %+v", record)
	}
	entries, err := store.PendingAutoMergeJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("queue still holds %d entries", len(entries))
	}
	if got := planeRequests(requests, "POST", "abc123def456"); len(got) == 0 {
		t.Fatal("no merge-SHA comment posted")
	}
	if got := countRequests(requests, http.MethodPatch); got != 1 {
		t.Fatalf("expected exactly one state patch (Done), got %d", got)
	}
	job, err := store.GetJob(context.Background(), jobIDOf(t, store))
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "resolved" {
		t.Fatalf("job state = %q, want resolved", job.State)
	}
}

func TestAutoMergeResumeAfterRestartVerifiesWithoutRedeploy(t *testing.T) {
	server, store, github, runner, _ := newAutoMergeTestServer(t)
	server.planeIntake.testPolicies = map[string]config.MergePolicy{"MACH": {AutoMerge: true}}
	entry := AutoMergeJob{
		JobID: jobIDOf(t, store), IssueKey: "MACH-6", Repo: "dan/sparkdash",
		PRURL: "https://github.com/dan/sparkdash/pull/7", PRNumber: 7,
		State: autoMergeDeploying, MergeSHA: "abc123def456", DeployRuns: 1,
	}
	if err := store.SaveAutoMergeState(context.Background(), entry); err != nil {
		t.Fatal(err)
	}

	if err := server.autoMergePass(context.Background()); err != nil {
		t.Fatal(err)
	}

	if github.mergeCalls != 0 {
		t.Fatalf("re-merged after restart: %d times", github.mergeCalls)
	}
	// The deploy runner only sees the health check, not a re-deploy.
	for _, command := range runner.runs {
		if strings.Contains(command, "deploy") {
			t.Fatalf("unexpected deploy command: %s", command)
		}
	}
	record := autoMergeMetadata(t, store, jobIDOf(t, store))
	if record == nil || record.State != autoMergeDone {
		t.Fatalf("audit record = %+v", record)
	}
}

func TestAutoMergeHealthURLCheck(t *testing.T) {
	server, store, _, _, _ := newAutoMergeTestServer(t)
	health := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer health.Close()
	server.planeIntake.testPolicies = map[string]config.MergePolicy{
		"MACH": {AutoMerge: true, HealthCheck: &config.MergeHealthCheck{URL: health.URL, Status: 200}},
	}
	entry := AutoMergeJob{
		JobID: jobIDOf(t, store), IssueKey: "MACH-6", Repo: "dan/sparkdash",
		PRURL: "https://github.com/dan/sparkdash/pull/7", PRNumber: 7,
		State: autoMergeVerifying, MergeSHA: "abc123def456",
	}
	if err := store.SaveAutoMergeState(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	for range autoMergeMaxVerify {
		if err := server.autoMergePass(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	record := autoMergeMetadata(t, store, jobIDOf(t, store))
	if record == nil || record.State != autoMergeFailed || !strings.Contains(record.Reason, "health check failed") {
		t.Fatalf("audit record = %+v", record)
	}
}

// countRequests counts recorded fixture requests by method.
func countRequests(requests *[]planeRequest, method string) int {
	count := 0
	for _, request := range *requests {
		if request.Method == method {
			count++
		}
	}
	return count
}

// jobIDOf returns the single job the auto-merge fixture created.
func jobIDOf(t *testing.T, store *Store) string {
	t.Helper()
	entries, err := store.db.Query(`SELECT job_id FROM automerge_jobs`)
	if err != nil {
		t.Fatal(err)
	}
	defer entries.Close()
	for entries.Next() {
		var jobID string
		if err := entries.Scan(&jobID); err != nil {
			t.Fatal(err)
		}
		return jobID
	}
	// Entry already processed: fall back to the single succeeded job.
	snapshot, err := store.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range snapshot.Jobs {
		if job.IssueKey == "MACH-6" {
			return job.ID
		}
	}
	t.Fatal("no MACH-6 job")
	return ""
}

// The remaining helpers patch the plane intake so tests can pin policies
// without a config file per case.
func TestQueueAutoMergeResolvesRepoAndNumber(t *testing.T) {
	t.Setenv("MACHINIST_SPARK_HOST", "spark-host.example.test")
	server, store, _, _, _ := newAutoMergeTestServer(t)
	server.githubRepositories = map[string]string{"sparkdash": "dan/sparkdash"}
	jobID, err := store.CreateJobWithOptions(context.Background(), "Spark ticket.", "sparkdash", "plan",
		config.ResolvedCommand{Name: "plan", Timeout: time.Minute}, CreateJobOptions{Title: "SPARK-2 widget", IssueKey: "SPARK-2"})
	if err != nil {
		t.Fatal(err)
	}
	candidate := PlaneSyncCandidate{JobID: jobID, RunID: "run_x", IssueKey: "SPARK-2", ProjectID: "proj"}
	if err := server.queueAutoMerge(context.Background(), candidate, "https://github.com/dan/sparkdash/pull/9"); err != nil {
		t.Fatal(err)
	}
	entries, err := store.PendingAutoMergeJobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, entry := range entries {
		if entry.JobID == jobID && entry.PRNumber == 9 && entry.Repo == "dan/sparkdash" {
			found = true
		}
	}
	if !found {
		t.Fatalf("entry not queued: %+v", entries)
	}
}

func TestSaveAutoMergeMetadataRoundTrip(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	jobID, err := store.CreateJobWithOptions(context.Background(), "Fix.", "sparkdash", "plan",
		config.ResolvedCommand{Name: "plan"}, CreateJobOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveAutoMergeMetadata(context.Background(), jobID, AutoMergeRecord{
		PR: "https://github.com/dan/sparkdash/pull/7", PRNumber: 7, MergeSHA: "abc",
		State: autoMergeDone, HealthResult: "HTTP 200", UpdatedAt: "now",
	}); err != nil {
		t.Fatal(err)
	}
	record := autoMergeMetadata(t, store, jobID)
	if record == nil || record.MergeSHA != "abc" || record.HealthResult != "HTTP 200" {
		t.Fatalf("record = %+v", record)
	}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"merge_sha":"abc"`) {
		t.Fatalf("json = %s", raw)
	}
}
