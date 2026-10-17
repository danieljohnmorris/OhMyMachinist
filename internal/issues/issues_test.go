package issues

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fixtureTransport struct {
	handler func(*http.Request) *http.Response
}

func (transport fixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if response := transport.handler(request); response != nil {
		return response, nil
	}
	return nil, errors.New("unexpected fixture request")
}

func fixtureResponse(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func hmacSHA256(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestPlaneClientFetchesPostsAndPolls(t *testing.T) {
	var sawAPIKey bool
	handler := func(request *http.Request) *http.Response {
		sawAPIKey = request.Header.Get("X-API-Key") == "test-plane-key"
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/workspaces/platform/projects/platform-project/issues/issue-1/":
			return fixtureResponse(`{"id":"issue-1","name":"Add archive page","description":"Spec","labels":[{"name":"factory"}]}`)
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/workspaces/platform/projects/platform-project/issues/" && request.URL.Query().Get("labels") == "factory":
			return fixtureResponse(`{"results":[{"id":"issue-1","name":"Add archive page","labels":[{"name":"factory"}]}]}`)
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/workspaces/platform/projects/platform-project/issues/issue-1/comments/":
			body, _ := io.ReadAll(request.Body)
			if !strings.Contains(string(body), "Factory update") {
				return nil
			}
			return fixtureResponse(`{"id":"comment-1"}`)
		case request.Method == http.MethodPatch && request.URL.Path == "/api/v1/workspaces/platform/projects/platform-project/issues/issue-1/":
			return fixtureResponse(`{"id":"issue-1"}`)
		default:
			return nil
		}
	}
	client := &PlaneClient{config: PlaneConfig{URL: "plane://fixture", Workspace: "platform", ProjectID: "platform-project", APIKey: "test-plane-key"}, client: &http.Client{Transport: fixtureTransport{handler: handler}}}
	issue, err := client.FetchIssue(t.Context(), "issue-1")
	if err != nil || issue.Key != "issue-1" || issue.Title != "Add archive page" ||
		issue.URL != "plane://fixture/api/v1/workspaces/platform/projects/platform-project/issues/issue-1/" || !sawAPIKey {
		t.Fatalf("issue = %#v, err = %v, api key = %t", issue, err, sawAPIKey)
	}
	if err = client.PostComment(t.Context(), "issue-1", "Factory update"); err != nil {
		t.Fatal(err)
	}
	issues, err := client.IssuesWithLabel(t.Context(), "factory")
	if err != nil || len(issues) != 1 || issues[0].Key != "issue-1" || issues[0].URL != "plane://fixture/api/v1/workspaces/platform/projects/platform-project/issues/issue-1/" {
		t.Fatalf("poll = %#v, err = %v", issues, err)
	}
	if err = client.SetState(t.Context(), "issue-1", "done"); err != nil {
		t.Fatal(err)
	}
}

func TestPlaneClientPollUsesHumanKeysAndSkipsClosedTickets(t *testing.T) {
	var projectLookups, stateLookups int
	handler := func(request *http.Request) *http.Response {
		switch {
		case request.URL.Path == "/api/v1/workspaces/platform/projects/platform-project/issues/":
			if request.URL.Query().Get("labels") != "factory" || !strings.Contains(request.URL.Query().Get("expand"), "state") {
				return nil
			}
			return fixtureResponse(`{"results":[
				{"id":"uuid-open","sequence_id":5,"name":"Open","labels":[{"name":"factory"}],"state":{"id":"s-started","name":"In Progress","group":"started"}},
				{"id":"uuid-done","sequence_id":6,"name":"Shipped","labels":[{"name":"factory"}],"state":{"id":"s-shipped","name":"Shipped","group":"completed"}},
				{"id":"uuid-cancelled","sequence_id":7,"name":"Dropped","state":"s-cancelled"},
				{"id":"uuid-backlog","sequence_id":8,"name":"Queued","state":"s-backlog"}
			]}`)
		case request.URL.Path == "/api/v1/workspaces/platform/projects/platform-project/":
			projectLookups++
			return fixtureResponse(`{"id":"platform-project","identifier":"MACH"}`)
		case request.URL.Path == "/api/v1/workspaces/platform/projects/platform-project/states/":
			stateLookups++
			return fixtureResponse(`{"results":[{"id":"s-cancelled","name":"Won't do","group":"cancelled"},{"id":"s-backlog","name":"Backlog","group":"backlog"}]}`)
		default:
			return nil
		}
	}
	client := &PlaneClient{config: PlaneConfig{URL: "https://plane.example", Workspace: "platform", ProjectID: "platform-project", APIKey: "key"}, client: &http.Client{Transport: fixtureTransport{handler: handler}}}
	found, err := client.IssuesWithLabel(t.Context(), "factory")
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found[0].Key != "MACH-5" || found[0].ID != "uuid-open" || found[1].Key != "MACH-8" {
		t.Fatalf("poll = %#v, want open MACH-5 and MACH-8 only", found)
	}
	if found[0].URL != "https://plane.example/platform/browse/MACH-5/" {
		t.Fatalf("url = %q, want browse URL", found[0].URL)
	}
	if projectLookups != 1 || stateLookups != 1 {
		t.Fatalf("project lookups = %d, state lookups = %d, want one each", projectLookups, stateLookups)
	}
}

func TestPlaneClientIssueState(t *testing.T) {
	handler := func(request *http.Request) *http.Response {
		switch {
		case strings.HasSuffix(request.URL.Path, "/issues/issue-1/"):
			return fixtureResponse(`{"id":"issue-1","name":"Add archive page","state":{"name":"Done","group":"completed"}}`)
		case strings.HasSuffix(request.URL.Path, "/issues/issue-2/"):
			return fixtureResponse(`{"id":"issue-2","name":"Wip","state":"In Progress"}`)
		default:
			return nil
		}
	}
	client := &PlaneClient{config: PlaneConfig{URL: "plane://fixture", Workspace: "platform", ProjectID: "platform-project", APIKey: "key"}, client: &http.Client{Transport: fixtureTransport{handler: handler}}}

	state, err := client.IssueState(t.Context(), "issue-1")
	if err != nil || state.Name != "Done" || state.Group != "completed" {
		t.Fatalf("state = %#v, err = %v", state, err)
	}
	if !IsCompletedPlaneState(state) {
		t.Fatal("completed group state not recognized")
	}
	state, err = client.IssueState(t.Context(), "issue-2")
	if err != nil || state.Name != "In Progress" {
		t.Fatalf("state = %#v, err = %v", state, err)
	}
	if IsCompletedPlaneState(state) {
		t.Fatal("started state recognized as completed")
	}
	if !IsCompletedPlaneState(PlaneIssueState{Name: "done"}) {
		t.Fatal("literal Done state not recognized")
	}
}

func TestParseIssueWebhookState(t *testing.T) {
	issue, state, err := ParseIssueWebhookState([]byte(`{"issue":{"identifier":"MACH-5","name":"Fix sidebar","state":{"name":"Done","group":"completed"}}}`))
	if err != nil || issue.Key != "MACH-5" || state.Name != "Done" || state.Group != "completed" {
		t.Fatalf("issue = %#v, state = %#v, err = %v", issue, state, err)
	}
	if !IsCompletedPlaneState(state) {
		t.Fatal("completed webhook state not recognized")
	}
	issue, state, err = ParseIssueWebhookState([]byte(`{"issue":{"identifier":"MACH-5","name":"Fix sidebar","state_name":"In Progress"}}`))
	if err != nil || state.Name != "In Progress" || IsCompletedPlaneState(state) {
		t.Fatalf("issue = %#v, state = %#v, err = %v", issue, state, err)
	}
	issue, state, err = ParseIssueWebhookState([]byte(`{"data":{"issue":{"identifier":"ABC-12","title":"Add archive page"}}}`))
	if err != nil || issue.Key != "ABC-12" || (state.Name != "" && IsCompletedPlaneState(state)) {
		t.Fatalf("issue = %#v, state = %#v, err = %v", issue, state, err)
	}
}

func TestLinearClientFetchesPostsAndPolls(t *testing.T) {
	var sawAuth bool
	var stateUpdate map[string]any
	var pollVariables []map[string]any
	handler := func(request *http.Request) *http.Response {
		sawAuth = request.Header.Get("Authorization") == "test-linear-key"
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(request.Body).Decode(&payload)
		switch {
		case strings.HasPrefix(payload.Query, "query Issue(") && strings.Contains(payload.Query, "states("):
			// Team states: a renamed completed state must still match "Done".
			return fixtureResponse(`{"data":{"issue":{"id":"uuid-1","team":{"states":{"nodes":[{"id":"state-started","name":"In Progress","type":"started"},{"id":"state-shipped","name":"Shipped","type":"completed"}]}}}}}`)
		case strings.HasPrefix(payload.Query, "query Issue("):
			// The singleton issue(id:) query resolves human identifiers;
			// IssueFilter.id only compares UUIDs and must not be used.
			if !strings.Contains(payload.Query, "issue(id: $key)") || strings.Contains(payload.Query, "filter") {
				return fixtureResponse(`{"errors":[{"message":"unexpected issue lookup"}]}`)
			}
			if payload.Variables["key"] != "ABC-12" {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}},
					Body: io.NopCloser(strings.NewReader(`{"data":null,"errors":[{"message":"Entity not found: Issue","extensions":{"code":"INVALID_INPUT"}}]}`))}
			}
			return fixtureResponse(`{"data":{"issue":{"id":"uuid-1","identifier":"ABC-12","title":"Add archive page","description":"Spec","url":"https://linear.example.app/ABC-12","team":{"id":"team-1"},"labels":{"nodes":[{"name":"factory"}]},"state":{"name":"Shipped","type":"completed"}}}}`)
		case strings.HasPrefix(payload.Query, "query Issues("):
			if payload.Variables["label"] != "factory" || payload.Variables["project"] != "Platform" || !strings.Contains(payload.Query, `nin: ["completed", "canceled"]`) {
				return nil
			}
			pollVariables = append(pollVariables, payload.Variables)
			if payload.Variables["after"] == nil {
				return fixtureResponse(`{"data":{"issues":{"nodes":[{"id":"uuid-1","identifier":"ABC-12","title":"Add archive page","url":"https://linear.example.app/ABC-12","team":{"id":"team-1"},"labels":{"nodes":[{"name":"factory"}]}}],"pageInfo":{"hasNextPage":true,"endCursor":"cursor-1"}}}}`)
			}
			return fixtureResponse(`{"data":{"issues":{"nodes":[{"id":"uuid-2","identifier":"ABC-13","title":"Second page","url":"https://linear.example.app/ABC-13","team":{"id":"team-1"},"labels":{"nodes":[{"name":"Factory"}]}}],"pageInfo":{"hasNextPage":false,"endCursor":"cursor-2"}}}}`)
		case strings.Contains(payload.Query, "commentCreate"):
			if payload.Variables["issue"] != "uuid-1" {
				return nil
			}
			return fixtureResponse(`{"data":{"commentCreate":{"success":true}}}`)
		case strings.Contains(payload.Query, "issueUpdate"):
			stateUpdate = payload.Variables
			return fixtureResponse(`{"data":{"issueUpdate":{"success":true}}}`)
		default:
			return nil
		}
	}
	client := &LinearClient{config: LinearConfig{APIKey: "test-linear-key"}, project: "Platform", endpoint: "linear://fixture", client: &http.Client{Transport: fixtureTransport{handler: handler}}}
	issue, err := client.FetchIssue(t.Context(), "ABC-12")
	if err != nil || issue.Key != "ABC-12" || issue.Title != "Add archive page" || issue.TeamID != "team-1" || !sawAuth ||
		len(issue.Labels) != 1 || issue.Labels[0] != "factory" {
		t.Fatalf("issue = %#v, err = %v, auth = %t", issue, err, sawAuth)
	}
	state, err := client.IssueState(t.Context(), "ABC-12")
	if err != nil || !IsCompletedLinearState(state) || state.URL != "https://linear.example.app/ABC-12" {
		t.Fatalf("state = %#v, err = %v", state, err)
	}
	if _, err = client.FetchIssue(t.Context(), "ABC-404"); !errors.Is(err, ErrIssueNotFound) {
		t.Fatalf("missing issue err = %v, want ErrIssueNotFound", err)
	}
	if err = client.PostComment(t.Context(), "ABC-12", "Factory update"); err != nil {
		t.Fatal(err)
	}
	issues, err := client.IssuesWithLabel(t.Context(), "factory", "Platform")
	if err != nil || len(issues) != 2 || issues[0].Key != "ABC-12" || issues[1].Key != "ABC-13" || len(issues[1].Labels) != 1 {
		t.Fatalf("poll = %#v, err = %v", issues, err)
	}
	if len(pollVariables) != 2 || pollVariables[1]["after"] != "cursor-1" {
		t.Fatalf("poll pages = %#v", pollVariables)
	}
	if err = client.SetState(t.Context(), "ABC-12", CompletedLinearState); err != nil {
		t.Fatal(err)
	}
	if stateUpdate["issue"] != "uuid-1" || stateUpdate["state"] != "state-shipped" {
		t.Fatalf("state update = %#v", stateUpdate)
	}
	if err = client.SetState(t.Context(), "ABC-12", "Nonexistent"); err == nil {
		t.Fatal("unknown state accepted")
	}
}

