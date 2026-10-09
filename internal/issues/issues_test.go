package issues

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func hmacSHA256(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func TestPlaneClientFetchesPostsAndPolls(t *testing.T) {
	var sawAPIKey bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		sawAPIKey = request.Header.Get("X-API-Key") == "test-plane-key"
		if !strings.Contains(request.URL.Path, "/api/v1/workspaces/platform/projects/platform-project/") {
			t.Fatalf("unexpected path: %s", request.URL.Path)
		}
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/issues/issue-1/"):
			_, _ = response.Write([]byte(`{"id":"issue-1","name":"Add archive page","description":"Spec","labels":[{"name":"factory"}]}`))
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/issues/"):
			_, _ = response.Write([]byte(`{"results":[{"id":"issue-1","name":"Add archive page","labels":[{"name":"factory"}]}]}`))
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/issues/issue-1/comments/"):
			body, _ := io.ReadAll(request.Body)
			if !strings.Contains(string(body), "Factory update") {
				t.Fatalf("comment body = %s", string(body))
			}
			_, _ = response.Write([]byte(`{"id":"comment-1"}`))
		case request.Method == http.MethodPatch && strings.HasSuffix(request.URL.Path, "/issues/issue-1/"):
			_, _ = response.Write([]byte(`{"id":"issue-1"}`))
		default:
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
	}))
	defer server.Close()
	client, err := NewPlaneClient(PlaneConfig{URL: server.URL, Workspace: "platform", ProjectID: "platform-project", APIKey: "test-plane-key"})
	if err != nil {
		t.Fatal(err)
	}
	issue, err := client.FetchIssue(t.Context(), "issue-1")
	if err != nil || issue.Key != "issue-1" || issue.Title != "Add archive page" || !sawAPIKey {
		t.Fatalf("issue = %#v, err = %v, api key = %t", issue, err, sawAPIKey)
	}
	if err = client.PostComment(t.Context(), "issue-1", "Factory update"); err != nil {
		t.Fatal(err)
	}
	issues, err := client.IssuesWithLabel(t.Context(), "factory")
	if err != nil || len(issues) != 1 || issues[0].Key != "issue-1" {
		t.Fatalf("poll = %#v, err = %v", issues, err)
	}
	if err = client.SetState(t.Context(), "issue-1", "done"); err != nil {
		t.Fatal(err)
	}
}

func TestLinearClientFetchesPostsAndPolls(t *testing.T) {
	var sawAuth bool
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		sawAuth = request.Header.Get("Authorization") == "test-linear-key"
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.NewDecoder(request.Body).Decode(&payload)
		switch {
		case strings.Contains(payload.Query, "query Issue"):
			_, _ = response.Write([]byte(`{"data":{"issues":{"nodes":[{"id":"uuid-1","identifier":"ABC-12","title":"Add archive page","description":"Spec","url":"https://linear.example.app/ABC-12","team":{"id":"team-1"},"labels":[{"name":"factory"}]}]}}}`))
		case strings.Contains(payload.Query, "query Issues"):
			if payload.Variables["label"] != "factory" || payload.Variables["project"] != "Platform" {
				t.Fatalf("variables = %#v", payload.Variables)
			}
			_, _ = response.Write([]byte(`{"data":{"issues":{"nodes":[{"id":"uuid-1","identifier":"ABC-12","title":"Add archive page","url":"https://linear.example.app/ABC-12","team":{"id":"team-1"}}]}}}`))
		case strings.Contains(payload.Query, "commentCreate"):
			_, _ = response.Write([]byte(`{"data":{"commentCreate":{"success":true}}}`))
		case strings.Contains(payload.Query, "workflowStates"):
			_, _ = response.Write([]byte(`{"data":{"workflowStates":{"nodes":[{"id":"state-1","name":"Done"}]}}}`))
		case strings.Contains(payload.Query, "issueUpdate"):
			_, _ = response.Write([]byte(`{"data":{"issueUpdate":{"success":true}}}`))
		default:
			t.Fatalf("unexpected query: %s", payload.Query)
		}
	}))
	defer server.Close()
	client := &LinearClient{config: LinearConfig{APIKey: "test-linear-key"}, client: server.Client()}
	client.client.Timeout = 0
	client.client.Transport = server.Client().Transport
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
	server := httptest.NewServer(handler)
	defer server.Close()
	body := []byte(`{"issue":{"id":"issue-1","name":"Add archive page"}}`)
	response, err := http.Post(server.URL, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned response = %d", response.StatusCode)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Signature", "sha256="+hmacSHA256("secret", body))
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("signed response = %d", response.StatusCode)
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
