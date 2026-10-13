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
	handler := func(request *http.Request) *http.Response {
		sawAuth = request.Header.Get("Authorization") == "test-linear-key"
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(request.Body).Decode(&payload)
		switch {
		case strings.Contains(payload.Query, "query Issue"):
			return fixtureResponse(`{"data":{"issues":{"nodes":[{"id":"uuid-1","identifier":"ABC-12","title":"Add archive page","description":"Spec","url":"https://linear.example.app/ABC-12","team":{"id":"team-1"},"labels":[{"name":"factory"}]}]}}}`)
		case strings.Contains(payload.Query, "query Issues"):
			if payload.Variables["label"] != "factory" || payload.Variables["project"] != "Platform" {
				return nil
			}
			return fixtureResponse(`{"data":{"issues":{"nodes":[{"id":"uuid-1","identifier":"ABC-12","title":"Add archive page","url":"https://linear.example.app/ABC-12","team":{"id":"team-1"}}]}}}`)
		case strings.Contains(payload.Query, "commentCreate"):
			return fixtureResponse(`{"data":{"commentCreate":{"success":true}}}`)
		case strings.Contains(payload.Query, "workflowStates"):
			return fixtureResponse(`{"data":{"workflowStates":{"nodes":[{"id":"state-1","name":"Done"}]}}}`)
		case strings.Contains(payload.Query, "issueUpdate"):
			return fixtureResponse(`{"data":{"issueUpdate":{"success":true}}}`)
		default:
			return nil
		}
	}
	client := &LinearClient{config: LinearConfig{APIKey: "test-linear-key"}, project: "Platform", endpoint: "linear://fixture", client: &http.Client{Transport: fixtureTransport{handler: handler}}}
	issue, err := client.FetchIssue(t.Context(), "ABC-12")
	if err != nil || issue.Key != "ABC-12" || issue.Title != "Add archive page" || issue.TeamID != "team-1" || !sawAuth {
		t.Fatalf("issue = %#v, err = %v, auth = %t", issue, err, sawAuth)
	}
	if err = client.PostComment(t.Context(), "ABC-12", "Factory update"); err != nil {
		t.Fatal(err)
	}
	issues, err := client.IssuesWithLabel(t.Context(), "factory", "Platform")
	if err != nil || len(issues) != 1 || issues[0].Key != "ABC-12" {
		t.Fatalf("poll = %#v, err = %v", issues, err)
	}
	if err = client.SetState(t.Context(), "ABC-12", "done"); err != nil {
		t.Fatal(err)
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