func TestLinearClientSurfacesGraphQLErrorsAndFailedMutations(t *testing.T) {
	handler := func(request *http.Request) *http.Response {
		var payload struct {
			Query string `json:"query"`
		}
		_ = json.NewDecoder(request.Body).Decode(&payload)
		switch {
		case strings.HasPrefix(payload.Query, "query Issues("):
			// Linear reports validation and rate-limit failures as GraphQL errors
			// with HTTP 200 or 400 and null data.
			return &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(`{"data":null,"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATELIMITED"}}]}`))}
		case strings.HasPrefix(payload.Query, "query Issue("):
			return fixtureResponse(`{"data":{"issue":{"id":"uuid-1","identifier":"ABC-12"}}}`)
		case strings.Contains(payload.Query, "commentCreate"):
			return fixtureResponse(`{"data":{"commentCreate":{"success":false}}}`)
		default:
			return fixtureResponse(`{"errors":[{"message":"Cannot query field"}]}`)
		}
	}
	client := &LinearClient{config: LinearConfig{APIKey: "key"}, endpoint: "linear://fixture", client: &http.Client{Transport: fixtureTransport{handler: handler}}}
	if _, err := client.IssuesWithLabel(t.Context(), "factory", ""); err == nil || !strings.Contains(err.Error(), "RATELIMITED") {
		t.Fatalf("poll err = %v, want rate limit error", err)
	}
	if err := client.PostComment(t.Context(), "ABC-12", "hello"); err == nil {
		t.Fatal("unsuccessful commentCreate accepted")
	}
}

