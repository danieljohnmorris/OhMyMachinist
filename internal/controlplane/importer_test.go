package controlplane

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"

	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/issues"
)

// newImporterServer starts an auth-off control plane on the given definition
// file so importer handlers run without token plumbing.
func newImporterServer(t *testing.T, definition string) (*Server, *httptest.Server) {
	t.Helper()
	directory := t.TempDir()
	definitionPath := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(definitionPath, []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, filepath.Join(directory, "machinist.db"))
	server, err := NewServer(store, definitionPath, "secret", 0, config.AuthNone)
	if err != nil {
		t.Fatal(err)
	}
	return server, httptest.NewServer(server.Handler())
}

// TestImportPlanePreviewAndImport covers the whole Plane path against a stub
// API: the project roster, the per-project plan (state mapping, completed
// skips) and an idempotent import.
func TestImportPlanePreviewAndImport(t *testing.T) {
	// The stub ignores the page parameter and repeats its results; the
	// importer must stop on the first page that yields no new issues.
	var seenPages []string
	plane := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		switch {
		case request.URL.Path == "/api/v1/workspaces/platform/projects/":
			_, _ = response.Write([]byte(`{"results":[{"id":"proj","name":"Platform","identifier":"MACH"},{"id":"other","name":"Other"}]}`))
		case request.URL.Path == "/api/v1/workspaces/platform/projects/proj/":
			_, _ = response.Write([]byte(`{"id":"proj","name":"Platform","identifier":"MACH"}`))
		case request.URL.Path == "/api/v1/workspaces/platform/projects/proj/issues/":
			page := request.URL.Query().Get("page")
			seenPages = append(seenPages, page)
			if page == "2" {
				_, _ = response.Write([]byte(`{"results":[
					{"id":"uuid-8","sequence_id":8,"name":"Dropped","state":{"id":"s-4","name":"Won't do","group":"cancelled"}}
				]}`))
				return
			}
			if page != "1" {
				_, _ = response.Write([]byte(`{"results":[]}`))
				return
			}
			_, _ = response.Write([]byte(`{"results":[
				{"id":"uuid-5","sequence_id":5,"name":"Add archive page","description":"Spec.","state":{"id":"s-1","name":"In Progress","group":"started"}},
				{"id":"uuid-6","sequence_id":6,"name":"Shipped work","state":{"id":"s-2","name":"Done","group":"completed"}},
				{"id":"uuid-7","sequence_id":7,"name":"Queued","state":{"id":"s-3","name":"Backlog","group":"backlog"}}
			]}`))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer plane.Close()
	t.Setenv("PLANE_URL", plane.URL)
	t.Setenv("PLANE_WORKSPACE", "platform")
	t.Setenv("PLANE_API_KEY", "key")

	server, web := newImporterServer(t, `
[commands.plan]
executor = "test"
prompt_file = "plan.md"
timeout = "1m"
[issue_sources.plane.platform]
project_id = "proj"
`)
	defer web.Close()

	// Roster: both remote projects, only the configured one importable.
	response, err := http.Get(web.URL + "/api/v1/import/preview?source=plane")
	if err != nil {
		t.Fatal(err)
	}
	var roster struct {
		Projects []importProject `json:"projects"`
	}
	if err := json.NewDecoder(response.Body).Decode(&roster); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || len(roster.Projects) != 2 ||
		roster.Projects[0].Name != "Platform" || roster.Projects[0].Repository != "platform" ||
		roster.Projects[1].Name != "Other" || roster.Projects[1].Repository != "" {
		t.Fatalf("roster = %#v, status = %d", roster.Projects, response.StatusCode)
	}

	// Plan: started maps to in_progress, backlog stays backlog, completed is
	// skipped with its reason. Keys are the human ticket form.
	response, err = http.Get(web.URL + "/api/v1/import/preview?source=plane&project=Platform")
	if err != nil {
		t.Fatal(err)
	}
	var plan struct {
		Source  string         `json:"source"`
		Project string         `json:"project"`
		Tickets []importTicket `json:"tickets"`
	}
	if err := json.NewDecoder(response.Body).Decode(&plan); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if plan.Source != "plane" || plan.Project != "Platform" || len(plan.Tickets) != 4 {
		t.Fatalf("plan = %#v, status = %d", plan, response.StatusCode)
	}
	if plan.Tickets[0].Key != "MACH-5" || plan.Tickets[0].State != "in_progress" || plan.Tickets[0].Action != "create" ||
		plan.Tickets[0].Title != "MACH-5: Add archive page" {
		t.Fatalf("first ticket = %#v, want MACH-5 in_progress create", plan.Tickets[0])
	}
	if plan.Tickets[1].Key != "MACH-6" || plan.Tickets[1].Action != "skip" || plan.Tickets[1].Reason != "completed" {
		t.Fatalf("completed ticket = %#v, want skipped as completed", plan.Tickets[1])
	}
	if plan.Tickets[2].Key != "MACH-7" || plan.Tickets[2].State != "backlog" || plan.Tickets[2].Action != "create" {
		t.Fatalf("backlog ticket = %#v, want MACH-7 backlog create", plan.Tickets[2])
	}

	// Import: two rows land, the completed issue stays out of the table.
	importResponse, err := http.Post(web.URL+"/api/v1/import", "application/json",
		strings.NewReader(`{"source":"plane","project":"Platform"}`))
	if err != nil {
		t.Fatal(err)
	}
	var counts struct {
		Created int `json:"created"`
		Skipped int `json:"skipped"`
	}
	if err := json.NewDecoder(importResponse.Body).Decode(&counts); err != nil {
		t.Fatal(err)
	}
	importResponse.Body.Close()
	if importResponse.StatusCode != http.StatusOK || counts.Created != 2 || counts.Skipped != 2 {
		t.Fatalf("import = %d, %#v, want created 2 skipped 2", importResponse.StatusCode, counts)
	}
	for key, state := range map[string]string{"MACH-5": "in_progress", "MACH-7": "backlog"} {
		ticket, err := server.store.ticket(t.Context(), key)
		if err != nil {
			t.Fatalf("imported ticket %s: %v", key, err)
		}
		if ticket.State != state || ticket.Project != "Platform" || ticket.Repository != "platform" {
			t.Fatalf("ticket %s = %#v, want state %s, project Platform, repository platform", key, ticket, state)
		}
	}
	if _, err := server.store.ticket(t.Context(), "MACH-6"); err == nil {
		t.Fatal("completed issue was imported")
	}

	// A repeated import adds nothing and reports every planned ticket skipped.
	repeat, err := http.Post(web.URL+"/api/v1/import", "application/json",
		strings.NewReader(`{"source":"plane","project":"Platform"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(repeat.Body).Decode(&counts); err != nil {
		t.Fatal(err)
	}
	repeat.Body.Close()
	if counts.Created != 0 || counts.Skipped != 4 {
		t.Fatalf("repeat import = %#v, want created 0 skipped 4", counts)
	}
	// Pages are followed (page 2 adds MACH-8) and the walk terminates on the
	// first page that yields nothing new: page 4 is never requested.
	if len(seenPages) == 0 || !slices.Contains(seenPages, "2") || slices.Contains(seenPages, "4") {
		t.Fatalf("issue pages = %v, want pages 1..3 and never 4", seenPages)
	}
}

// TestImportLinearPreviewAndImport covers the Linear path: the GraphQL
// project roster and an import whose states map onto the tickets table.
func TestImportLinearPreviewAndImport(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	linear := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		queries = append(queries, payload.Query)
		mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		write := func(body string) { _, _ = response.Write([]byte(body)) }
		switch {
		case strings.Contains(payload.Query, "query Projects("):
			write(`{"data":{"projects":{"nodes":[{"id":"p1","name":"Platform"},{"id":"p2","name":"Unrelated"}],"pageInfo":{"hasNextPage":false}}}}`)
		case strings.Contains(payload.Query, "query Issues("):
			if payload.Variables["project"] != "Platform" {
				write(`{"errors":[{"message":"unexpected project"}]}`)
				return
			}
			write(`{"data":{"issues":{"nodes":[
				{"id":"uuid-12","identifier":"ENG-12","title":"Add import","description":"Spec.","state":{"name":"In Progress","type":"started"}},
				{"id":"uuid-13","identifier":"ENG-13","title":"Shipped","state":{"name":"Done","type":"completed"}},
				{"id":"uuid-14","identifier":"ENG-14","title":"Queued","state":{"name":"Todo","type":"unstarted"}}
			],"pageInfo":{"hasNextPage":false}}}}`)
		default:
			write(`{"errors":[{"message":"unexpected query"}]}`)
		}
	}))
	defer linear.Close()

	// The connection constructor is the test seam: bind it to the stub API.
	restore := newLinearImportClient
	newLinearImportClient = func(connection issues.LinearConnection) (*issues.LinearClient, error) {
		return issues.NewLinearClient(issues.LinearConfig{APIKey: connection.APIKey, Project: connection.Project, Endpoint: linear.URL})
	}
	t.Cleanup(func() { newLinearImportClient = restore })

	server, web := newImporterServer(t, `
[commands.plan]
executor = "test"
prompt_file = "plan.md"
timeout = "1m"
[issue_sources.linear.platform]
project = "Platform"
`)
	defer web.Close()
	t.Setenv("LINEAR_API_KEY", "key")

	response, err := http.Get(web.URL + "/api/v1/import/preview?source=linear")
	if err != nil {
		t.Fatal(err)
	}
	var roster struct {
		Projects []importProject `json:"projects"`
	}
	if err := json.NewDecoder(response.Body).Decode(&roster); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(roster.Projects) != 2 || roster.Projects[0].Name != "Platform" || roster.Projects[0].Repository != "platform" ||
		roster.Projects[1].Repository != "" {
		t.Fatalf("roster = %#v", roster.Projects)
	}

	importResponse, err := http.Post(web.URL+"/api/v1/import", "application/json",
		strings.NewReader(`{"source":"linear","project":"Platform"}`))
	if err != nil {
		t.Fatal(err)
	}
	var counts struct {
		Created int `json:"created"`
		Skipped int `json:"skipped"`
	}
	if err := json.NewDecoder(importResponse.Body).Decode(&counts); err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, importResponse.Body)
	importResponse.Body.Close()
	if importResponse.StatusCode != http.StatusOK || counts.Created != 2 || counts.Skipped != 1 {
		t.Fatalf("import = %d, %#v, want created 2 skipped 1", importResponse.StatusCode, counts)
	}
	ticket, err := server.store.ticket(t.Context(), "ENG-12")
	if err != nil || ticket.State != "in_progress" || ticket.Project != "Platform" || ticket.Repository != "platform" {
		t.Fatalf("ENG-12 = %#v, err = %v", ticket, err)
	}
	if ticket, err = server.store.ticket(t.Context(), "ENG-14"); err != nil || ticket.State != "backlog" {
		t.Fatalf("ENG-14 = %#v, err = %v", ticket, err)
	}
	if _, err = server.store.ticket(t.Context(), "ENG-13"); err == nil {
		t.Fatal("completed issue was imported")
	}
	for _, query := range queries {
		if strings.Contains(query, "filter") && !strings.Contains(query, "project: {name: {eq: $project}}") {
			t.Fatalf("issue query is not scoped to the project: %s", query)
		}
	}
}

// TestImportRejectsUnknownSourcesAndProjects pins the client-error mapping.
func TestImportRejectsUnknownSourcesAndProjects(t *testing.T) {
	_, web := newImporterServer(t, "[issue_sources.plane.platform]\nproject_id = \"proj\"\n")
	defer web.Close()

	response, err := http.Get(web.URL + "/api/v1/import/preview?source=github")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown source status = %d, want 400", response.StatusCode)
	}
	response, err = http.Get(web.URL + "/api/v1/import/preview?source=plane&project=Missing")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown project status = %d, want 400", response.StatusCode)
	}
}

// TestImportRouteAuth pins the auth contract of the new routes: reads need a
// valid credential, writes need a write-scope token or the browser CSRF
// defense.
func TestImportRouteAuth(t *testing.T) {
	_, web := newAuthTestServer(t, config.AuthTokens)
	defer web.Close()

	response := authRequest(t, http.MethodGet, web.URL+"/api/v1/import/preview?source=plane", "", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("preview without token status = %d, want 401", response.StatusCode)
	}
	response = authRequest(t, http.MethodGet, web.URL+"/api/v1/import/preview?source=plane", "mmt_bogus", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("preview with bogus token status = %d, want 401", response.StatusCode)
	}
	response = authRequest(t, http.MethodPost, web.URL+"/api/v1/import", "", map[string]string{"source": "plane", "project": "Platform"})
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("import without token status = %d, want 403", response.StatusCode)
	}
	response = authRequest(t, http.MethodPost, web.URL+"/api/v1/import", "mmt_bogus", map[string]string{"source": "plane", "project": "Platform"})
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("import with bogus token status = %d, want 401", response.StatusCode)
	}
}

// A remote project name that only case-insensitively matches its configured
// tracker key ("Omarchy Mobile" vs OMM) must import under the configured key:
// approval and the project views look the project up exactly.
func TestImportStampsConfiguredTrackerProject(t *testing.T) {
	definition := config.Config{Tracker: config.Tracker{Projects: map[string]config.TrackerProject{
		"OMM": {Repository: "mobile"},
	}}}
	result := importResult{Source: "plane", Project: "Omarchy Mobile", sourceRepository: "mobile", Tickets: []importTicket{{Key: "OMM-1", State: "backlog"}}}
	stampTrackerProject(definition, &result)
	if result.Project != "OMM" {
		t.Fatalf("project = %q, want OMM", result.Project)
	}
	// No covering entry: the remote name survives untouched.
	definition = config.Config{Tracker: config.Tracker{Projects: map[string]config.TrackerProject{}}}
	unrouted := importResult{Source: "plane", Project: "Omarchy Mobile", sourceRepository: "mobile", Tickets: []importTicket{{Key: "OMM-2", State: "backlog"}}}
	stampTrackerProject(definition, &unrouted)
	if unrouted.Project != "Omarchy Mobile" {
		t.Fatalf("project = %q, want the remote name", unrouted.Project)
	}
}
