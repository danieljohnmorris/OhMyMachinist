package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
		// Emulates real Plane: keys resolve only through the workspace-level
		// endpoint, issues carry a state UUID, and states live per project.
		const doneID, startedID, attentionID = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222", "33333333-3333-3333-3333-333333333333"
		path := request.URL.Path
		write := func(body string) {
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(body))
		}
		if request.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/workspaces/platform/issues/") {
			key := strings.TrimSuffix(strings.TrimPrefix(path, "/api/v1/workspaces/platform/issues/"), "/")
			state := doneID
			if key == "MACH-6" {
				state = startedID
			}
			write(`{"id":"uuid-` + key + `","project":"proj","state":"` + state + `"}`)
			return
		}
		if request.Method == http.MethodGet && path == "/api/v1/workspaces/platform/projects/proj/states/" {
			write(`{"results":[{"id":"` + startedID + `","name":"In Progress","group":"started"},{"id":"` + doneID + `","name":"Done","group":"completed"},{"id":"` + attentionID + `","name":"Needs attention","group":"started"}]}`)
			return
		}
		if request.Method == http.MethodGet && strings.HasSuffix(path, "/states/"+attentionID+"/") {
			write(`{"id":"` + attentionID + `","name":"Needs attention","group":"started"}`)
			return
		}
		if request.Method == http.MethodGet && strings.HasSuffix(path, "/states/"+doneID+"/") {
			write(`{"id":"` + doneID + `","name":"Done","group":"completed"}`)
			return
		}
		if request.Method == http.MethodGet && strings.HasSuffix(path, "/states/"+startedID+"/") {
			write(`{"id":"` + startedID + `","name":"In Progress","group":"started"}`)
			return
		}
		if request.Method == http.MethodGet && strings.Contains(path, "/projects/proj/issues/") {
			response.WriteHeader(http.StatusNotFound)
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
	server, err := NewServer(store, definitionPath, "secret", 0, config.AuthTokens)
	if err != nil {
		t.Fatal(err)
	}
	server.planePause = 0
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

func (c *constantGitHubCLI) PullRequestStatus(context.Context, string, int) (PullRequestStatus, error) {
	return PullRequestStatus{}, nil
}

func (c *constantGitHubCLI) UpdatePullRequestBranch(context.Context, string, int) error {
	return nil
}

func (c *constantGitHubCLI) MergePullRequest(context.Context, string, int, string) (string, error) {
	return "", nil
}

func TestTicketKeyExtraction(t *testing.T) {
	cases := []struct {
		name string
		job  Job
		want string
	}{
		{"stored ticket identity wins", Job{Metadata: &JobMetadata{Tracker: "builtin", TicketKey: "MM-3"}, IssueKey: "MACH-5", Title: "SPARK-10: other"}, "MM-3"},
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
	jobID := seedJob(t, store, "MACH-6: Fix sidebar", "", "failed")
	planeClient, requests := newPlaneFixture(t)
	cli, _ := newScriptedGitHubCLI(scriptedGitHubResult{stdout: `[
		{"number": 12, "title": "MACH-6: Fix sidebar", "headRefName": "mach-6-sidebar", "url": "https://github.com/dan/sparkdash/pull/12", "mergedBy": {"login": "kim"}},
		{"number": 10, "title": "MACH-6: one", "headRefName": "mach-6-one", "url": "https://github.com/dan/sparkdash/pull/10", "mergedBy": {"login": "dan"}}
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
		case request.Method == http.MethodPatch && strings.HasSuffix(request.Path, "/projects/proj/issues/uuid-MACH-6/"):
			patch = &request
		case request.Method == http.MethodPost && strings.HasSuffix(request.Path, "/projects/proj/issues/uuid-MACH-6/comments/"):
			comment = &request
		}
	}
	if patch == nil || !strings.Contains(patch.Body, `"state":"11111111-1111-1111-1111-111111111111"`) {
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

// linearRequest is one GraphQL call received by the fake Linear API.
type linearRequest struct {
	Query     string
	Variables map[string]any
}

// newLinearFixture starts a fake Linear GraphQL API. states maps the ticket
// keys this workspace knows to their workflow state type; every other key is
// "Entity not found", as Linear reports issues of other workspaces.
func newLinearFixture(t *testing.T, states map[string]string) (*issues.LinearClient, *[]linearRequest) {
	t.Helper()
	var mu sync.Mutex
	var requests []linearRequest
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var payload linearRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, payload)
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		write := func(body string) { _, _ = response.Write([]byte(body)) }
		switch {
		case strings.Contains(payload.Query, "commentCreate"):
			write(`{"data":{"commentCreate":{"success":true}}}`)
			return
		case strings.Contains(payload.Query, "issueUpdate"):
			write(`{"data":{"issueUpdate":{"success":true}}}`)
			return
		}
		key, _ := payload.Variables["key"].(string)
		stateType, known := states[key]
		if !known {
			write(`{"data":{"issue":null},"errors":[{"message":"Entity not found: Issue"}]}`)
			return
		}
		if strings.Contains(payload.Query, "states(") {
			write(`{"data":{"issue":{"id":"uuid-` + key + `","team":{"states":{"nodes":[{"id":"state-started","name":"In Progress","type":"started"},{"id":"state-done","name":"Done","type":"completed"}]}}}}}`)
			return
		}
		name := "In Progress"
		if stateType == "completed" {
			name = "Done"
		}
		write(`{"data":{"issue":{"id":"uuid-` + key + `","identifier":"` + key + `","title":"Ticket","description":"","url":"https://linear.app/team/issue/` + key + `","labels":{"nodes":[]},"team":{"id":"team"},"state":{"name":"` + name + `","type":"` + stateType + `"}}}}`)
	}))
	t.Cleanup(server.Close)
	client, err := issues.NewLinearClient(issues.LinearConfig{APIKey: "key", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	return client, &requests
}

// seedTrackedJob creates a finished job carrying stored ticket identity, as
// issue triggers and builtin approval now record it.
func seedTrackedJob(t *testing.T, store *Store, tracker, key, trigger, state string) string {
	t.Helper()
	jobID := seedJob(t, store, key+": Tracked work", "", state)
	if _, err := store.db.Exec(`UPDATE jobs SET trigger_identity=?, issue_key=? WHERE id=?`, trigger, key, jobID); err != nil {
		t.Fatal(err)
	}
	metadata := `{"tracker":"` + tracker + `","ticket_key":"` + key + `"}`
	if _, err := store.db.Exec(`INSERT INTO job_metadata(job_id,metadata) VALUES(?,?) ON CONFLICT(job_id) DO UPDATE SET metadata=excluded.metadata`, jobID, metadata); err != nil {
		t.Fatal(err)
	}
	return jobID
}

func mergedPullRequest(number int, title, url, login string) GitHubPullRequest {
	pullRequest := GitHubPullRequest{Number: number, Title: title, HeadRefName: "main", URL: url}
	pullRequest.MergedBy.Login = login
	return pullRequest
}

func signWebhook(secret string, payload []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	return hex.EncodeToString(mac.Sum(nil))
}

// postIssueWebhook delivers payload to the issue webhook with one signature
// header and asserts the response status.
func postIssueWebhook(t *testing.T, server *Server, header, signature string, payload []byte, wantStatus int) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/issues", strings.NewReader(string(payload)))
	if signature != "" {
		request.Header.Set(header, signature)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != wantStatus {
		t.Fatalf("webhook status = %d, want %d: %s", recorder.Code, wantStatus, recorder.Body.String())
	}
}

func TestIssueWebhookResolvesCompletedTickets(t *testing.T) {
	server, store := newResolutionTestServer(t)
	server.webhookSecret = "hook-secret"
	planeClient, _ := newPlaneFixture(t)
	server.planeClients = map[string]*issues.PlaneClient{"issue/sparkdash": planeClient}
	resolved := seedJob(t, store, "MACH-5: Fix sidebar", "", "failed")
	longer := seedJob(t, store, "MACH-51: Longer key", "", "failed")
	builtin := seedTrackedJob(t, store, trackerBuiltin, "MACH-5", "", "failed")
	other := seedJob(t, store, "SPARK-10: Hand finished", "", "failed")

	payload := []byte(`{"issue":{"identifier":"MACH-5","title":"Fix sidebar","state":{"name":"Done","group":"completed"}}}`)
	postIssueWebhook(t, server, "X-Signature", "", payload, http.StatusUnauthorized)
	postIssueWebhook(t, server, "X-Signature", signWebhook("wrong-secret", payload), payload, http.StatusUnauthorized)
	postIssueWebhook(t, server, "X-Signature", signWebhook("hook-secret", payload), payload, http.StatusAccepted)
	job := jobByID(t, store, resolved)
	if job.State != "resolved" || job.ResolutionKind != "plane" || job.ResolutionDetail != "Plane Done" {
		t.Fatalf("job = %s/%s/%s, want resolved/plane/Plane Done", job.State, job.ResolutionKind, job.ResolutionDetail)
	}
	if jobByID(t, store, longer).State != "failed" {
		t.Fatal("Plane MACH-5 completion resolved MACH-51")
	}
	if jobByID(t, store, builtin).State != "failed" {
		t.Fatal("Plane MACH-5 completion resolved the builtin MACH-5")
	}
	if jobByID(t, store, other).State != "failed" {
		t.Fatal("unrelated ticket was resolved")
	}

	// A non-completed state must not resolve anything.
	openPayload := []byte(`{"issue":{"identifier":"SPARK-10","title":"Hand finished","state":{"name":"In Progress","group":"started"}}}`)
	postIssueWebhook(t, server, "X-Signature", "sha256="+signWebhook("hook-secret", openPayload), openPayload, http.StatusAccepted)
	if jobByID(t, store, other).State != "failed" {
		t.Fatal("open ticket was resolved")
	}

	// Real Plane deliveries announce a state move as a state-only activity;
	// intake treats those as no-ops but completion must still resolve.
	statePayload := []byte(`{"event":"issue","action":"updated","data":{"identifier":"SPARK-10","name":"Hand finished","state":{"name":"Done","group":"completed"}},"activity":{"field":"state","old_value":"In Progress","new_value":"Done"}}`)
	postIssueWebhook(t, server, "X-Signature", signWebhook("hook-secret", statePayload), statePayload, http.StatusAccepted)
	if job := jobByID(t, store, other); job.State != "resolved" || job.ResolutionKind != "plane" {
		t.Fatalf("state-only completion = %s/%s, want resolved/plane", job.State, job.ResolutionKind)
	}
}

func TestIssueWebhookLinearCompletionRequiresSignatureFreshnessAndSource(t *testing.T) {
	server, store := newResolutionTestServer(t)
	server.webhookSecret = "hook-secret"
	// Two Linear workspaces both have a MACH-5; only workspace A completed it.
	clientA, _ := newLinearFixture(t, map[string]string{"MACH-5": "completed"})
	clientB, _ := newLinearFixture(t, map[string]string{"MACH-5": "started"})
	// Workspace C does not know MACH-5 at all.
	clientC, _ := newLinearFixture(t, map[string]string{})
	server.issueSources = map[string]issuePoller{
		"issue/linear-a": linearPoller{client: clientA},
		"issue/linear-b": linearPoller{client: clientB},
		"issue/linear-c": linearPoller{client: clientC},
	}
	planeClient, planeRequests := newPlaneFixture(t)
	server.planeClients = map[string]*issues.PlaneClient{"issue/sparkdash": planeClient}
	sourceA := seedTrackedJob(t, store, trackerLinear, "MACH-5", "issue/linear-a", "failed")
	sourceB := seedTrackedJob(t, store, trackerLinear, "MACH-5", "issue/linear-b", "succeeded")
	sourceC := seedTrackedJob(t, store, trackerLinear, "MACH-5", "issue/linear-c", "failed")
	plane := seedJob(t, store, "MACH-5: Plane twin", "", "failed")
	builtin := seedTrackedJob(t, store, trackerBuiltin, "MACH-5", "", "failed")

	payloadAt := func(at time.Time) []byte {
		return []byte(`{"action":"update","type":"Issue","data":{"id":"uuid-MACH-5","identifier":"MACH-5","title":"Ticket","state":{"id":"state-done","name":"Done","type":"completed"}},"webhookTimestamp":` + strconv.FormatInt(at.UnixMilli(), 10) + `}`)
	}
	fresh := payloadAt(time.Now())
	postIssueWebhook(t, server, "Linear-Signature", signWebhook("wrong-secret", fresh), fresh, http.StatusUnauthorized)
	stale := payloadAt(time.Now().Add(-10 * time.Minute))
	postIssueWebhook(t, server, "Linear-Signature", signWebhook("hook-secret", stale), stale, http.StatusUnauthorized)
	if jobByID(t, store, sourceA).State != "failed" {
		t.Fatal("rejected delivery resolved a job")
	}

	postIssueWebhook(t, server, "Linear-Signature", signWebhook("hook-secret", fresh), fresh, http.StatusAccepted)
	if job := jobByID(t, store, sourceA); job.State != "resolved" || job.ResolutionKind != "linear" || job.ResolutionDetail != "Linear Done" {
		t.Fatalf("source A job = %s/%s/%s, want resolved/linear/Linear Done", job.State, job.ResolutionKind, job.ResolutionDetail)
	}
	if jobByID(t, store, sourceB).State != "succeeded" {
		t.Fatal("MACH-5 completed in workspace A resolved workspace B's open MACH-5")
	}
	if jobByID(t, store, sourceC).State != "failed" {
		t.Fatal("MACH-5 completed in workspace A resolved a workspace that does not know MACH-5")
	}
	if jobByID(t, store, plane).State != "failed" || jobByID(t, store, builtin).State != "failed" {
		t.Fatal("Linear completion resolved a Plane or builtin MACH-5")
	}
	if len(*planeRequests) != 0 {
		t.Fatalf("Linear completion queried Plane: %+v", *planeRequests)
	}
}

func TestReconcileResolvesLinearTicketsThroughTheirOwnSource(t *testing.T) {
	server, store := newResolutionTestServer(t)
	linearClient, linearRequests := newLinearFixture(t, map[string]string{"MACH-7": "started", "MACH-8": "completed"})
	server.issueSources = map[string]issuePoller{"issue/linear": linearPoller{client: linearClient}}
	planeClient, planeRequests := newPlaneFixture(t)
	server.planeClients = map[string]*issues.PlaneClient{"issue/sparkdash": planeClient}
	server.github = &constantGitHubCLI{pullRequests: []GitHubPullRequest{
		mergedPullRequest(7, "MACH-7: Fix export", "https://github.com/dan/sparkdash/pull/7", "kim"),
	}}
	merged := seedTrackedJob(t, store, trackerLinear, "MACH-7", "issue/linear", "succeeded")
	completed := seedTrackedJob(t, store, trackerLinear, "MACH-8", "issue/linear", "failed")
	// A legacy job admitted by the Linear trigger carries no stored tracker.
	legacy := seedJob(t, store, "MACH-8: Earlier attempt", "", "failed")
	if _, err := store.db.Exec(`UPDATE jobs SET trigger_identity='issue/linear' WHERE id=?`, legacy); err != nil {
		t.Fatal(err)
	}

	if err := server.reconcileTicketOutcomes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if job := jobByID(t, store, merged); job.State != "resolved" || job.ResolutionKind != "pr" || job.ResolutionDetail != "PR #7 (merged by kim)" {
		t.Fatalf("merged job = %s/%s/%s", job.State, job.ResolutionKind, job.ResolutionDetail)
	}
	for _, id := range []string{completed, legacy} {
		if job := jobByID(t, store, id); job.State != "resolved" || job.ResolutionKind != "linear" || job.ResolutionDetail != "Linear Done" {
			t.Fatalf("completed job %s = %s/%s/%s, want resolved/linear/Linear Done", id, job.State, job.ResolutionKind, job.ResolutionDetail)
		}
	}
	var stateUpdates, comments []linearRequest
	for _, request := range *linearRequests {
		switch {
		case strings.Contains(request.Query, "issueUpdate"):
			stateUpdates = append(stateUpdates, request)
		case strings.Contains(request.Query, "commentCreate"):
			comments = append(comments, request)
		}
	}
	if len(stateUpdates) != 1 || stateUpdates[0].Variables["issue"] != "uuid-MACH-7" || stateUpdates[0].Variables["state"] != "state-done" {
		t.Fatalf("linear state updates = %+v, want MACH-7 moved to Done once", stateUpdates)
	}
	if len(comments) != 1 || comments[0].Variables["issue"] != "uuid-MACH-7" || !strings.Contains(fmt.Sprint(comments[0].Variables["body"]), "[PR #7](https://github.com/dan/sparkdash/pull/7)") {
		t.Fatalf("linear comments = %+v, want one PR comment on MACH-7", comments)
	}
	if len(*planeRequests) != 0 {
		t.Fatalf("the sole Plane client was used for Linear jobs: %+v", *planeRequests)
	}
}

func TestReconcileResolvesBuiltinTicketOnMergedPullRequestAtomically(t *testing.T) {
	server, store := newResolutionTestServer(t)
	planeClient, planeRequests := newPlaneFixture(t)
	server.planeClients = map[string]*issues.PlaneClient{"issue/sparkdash": planeClient}
	ticket, err := store.createTicket(t.Context(), Ticket{Title: "Export CSV", Repository: "sparkdash"}, "MACH")
	if err != nil {
		t.Fatal(err)
	}
	jobID, _, err := store.approveTicket(t.Context(), ticket.Key, testAgent("build", "{{machinist.prompt}}"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET state='succeeded' WHERE id=?`, jobID); err != nil {
		t.Fatal(err)
	}
	// The delivered PR title does not name the ticket; the PR recorded on the
	// job links it.
	if _, err := store.db.Exec(`UPDATE job_metadata SET metadata=json_set(metadata,'$.pr_url',?) WHERE job_id=?`, "https://github.com/dan/sparkdash/pull/42", jobID); err != nil {
		t.Fatal(err)
	}
	server.github = &constantGitHubCLI{pullRequests: []GitHubPullRequest{
		mergedPullRequest(42, "Add CSV export", "https://github.com/dan/sparkdash/pull/42", "kim"),
	}}

	for range 2 {
		if err := server.reconcileTicketOutcomes(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if job := jobByID(t, store, jobID); job.State != "resolved" || job.ResolutionKind != "pr" || job.ResolutionDetail != "PR #42 (merged by kim)" {
		t.Fatalf("builtin job = %s/%s/%s", job.State, job.ResolutionKind, job.ResolutionDetail)
	}
	updated, err := store.ticket(t.Context(), ticket.Key)
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != "done" {
		t.Fatalf("ticket state = %q, want done", updated.State)
	}
	resolutionComments := 0
	for _, comment := range updated.Comments {
		if strings.Contains(comment.Body, "Resolved by merged pull request #42") && strings.Contains(comment.Body, "https://github.com/dan/sparkdash/pull/42") {
			resolutionComments++
		}
	}
	if resolutionComments != 1 {
		t.Fatalf("resolution comments = %d, want exactly one across passes: %+v", resolutionComments, updated.Comments)
	}
	if len(*planeRequests) != 0 {
		t.Fatalf("the sole Plane client was used for a builtin ticket: %+v", *planeRequests)
	}
}

func TestPlaneClientForIgnoresBuiltinAndLinearJobs(t *testing.T) {
	server, _ := newResolutionTestServer(t)
	planeClient, _ := newPlaneFixture(t)
	linearClient, _ := newLinearFixture(t, nil)
	server.planeClients = map[string]*issues.PlaneClient{"issue/sparkdash": planeClient}
	server.issueSources = map[string]issuePoller{"issue/linear": linearPoller{client: linearClient}}
	if server.planeClientFor(Job{Title: "MACH-5: legacy"}) != planeClient {
		t.Fatal("legacy Plane job lost the single-client fallback")
	}
	if server.planeClientFor(Job{Metadata: &JobMetadata{Tracker: trackerBuiltin, TicketKey: "MACH-5"}}) != nil {
		t.Fatal("builtin job got the Plane fallback client")
	}
	if server.planeClientFor(Job{Metadata: &JobMetadata{Tracker: trackerLinear, TicketKey: "MACH-5"}, TriggerID: "issue/linear"}) != nil {
		t.Fatal("Linear job got the Plane fallback client")
	}
	if server.planeClientFor(Job{Title: "MACH-5: legacy", TriggerID: "issue/linear"}) != nil {
		t.Fatal("legacy job of a Linear trigger got the Plane fallback client")
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

func TestReconcileGroupsJobsByTicketAndSkipsDoneTickets(t *testing.T) {
	server, store := newResolutionTestServer(t)
	open1 := seedJob(t, store, "MACH-6: First attempt", "", "failed")
	open2 := seedJob(t, store, "MACH-6: Second attempt", "", "failed")
	done := seedJob(t, store, "MACH-5: Already done", "", "failed")
	planeClient, requests := newPlaneFixture(t)
	cli, _ := newScriptedGitHubCLI(scriptedGitHubResult{stdout: `[
		{"number": 30, "title": "MACH-6: fix", "headRefName": "mach-6", "url": "https://github.com/dan/sparkdash/pull/30", "mergedBy": {"login": "kim"}},
		{"number": 31, "title": "MACH-5: fix", "headRefName": "mach-5", "url": "https://github.com/dan/sparkdash/pull/31", "mergedBy": {"login": "kim"}}
	]`})
	server.github = cli
	server.planeClients = map[string]*issues.PlaneClient{"issue/sparkdash": planeClient}

	if err := server.reconcileTicketOutcomes(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{open1, open2, done} {
		if job := jobByID(t, store, id); job.State != "resolved" {
			t.Fatalf("job %s state = %q, want resolved", id, job.State)
		}
	}
	comments := map[string]int{}
	patches := map[string]int{}
	for _, request := range *requests {
		if request.Method == http.MethodPost && strings.HasSuffix(request.Path, "/comments/") {
			comments[request.Path]++
		}
		if request.Method == http.MethodPatch {
			patches[request.Path]++
		}
	}
	if comments["/api/v1/workspaces/platform/projects/proj/issues/uuid-MACH-6/comments/"] != 1 {
		t.Fatalf("MACH-6 comments = %v, want exactly one for two jobs", comments)
	}
	for path := range comments {
		if strings.Contains(path, "MACH-5") {
			t.Fatalf("already-done MACH-5 was commented: %v", comments)
		}
	}
	for path := range patches {
		if strings.Contains(path, "MACH-5") {
			t.Fatalf("already-done MACH-5 was patched: %v", patches)
		}
	}
}