func TestWebhookHMACAndParsing(t *testing.T) {
	body := []byte(`{"data":{"issue":{"identifier":"ABC-12","title":"Add archive page"}}}`)
	if VerifyHMAC("secret", body, "") {
		t.Fatal("missing signature accepted")
	}
	mac := hmacSHA256("secret", body)
	if !VerifyHMAC("secret", body, "sha256="+mac) {
		t.Fatal("valid signature rejected")
	}
	issue, err := ParseIssueWebhook(body)
	if err != nil || issue.Key != "ABC-12" || issue.Title != "Add archive page" {
		t.Fatalf("issue = %#v, err = %v", issue, err)
	}
}

func TestWebhookHandlerVerifiesBeforeSubmission(t *testing.T) {
	submitted := make(chan Issue, 1)
	handler := WebhookHandler{Secret: "secret", Submit: func(issue Issue) error { submitted <- issue; return nil }}
	body := []byte(`{"issue":{"id":"issue-1","name":"Add archive page"}}`)
	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned response = %d", recorder.Code)
	}
	request = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	request.Header.Set("X-Signature", "sha256="+hmacSHA256("secret", body))
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("signed response = %d", recorder.Code)
	}
	issue := <-submitted
	if issue.Key != "issue-1" || issue.Title != "Add archive page" {
		t.Fatalf("issue = %#v", issue)
	}
}

