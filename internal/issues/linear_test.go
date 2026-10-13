package issues

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func newFixtureLinearClient(handler func(*http.Request) *http.Response, resolvedState string) *LinearClient {
	return &LinearClient{
		config:   LinearConfig{APIKey: "test-linear-key", ResolvedState: resolvedState},
		endpoint: linearEndpoint,
		client:   &http.Client{Transport: fixtureTransport{handler: handler}},
	}
}

func linearRequestBody(t *testing.T, request *http.Request) map[string]any {
	t.Helper()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	return parsed
}

func TestLinearRequestSurfacesGraphQLErrors(t *testing.T) {
	handler := func(request *http.Request) *http.Response {
		return fixtureResponse(`{"data":null,"errors":[{"message":"Issue not found"}]}`)
	}
	client := newFixtureLinearClient(handler, "")
	var output struct {
		Data struct {
			Issues struct {
				Nodes []linearIssueNode `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
	}
	if err := client.request(t.Context(), map[string]any{"query": "query { issues { nodes { id } } }"}, &output); err == nil {
		t.Fatal("expected GraphQL errors to fail the request")
	}
}

func TestLinearIssuesWithLabelPaginates(t *testing.T) {
	requests := 0
	handler := func(request *http.Request) *http.Response {
		requests++
		parsed := linearRequestBody(t, request)
		variables := parsed["variables"].(map[string]any)
		switch requests {
		case 1:
			if _, ok := variables["after"]; !ok {
				t.Fatalf("expected after variable in first request: %v", variables)
			}
			return fixtureResponse(`{"data":{"issues":{"nodes":[{"id":"1","identifier":"A-1","title":"First"},{"id":"2","identifier":"A-2","title":"Second"}],"pageInfo":{"hasNextPage":true,"endCursor":"CUR1"}}}}`)
		case 2:
			if variables["after"] != "CUR1" {
				t.Fatalf("expected endCursor in second request, got %v", variables["after"])
			}
			return fixtureResponse(`{"data":{"issues":{"nodes":[{"id":"3","identifier":"A-3","title":"Third"}],"pageInfo":{"hasNextPage":false,"endCursor":""}}}}`)
		default:
			return nil
		}
	}
	client := newFixtureLinearClient(handler, "")
	issues, err := client.IssuesWithLabel(t.Context(), "factory", "Platform")
	if err != nil {
		t.Fatalf("IssuesWithLabel: %v", err)
	}
	if len(issues) != 3 || issues[2].Key != "A-3" {
		t.Fatalf("expected 3 issues across pages, got %#v", issues)
	}
	if requests != 2 {
		t.Fatalf("expected 2 requests, got %d", requests)
	}
}

func TestLinearSetStateVerifiesMutationSuccess(t *testing.T) {
	success := true
	handler := func(request *http.Request) *http.Response {
		parsed := linearRequestBody(t, request)
		query, _ := parsed["query"].(string)
		switch {
		case strings.Contains(query, "issues(filter:"):
			return fixtureResponse(`{"data":{"issues":{"nodes":[{"id":"uuid-1","identifier":"A-1","title":"T","team":{"id":"team-1"}}]}}}`)
		case strings.Contains(query, "workflowStates"):
			return fixtureResponse(`{"data":{"workflowStates":{"nodes":[{"id":"state-done","name":"Done"},{"id":"state-todo","name":"Todo"}]}}}`)
		case strings.Contains(query, "issueUpdate"):
			if !success {
				return fixtureResponse(`{"data":{"issueUpdate":{"success":false}}}`)
			}
			return fixtureResponse(`{"data":{"issueUpdate":{"success":true}}}`)
		default:
			return nil
		}
	}
	client := newFixtureLinearClient(handler, "")

	if err := client.SetState(t.Context(), "A-1", "Done"); err != nil {
		t.Fatalf("SetState: %v", err)
	}

	success = false
	if err := client.SetState(t.Context(), "A-1", "Done"); err == nil {
		t.Fatal("expected failed mutation to error")
	}
}

func TestLinearIssueState(t *testing.T) {
	handler := func(request *http.Request) *http.Response {
		return fixtureResponse(`{"data":{"issues":{"nodes":[{"state":{"name":"In Progress"}}]}}}`)
	}
	client := newFixtureLinearClient(handler, "")
	state, err := client.IssueState(t.Context(), "A-1")
	if err != nil {
		t.Fatalf("IssueState: %v", err)
	}
	if state != "In Progress" {
		t.Fatalf("expected In Progress, got %q", state)
	}
}

func TestLinearDefaultResolvedState(t *testing.T) {
	client, err := NewLinearClient(LinearConfig{APIKey: "key"})
	if err != nil {
		t.Fatalf("NewLinearClient: %v", err)
	}
	if client.ResolvedState() != DefaultLinearResolvedState {
		t.Fatalf("expected default resolved state %q, got %q", DefaultLinearResolvedState, client.ResolvedState())
	}
	client, err = NewLinearClient(LinearConfig{APIKey: "key", ResolvedState: "Shipped"})
	if err != nil {
		t.Fatalf("NewLinearClient: %v", err)
	}
	if client.ResolvedState() != "Shipped" {
		t.Fatalf("expected Shipped, got %q", client.ResolvedState())
	}
}
