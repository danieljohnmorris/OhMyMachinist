package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestReviewInstructionsRoundTrip(t *testing.T) {
	store := openTestStore(t, t.TempDir()+"/machinist.db")
	ctx := context.Background()

	if _, found, err := store.ReviewInstructions(ctx, "omacom/mobile"); err != nil || found {
		t.Fatalf("expected no stored instructions, found=%v err=%v", found, err)
	}
	if err := store.SaveReviewInstructions(ctx, "omacom/mobile", "Run go test before review.\n"); err != nil {
		t.Fatal(err)
	}
	stored, found, err := store.ReviewInstructions(ctx, "omacom/mobile")
	if err != nil || !found {
		t.Fatalf("expected stored instructions, found=%v err=%v", found, err)
	}
	if stored != "Run go test before review." {
		t.Fatalf("stored instructions trailing-newline-normalized, got %q", stored)
	}
	list, err := store.AllReviewInstructions(ctx)
	if err != nil || len(list) != 1 || list[0].Repository != "omacom/mobile" {
		t.Fatalf("unexpected list: %v err=%v", list, err)
	}
	if err := store.DeleteReviewInstructions(ctx, "omacom/mobile"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := store.ReviewInstructions(ctx, "omacom/mobile"); found {
		t.Fatal("expected instructions deleted")
	}
}

func TestResolveReviewProject(t *testing.T) {
	server := &Server{githubRepositories: map[string]string{"mobile": "omacom/Mobile"}}
	cases := []struct{ input, want string }{
		{"mobile", "omacom/mobile"},
		{"MOBILE", "omacom/mobile"},
		{"omacom/Mobile", "omacom/mobile"},
		{"other/repo", "other/repo"},
		{"  ", ""},
	}
	for _, test := range cases {
		if got := server.resolveReviewProject(test.input); got != test.want {
			t.Errorf("resolveReviewProject(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestReviewInstructionsHTTP(t *testing.T) {
	server, endpoint := newTestHTTPServer(t)
	server.githubRepositories = map[string]string{"mobile": "omacom/mobile"}
	csrf := server.csrfToken

	// Default is served when nothing is stored.
	response, err := http.Get(endpoint.URL + "/api/v1/review-instructions?project=mobile")
	if err != nil {
		t.Fatal(err)
	}
	var payload reviewInstructionsResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if payload.Custom || payload.Instructions != DefaultReviewInstructions || payload.Project != "omacom/mobile" {
		t.Fatalf("expected default response, got %+v", payload)
	}

	// Saving requires browser CSRF or a worker bearer token.
	save := func(body string, headers map[string]string) *http.Response {
		request, err := http.NewRequest(http.MethodPut, endpoint.URL+"/api/v1/review-instructions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		for key, value := range headers {
			request.Header.Set(key, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	denied := save(`{"project":"mobile","instructions":"# project rules"}`, nil)
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF, got %d", denied.StatusCode)
	}
	denied.Body.Close()

	accepted := save(`{"project":"mobile","instructions":"# project rules"}`, map[string]string{"X-Machinist-CSRF": csrf, "Origin": endpoint.URL})
	if accepted.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", accepted.StatusCode, readAllBody(accepted))
	}
	accepted.Body.Close()

	response, err = http.Get(endpoint.URL + "/api/v1/review-instructions?project=mobile")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if !payload.Custom || payload.Instructions != "# project rules" {
		t.Fatalf("expected stored override, got %+v", payload)
	}

	// An empty body deletes the override; the default is served again.
	cleared := save(`{"project":"mobile","instructions":""}`, map[string]string{"X-Machinist-CSRF": csrf, "Origin": endpoint.URL})
	if cleared.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on delete, got %d", cleared.StatusCode)
	}
	cleared.Body.Close()
	response, err = http.Get(endpoint.URL + "/api/v1/review-instructions?project=omacom/mobile")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if payload.Custom || payload.Instructions != DefaultReviewInstructions {
		t.Fatalf("expected default after delete, got %+v", payload)
	}
}

func TestReviewInstructionsListEndpoint(t *testing.T) {
	server, endpoint := newTestHTTPServer(t)
	if err := server.store.SaveReviewInstructions(t.Context(), "omacom/mobile", "rules"); err != nil {
		t.Fatal(err)
	}
	response, err := http.Get(endpoint.URL + "/api/v1/review-instructions")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var payload reviewInstructionsListResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Default != DefaultReviewInstructions || payload.Instructions["omacom/mobile"] != "rules" {
		t.Fatalf("unexpected list payload: %+v", payload)
	}
}

func readAllBody(response *http.Response) string {
	var buffer bytes.Buffer
	_, _ = buffer.ReadFrom(response.Body)
	return fmt.Sprintf("%s", buffer.String())
}
