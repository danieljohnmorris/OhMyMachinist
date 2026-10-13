package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/issues"
)

type planeRequest struct {
	Method string
	Path   string
	Body   string
}

// newPlaneFixture starts a fake Plane API and returns a client plus the
// requests it received.
func newPlaneFixture(t *testing.T) (*issues.PlaneClient, *[]planeRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []planeRequest
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		requests = append(requests, planeRequest{Method: request.Method, Path: request.URL.Path, Body: string(body)})
		mu.Unlock()
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/issues/SPARK-10/") {
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"id":"SPARK-10","name":"Hand finished","state":{"name":"Done","group":"completed"}}`))
			return
		}
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/issues/MACH-6/") {
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"id":"MACH-6","name":"Still open","state":{"name":"In Progress","group":"started"}}`))
			return
		}
		if request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/issues/") {
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(`{"id":"any","name":"Done elsewhere","state":{"name":"Done","group":"completed"}}`))
			return
		}
		response.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	client, err := issues.NewPlaneClient(issues.PlaneConfig{URL: server.URL, Workspace: "platform", ProjectID: "proj", APIKey: "key"})
	if err != nil {
		t.Fatal(err)
	}
	return client, &requests
}

func newResolutionTestServer(t *testing.T) (*Server, *Store) {
	t.Helper()
	directory := t.TempDir()
	promptPath := filepath.Join(directory, "plan.md")
	if err := os.WriteFile(promptPath, []byte("Plan {{machinist.prompt}}.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	definitionPath := filepath.Join(directory, "config.toml")
	definition := "[commands.plan]\nexecutor = \"test\"\nprompt_file = \"plan.md\"\ntimeout = \"1m\"\n\n[github.repositories]\nsparkdash = \"dan/sparkdash\"\n"
	if err := os.WriteFile(definitionPath, []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, filepath.Join(directory, "machinist.db"))
	server, err := NewServer(store, definitionPath, "secret", 0)
	if err != nil {
		t.Fatal(err)
	}
	return server, store
}

// seedJob creates a finished job the way older Machinist versions stored them:
// no issue_key column, just a title and trigger subject.
func seedJob(t *testing.T, store *Store, title, subject, state string) string {
	t.Helper()
	jobID, err := store.CreateJobWithOptions(t.Context(), "Fix the sidebar.", "sparkdash", "plan", config.ResolvedCommand{Name: "plan"}, CreateJobOptions{Title: title})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET state=?, trigger_subject=? WHERE id=?`, state, subject, jobID); err != nil {
		t.Fatal(err)
	}
	return jobID
}

func jobByID(t *testing.T, store *Store, jobID string) Job {
	t.Helper()
	snapshot, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range snapshot.Jobs {
		if job.ID == jobID {
			return job
		}
	}
	t.Fatalf("job %s not found", jobID)
	return Job{}
}

// constantGitHubCLI answers every merged-PR search with the same list.
type constantGitHubCLI struct {
	pullRequests []GitHubPullRequest
	err          error
}

func (c *constantGitHubCLI) SearchRequestedIssues(context.Context, []string, string, int) ([]GitHubCandidate, error) {
	return nil, nil
}

func (c *constantGitHubCLI) IssueDetails(context.Context, string, int, string) (GitHubIssueDetails, error) {
	return GitHubIssueDetails{}, nil
}

func (c *constantGitHubCLI) Permission(context.Context, string, string) (string, error) {
	return "admin", nil
}

func (c *constantGitHubCLI) AcknowledgeRequest(context.Context, string, int, string, string, bool) error {
	return nil
}

func (c *constantGitHubCLI) MergedPullRequests(context.Context, string, int) ([]GitHubPullRequest, error) {
	return c.pullRequests, c.err
}

func TestTicketKeyExtraction(t *testing.T) {
	cases := []struct {
		name string
		job  Job
		want string
	}{
		{"stored key wins", Job{IssueKey: "MACH-5", Title: "SPARK-10: other"}, "MACH-5"},
		{"trigger subject", Job{TriggerSubject: "https://plane.example/projects/p/issues/MACH-5/"}, "MACH-5"},
		{"title", Job{Title: "MACH-5: Fix sidebar"}, "MACH-5"},
		{"github issue title", Job{GitHubIssueTitle: "MACH-8: Something"}, "MACH-8"},
		{"no key", Job{Title: "No ticket here"}, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ticketKey(testCase.job); got != testCase.want {
				t.Fatalf("ticketKey() = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestPullRequestMatchesTicket(t *testing.T) {
	mach5 := "MACH-5"
	if !pullRequestMatchesTicket(GitHubPullRequest{Title: "MACH-5: Fix sidebar", HeadRefName: "main"}, mach5, "") {
		t.Fatal("title match failed")
	}
	if !pullRequestMatchesTicket(GitHubPullRequest{Title: "Fix sidebar", HeadRefName: "factory/mach-5-fix-sidebar"}, mach5, "") {
		t.Fatal("branch match failed")
	}
	if !pullRequestMatchesTicket(GitHubPullRequest{Title: "Fix sidebar", Body: "Closes https://plane.example/projects/p/issues/MACH-5/"}, mach5, "https://plane.example/projects/p/issues/MACH-5/") {
		t.Fatal("plane link match failed")
	}
	if pullRequestMatchesTicket(GitHubPullRequest{Title: "MACH-51: unrelated", HeadRefName: "mach-51"}, mach5, "") {
		t.Fatal("longer ticket key must not match")
	}
	if pullRequestMatchesTicket(GitHubPullRequest{Title: "unrelated"}, mach5, "") {
		t.Fatal("unrelated PR matched")
	}
	if !pullRequestMatchesTicket(GitHubPullRequest{Body: "close https://plane.example/projects/p/issues/MACH-5#top"}, mach5, "https://plane.example/projects/p/issues/MACH-5/") {
		t.Fatal("plane link with fragment failed")
	}
	if pullRequestMatchesTicket(GitHubPullRequest{Body: "close https://plane.example/projects/p/issues/MACH-5x"}, mach5, "https://plane.example/projects/p/issues/MACH-5/") {
		t.Fatal("longer plane issue path matched")
	}
}

func TestReconcileResolvesMergedPullRequestAndMovesPlaneTicket(t *testing.T) {
	server, store := newResolutionTestServer(t)
	jobID := seedJob(t, store, "MACH-5: Fix sidebar", "", "failed")
	planeClient, requests := newPlaneFixture(t)
	cli, _ := newScriptedGitHubCLI(scriptedGitHubResult{stdout: `[
		{"number": 12, "title": "MACH-5: Fix sidebar", "headRefName": "mach-5-sidebar", "url": "https://github.com/dan/sparkdash/pull/12", "mergedBy": {"login": "kim"}},
		{"number": 10, "title": "MACH-5: one", "headRefName": "mach-5-one", "url": "https://github.com/dan/sparkdash/pull/10", "mergedBy": {"login": "dan"}}
	]`})
	server.github = cli
	server.planeClients = map[string]*issues.PlaneClient{"issue/sparkdash": planeClient}

	if err := server.reconcileTicketOutcomes(t.Context()); err != nil {
		t.Fatal(err)
	}

	job := jobByID(t, store, jobID)
	if job.State != "resolved" {
		t.Fatalf("state = %q, want resolved", job.State)
	}
	if job.ResolutionKind != "pr" {
		t.Fatalf("resolution kind = %q, want pr", job.ResolutionKind)
	}
	if job.ResolutionDetail != "PR #12 (merged by kim)" {
		t.Fatalf("resolution detail = %q, want the latest merged PR", job.ResolutionDetail)
	}
	if job.ResolvedAt == nil {
		t.Fatal("resolved at missing")
	}

	var patch, comment *planeRequest
	for _, request := range *requests {
		switch {
		case request.Method == http.MethodPatch && strings.HasSuffix(request.Path, "/issues/MACH-5/"):
			patch = &request
		case request.Method == http.MethodPost && strings.HasSuffix(request.Path, "/issues/MACH-5/comments/"):
			comment = &request
		}
	}
	if patch == nil || !strings.Contains(patch.Body, `"state":"Done"`) {
		t.Fatalf("plane state patch missing: %+v", patch)
	}
	if comment == nil || !strings.Contains(comment.Body, "PR #12") || !strings.Contains(comment.Body, "@kim") {
		t.Fatalf("plane comment missing: %+v", comment)
	}
}

func TestReconcileBackfillsCardsFromPlaneState(t *testing.T) {
	server, store := newResolutionTestServer(t)
	// Legacy cards: no stored issue key, only titles and trigger subjects.
	backfilled := []string{
		seedJob(t, store, "SPARK-10: Hand finished", "https://plane.example/projects/p/issues/SPARK-10/", "failed"),
		seedJob(t, store, "MACH-5: Fix sidebar", "", "failed"),
		seedJob(t, store, "MACH-8: Merge window", "", "cancelled"),
	}
	stillOpen := seedJob(t, store, "MACH-6: Not done", "", "failed")
	noTicket := seedJob(t, store, "Local tidy up", "", "failed")
	planeClient, _ := newPlaneFixture(t)
	server.github = &constantGitHubCLI{pullRequests: []GitHubPullRequest{}}
	server.planeClients = map[string]*issues.PlaneClient{"issue/sparkdash": planeClient}

	if err := server.reconcileTicketOutcomes(t.Context()); err != nil {
		t.Fatal(err)
	}

	for _, jobID := range backfilled {
		job := jobByID(t, store, jobID)
		if job.State != "resolved" || job.ResolutionKind != "plane" || job.ResolutionDetail != "Plane Done" {
			t.Fatalf("job %s = %s/%s/%s, want resolved/plane/Plane Done", jobID, job.State, job.ResolutionKind, job.ResolutionDetail)
		}
	}
	open := jobByID(t, store, stillOpen)
	if open.State != "failed" {
		t.Fatalf("open ticket job state = %q, want failed", open.State)
	}
	local := jobByID(t, store, noTicket)
	if local.State != "failed" {
		t.Fatalf("unticketed job state = %q, want failed", local.State)
	}
}

func TestReconcileSkipsActiveAndResolvedJobs(t *testing.T) {
	server, store := newResolutionTestServer(t)
	running := seedJob(t, store, "MACH-5: Fix sidebar", "", "running")
	resolved := seedJob(t, store, "MACH-8: Merge window", "", "failed")
	if _, err := store.db.Exec(`UPDATE jobs SET state='resolved', resolution_kind='plane', resolution_detail='Plane Done' WHERE id=?`, resolved); err != nil {
		t.Fatal(err)
	}
	planeClient, requests := newPlaneFixture(t)
	server.planeClients = map[string]*issues.PlaneClient{"issue/sparkdash": planeClient}
	server.github = nil

	if err := server.reconcileTicketOutcomes(t.Context()); err != nil {
		t.Fatal(err)
	}

	if job := jobByID(t, store, running); job.State != "running" {
		t.Fatalf("running job state = %q, want running", job.State)
	}
	if job := jobByID(t, store, resolved); job.ResolutionKind != "plane" || job.ResolutionDetail != "Plane Done" {
		t.Fatalf("resolved job changed: %+v", job)
	}
	if len(*requests) != 0 {
		t.Fatalf("unexpected plane requests: %+v", *requests)
	}
}

func TestIssueWebhookResolvesCompletedTickets(t *testing.T) {
	server, store := newResolutionTestServer(t)
	server.webhookSecret = "hook-secret"
	resolved := seedJob(t, store, "MACH-5: Fix sidebar", "", "failed")
	other := seedJob(t, store, "SPARK-10: Hand finished", "", "failed")

	webServer := httptest.NewServer(server.Handler())
	defer webServer.Close()

	post := func(t *testing.T, payload []byte, signature string, wantStatus int) *http.Response {
		t.Helper()
		request, err := http.NewRequest(http.MethodPost, webServer.URL+"/api/v1/webhooks/issues", strings.NewReader(string(payload)))
		if err != nil {
			t.Fatal(err)
		}
		if signature != "" {
			request.Header.Set("X-Signature", signature)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != wantStatus {
			body, _ := io.ReadAll(response.Body)
			t.Fatalf("webhook status = %d, want %d: %s", response.StatusCode, wantStatus, body)
		}
		return response
	}

	payload := []byte(`{"issue":{"identifier":"MACH-5","title":"Fix sidebar","state":{"name":"Done","group":"completed"}}}`)
	post(t, payload, "", http.StatusUnauthorized)
	mac := hmac.New(sha256.New, []byte("hook-secret"))
	mac.Write(payload)
	post(t, payload, hex.EncodeToString(mac.Sum(nil)), http.StatusAccepted)
	job := jobByID(t, store, resolved)
	if job.State != "resolved" || job.ResolutionKind != "plane" || job.ResolutionDetail != "Plane Done" {
		t.Fatalf("job = %s/%s/%s, want resolved/plane/Plane Done", job.State, job.ResolutionKind, job.ResolutionDetail)
	}
	if jobByID(t, store, other).State != "failed" {
		t.Fatal("unrelated ticket was resolved")
	}

	// A non-completed state must not resolve anything.
	openPayload := []byte(`{"issue":{"identifier":"SPARK-10","title":"Hand finished","state":{"name":"In Progress","group":"started"}}}`)
	openSignature := "sha256=" + func() string {
		mac := hmac.New(sha256.New, []byte("hook-secret"))
		mac.Write(openPayload)
		return hex.EncodeToString(mac.Sum(nil))
	}()
	post(t, openPayload, openSignature, http.StatusAccepted)
	if jobByID(t, store, other).State != "failed" {
		t.Fatal("open ticket was resolved")
	}
}

func TestResolveTicketsForIssueMatchesStoredKeyTitleAndSubject(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	byKey := seedJob(t, store, "Unrelated title", "", "failed")
	if _, err := store.db.Exec(`UPDATE jobs SET issue_key='MACH-5' WHERE id=?`, byKey); err != nil {
		t.Fatal(err)
	}
	byTitle := seedJob(t, store, "MACH-6: Fix sidebar", "", "failed")
	bySubject := seedJob(t, store, "Unrelated", "https://plane.example/projects/p/issues/MACH-8/", "failed")
	running := seedJob(t, store, "MACH-9: In flight", "", "running")
	alreadyResolved := seedJob(t, store, "MACH-5: duplicate", "", "failed")
	if _, err := store.db.Exec(`UPDATE jobs SET state='resolved', resolution_kind='pr' WHERE id=?`, alreadyResolved); err != nil {
		t.Fatal(err)
	}

	count, err := store.ResolveTicketsForIssue(t.Context(), "MACH-5", "plane", "Plane Done", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("resolved count = %d, want 1", count)
	}
	if job := jobByID(t, store, byKey); job.State != "resolved" || job.ResolutionDetail != "Plane Done" {
		t.Fatalf("stored key job = %+v", job)
	}

	count, err = store.ResolveTicketsForIssue(t.Context(), "MACH-6", "pr", "PR #2 (merged by dan)", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("resolved count = %d, want 1", count)
	}
	if job := jobByID(t, store, byTitle); job.State != "resolved" || job.ResolutionKind != "pr" {
		t.Fatalf("title job = %+v", job)
	}

	count, err = store.ResolveTicketsForIssue(t.Context(), "MACH-8", "plane", "Plane Done", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("resolved count = %d, want 1", count)
	}
	if job := jobByID(t, store, bySubject); job.State != "resolved" || job.ResolutionKind != "plane" {
		t.Fatalf("subject job = %+v", job)
	}

	// Active and already-resolved jobs stay untouched on later passes.
	if _, err := store.ResolveTicketsForIssue(t.Context(), "MACH-9", "plane", "Plane Done", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if job := jobByID(t, store, running); job.State != "running" {
		t.Fatalf("running job state = %q, want running", job.State)
	}
	if job := jobByID(t, store, alreadyResolved); job.ResolutionKind != "pr" {
		t.Fatalf("resolved job overwritten: %+v", job)
	}

	if _, err := store.ResolveTicketsForIssue(t.Context(), "", "plane", "Plane Done", time.Now().UTC()); err == nil {
		t.Fatal("empty issue key accepted")
	}
	if _, err := store.ResolveTicketsForIssue(t.Context(), "MACH-5", "email", "x", time.Now().UTC()); err == nil {
		t.Fatal("unknown resolution kind accepted")
	}
}

func TestMarkJobResolvedRequiresResolvableState(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	jobID := seedJob(t, store, "MACH-5: Fix sidebar", "", "succeeded")
	if err := store.MarkJobResolved(t.Context(), jobID, "pr", "PR #7 (merged by dan)", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if job := jobByID(t, store, jobID); job.State != "resolved" || job.ResolutionKind != "pr" || job.ResolvedAt == nil {
		t.Fatalf("job = %+v", job)
	}
	if err := store.MarkJobResolved(t.Context(), jobID, "plane", "Plane Done", time.Now().UTC()); err == nil {
		t.Fatal("re-resolving a resolved job must fail")
	}
	queued := seedJob(t, store, "MACH-6: queued", "", "queued")
	if err := store.MarkJobResolved(t.Context(), queued, "plane", "Plane Done", time.Now().UTC()); err == nil {
		t.Fatal("resolving a queued job must fail")
	}
}