func TestTitleIncludesIssueKey(t *testing.T) {
	if got := Title("ABC-12", "Add archive page"); got != "ABC-12: Add archive page" {
		t.Fatalf("title = %q", got)
	}
}

func TestParseIssueWebhookIgnoresNoOpEvents(t *testing.T) {
	noOps := []string{
		// Plane state-only update: activity names the changed field.
		`{"event":"issue","action":"update","data":{"id":"issue-1","name":"Add archive page"},"activity":{"field":"state","old_value":"Todo","new_value":"Done"}}`,
		// Comment-only event carries no issue content.
		`{"event":"issue_comment","action":"create","data":{"comment_html":"<p>ping</p>"}}`,
		// Deletions never submit work.
		`{"event":"issue","action":"delete","data":{"id":"issue-1"}}`,
		// Assignee churn is not intake content.
		`{"event":"issue","action":"update","data":{"id":"issue-1","name":"Add archive page"},"activity":{"field":"assignees","new_value":["u2"]}}`,
	}
	for _, body := range noOps {
		if _, err := ParseIssueWebhook([]byte(body)); !errors.Is(err, ErrNoOpEvent) {
			t.Fatalf("ParseIssueWebhook(%s) = %v, want ErrNoOpEvent", body, err)
		}
	}
	// Content updates still parse, including Plane's flat data payload.
	content := []byte(`{"event":"issue","action":"update","data":{"id":"issue-1","name":"Add archive page","description":"Build it."},"activity":{"field":"description","old_value":"old","new_value":"Build it."}}`)
	issue, err := ParseIssueWebhook(content)
	if err != nil || issue.Key != "issue-1" || issue.Title != "Add archive page" || issue.Description != "Build it." {
		t.Fatalf("issue = %#v, err = %v", issue, err)
	}
}

func TestWebhookHandlerAcknowledgesNoOpEventsWithoutSubmit(t *testing.T) {
	submissions := 0
	handler := WebhookHandler{Secret: "secret", Submit: func(Issue) error { submissions++; return nil }}
	body := []byte(`{"event":"issue","action":"update","data":{"id":"issue-1","name":"Add archive page"},"activity":{"field":"state","old_value":"Todo","new_value":"Done"}}`)
	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(body)))
	request.Header.Set("X-Signature", "sha256="+hmacSHA256("secret", body))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("no-op response = %d", recorder.Code)
	}
	if submissions != 0 {
		t.Fatalf("submissions = %d", submissions)
	}
}
