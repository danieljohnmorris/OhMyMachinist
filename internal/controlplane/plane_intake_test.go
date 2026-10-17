package controlplane

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/plane"
)

// planeFixture is a fake Plane API that records every write it receives.
type planeFixture struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []planeRequest
	issue    string
}

func newPlaneIntakeFixture(t *testing.T) *planeFixture {
	t.Helper()
	fixture := &planeFixture{issue: planeIssueJSON("i1", "MACH", 9, "Todo", "unstarted", "factory")}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		fixture.mu.Lock()
		fixture.requests = append(fixture.requests, planeRequest{Method: request.Method, Path: request.URL.Path, Body: string(body)})
		issue := fixture.issue
		fixture.mu.Unlock()
		path := request.URL.Path
		write := func(payload string) {
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write([]byte(payload))
		}
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(path, "/projects/"):
			write(`{"results":[{"id":"p1","identifier":"MACH","name":"Machinist"}]}`)
		case request.Method == http.MethodGet && strings.HasSuffix(path, "/labels/"):
			write(`{"results":[{"id":"lbl-factory","name":"factory"},{"id":"lbl-approved","name":"approved"},{"id":"lbl-failed","name":"failed"}]}`)
		case request.Method == http.MethodGet && strings.HasSuffix(path, "/states/"):
			write(`{"results":[{"id":"st-todo","name":"Todo","group":"unstarted"},{"id":"st-done","name":"Done","group":"completed"}]}`)
		case request.Method == http.MethodGet && strings.Contains(path, "/issues/"):
			write(issue)
		case request.Method == http.MethodPost && strings.HasSuffix(path, "/comments/"):
			response.WriteHeader(http.StatusCreated)
		case request.Method == http.MethodPost && strings.HasSuffix(path, "/states/"):
			write(`{"id":"st-new","name":"new","group":"started"}`)
		case request.Method == http.MethodPost && strings.HasSuffix(path, "/labels/"):
			write(`{"id":"lbl-new","name":"new"}`)
		default:
			response.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *planeFixture) writes() []planeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	writes := []planeRequest{}
	for _, request := range f.requests {
		if request.Method != http.MethodGet {
			writes = append(writes, request)
		}
	}
	return writes
}

func (f *planeFixture) all() []planeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]planeRequest{}, f.requests...)
}

func planeIssueJSON(id, identifier string, sequence int64, stateName, stateGroup, label string) string {
	payload := map[string]any{
		"id": id, "name": "Fix the thing", "sequence_id": sequence, "project": "p1",
		"project_detail":   map[string]any{"identifier": identifier},
		"description_html": "<h2>Spec</h2><p>Do the thing</p>",
		"is_draft":         false,
		"labels":           []map[string]any{{"id": "lbl-" + label, "name": label}},
		"state":            map[string]any{"id": "st-" + strings.ToLower(stateName), "name": stateName, "group": stateGroup},
	}
	body, _ := json.Marshal(payload)
	return string(body)
}

func planeEnvelope(t *testing.T, issue string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"event": "issue", "action": "create", "data": json.RawMessage(issue)})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func signPlane(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// newIntakeFixture builds an intake driver pointed at the fake Plane API with
// shadow disabled so the tests can observe real side effects.
func newIntakeFixture(t *testing.T, shadow bool) (*PlaneIntake, *Store, *planeFixture) {
	t.Helper()
	fixture := newPlaneIntakeFixture(t)
	directory := t.TempDir()
	definitionPath := filepath.Join(directory, "config.toml")
	definition := "[commands.build-pr]\nexecutor = \"test\"\ntimeout = \"1m\"\n\n" +
		"[plane]\nurl = \"" + fixture.server.URL + "\"\nworkspace = \"ws\"\nwebhook_secret = \"s3cret\"\nui_url = \"http://ui\"\n" +
		"shadow = " + boolLiteral(shadow) + "\nrepositories = { p1 = \"OhMyMachinist\" }\nidentifiers = { p1 = \"MACH\" }\n"
	if err := os.WriteFile(definitionPath, []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PLANE_API_KEY", "key")
	t.Setenv("MACHINIST_HOME", filepath.Join(directory, "home"))
	store := openTestStore(t, filepath.Join(directory, "machinist.db"))
	intake, err := NewPlaneIntake(store, definitionPath, filepath.Join(directory, "home"))
	if err != nil {
		t.Fatal(err)
	}
	intake.SetCommandResolver(func(name string) (config.ResolvedCommand, error) {
		return testAgent(name, "prompt"), nil
	})
	return intake, store, fixture
}

func boolLiteral(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

// The Plane base URL must come from config or the environment; with neither
// set the neutral placeholder default applies and no personal host leaks in.
func TestPlaneBaseComesFromConfigOrEnv(t *testing.T) {
	directory := t.TempDir()
	definitionPath := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(definitionPath, []byte("[commands.build-pr]\nexecutor = \"test\"\ntimeout = \"1m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MACHINIST_HOME", filepath.Join(directory, "home"))
	store := openTestStore(t, filepath.Join(directory, "machinist.db"))
	intake, err := NewPlaneIntake(store, definitionPath, filepath.Join(directory, "home"))
	if err != nil {
		t.Fatal(err)
	}
	url, workspace := intake.PlaneBase(context.Background())
	if url != "https://plane.example.com" {
		t.Fatalf("default plane URL = %q, want the neutral placeholder", url)
	}
	if workspace != plane.DefaultWorkspace {
		t.Fatalf("default workspace = %q, want %q", workspace, plane.DefaultWorkspace)
	}

	t.Setenv("PLANE_URL", "https://plane.example.test")
	url, _ = intake.PlaneBase(context.Background())
	if url != "https://plane.example.test" {
		t.Fatalf("PLANE_URL override = %q, want https://plane.example.test", url)
	}
}

func TestPlaneIntakeCreatesJobAndDedupes(t *testing.T) {
	intake, store, fixture := newIntakeFixture(t, false)
	body := planeEnvelope(t, fixture.issue)
	accepted, err := intake.HandleWebhook(t.Context(), body, signPlane("s3cret", body), "delivery-1")
	if err != nil || !accepted {
		t.Fatalf("webhook accepted=%v err=%v", accepted, err)
	}
	snapshot, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(snapshot.Jobs))
	}
	job := snapshot.Jobs[0]
	if job.IssueKey != "MACH-9" || job.Title != "MACH-9: Fix the thing" || job.State != "queued" {
		t.Fatalf("job = %#v", job)
	}
	if job.Metadata == nil || job.Metadata.IntakeSource != "webhook:delivery-1" || job.Metadata.IntakeSpecHash == "" {
		t.Fatalf("job metadata = %#v", job.Metadata)
	}
	if len(fixture.writes()) != 1 || !strings.HasSuffix(fixture.writes()[0].Path, "/comments/") {
		t.Fatalf("expected one start comment, got %#v", fixture.writes())
	}

	// Same spec again: no new job, no new comment.
	if _, err := intake.HandleWebhook(t.Context(), body, signPlane("s3cret", body), "delivery-2"); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = store.Snapshot(t.Context())
	if len(snapshot.Jobs) != 1 {
		t.Fatalf("repeat delivery created %d jobs", len(snapshot.Jobs))
	}
	if len(fixture.writes()) != 1 {
		t.Fatalf("repeat delivery posted a comment: %#v", fixture.writes())
	}
}

func TestPlaneIntakeUpdatesQueuedJobAndIgnoresRunning(t *testing.T) {
	intake, store, fixture := newIntakeFixture(t, false)
	body := planeEnvelope(t, fixture.issue)
	if _, err := intake.HandleWebhook(t.Context(), body, signPlane("s3cret", body), "d1"); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Snapshot(t.Context())
	jobID := snapshot.Jobs[0].ID

	// A spec edit while the job is queued rewrites the queued job in place.
	edited := planeEnvelope(t, planeIssueJSON("i1", "MACH", 9, "Todo", "unstarted", "factory"))
	editedIssue := strings.Replace(string(edited), "Do the thing", "Do the other thing", 1)
	if _, err := intake.HandleWebhook(t.Context(), []byte(editedIssue), signPlane("s3cret", []byte(editedIssue)), "d2"); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = store.Snapshot(t.Context())
	if len(snapshot.Jobs) != 1 || snapshot.Jobs[0].ID != jobID {
		t.Fatalf("queued edit created a new job: %#v", snapshot.Jobs)
	}
	job := jobByID(t, store, jobID)
	if !strings.Contains(job.Prompt, "Do the other thing") {
		t.Fatalf("queued job prompt not updated: %q", job.Prompt)
	}

	// An edit while running is ignored.
	if _, err := store.db.Exec(`UPDATE jobs SET state='running' WHERE id=?`, jobID); err != nil {
		t.Fatal(err)
	}
	edited2 := strings.Replace(string(edited), "Do the thing", "Do nothing", 1)
	if _, err := intake.HandleWebhook(t.Context(), []byte(edited2), signPlane("s3cret", []byte(edited2)), "d3"); err != nil {
		t.Fatal(err)
	}
	snapshot, _ = store.Snapshot(t.Context())
	if len(snapshot.Jobs) != 1 {
		t.Fatalf("running edit created a new job: %d", len(snapshot.Jobs))
	}
	job = jobByID(t, store, jobID)
	if strings.Contains(job.Prompt, "Do nothing") {
		t.Fatalf("running job prompt was rewritten: %q", job.Prompt)
	}
}

func TestPlaneIntakeShadowModeHasNoSideEffects(t *testing.T) {
	intake, store, fixture := newIntakeFixture(t, true)
	body := planeEnvelope(t, fixture.issue)
	if _, err := intake.HandleWebhook(t.Context(), body, signPlane("s3cret", body), "d1"); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Snapshot(t.Context())
	if len(snapshot.Jobs) != 0 {
		t.Fatalf("shadow mode created %d jobs", len(snapshot.Jobs))
	}
	if writes := fixture.writes(); len(writes) != 0 {
		t.Fatalf("shadow mode touched Plane: %#v", writes)
	}
	records, err := store.PlaneIntakeAll(t.Context())
	if err != nil || len(records) != 0 {
		t.Fatalf("shadow mode stored intake state: %#v, %v", records, err)
	}
	decisions, err := store.PlaneShadowDecisions(t.Context(), 10)
	if err != nil || len(decisions) == 0 {
		t.Fatalf("shadow mode recorded no decision: %#v, %v", decisions, err)
	}
	found := false
	for _, decision := range decisions {
		if decision.Kind == "job" && decision.Key == "MACH-9" {
			found = true
		}
	}
	if !found {
		t.Fatalf("shadow decisions = %#v", decisions)
	}
}

func TestPlaneIntakeApproveBridge(t *testing.T) {
	intake, _, fixture := newIntakeFixture(t, false)
	fixture.mu.Lock()
	fixture.issue = planeIssueJSON("i1", "MACH", 9, "Todo", "unstarted", "approved")
	fixture.mu.Unlock()
	body := planeEnvelope(t, fixture.issue)
	if _, err := intake.HandleWebhook(t.Context(), body, signPlane("s3cret", body), "d1"); err != nil {
		t.Fatal(err)
	}
	patched := false
	for _, request := range fixture.writes() {
		if request.Method == http.MethodPatch && strings.Contains(request.Body, "lbl-factory") {
			patched = true
		}
	}
	if !patched {
		t.Fatalf("approve bridge did not add the factory label: %#v", fixture.writes())
	}
}

func TestPlaneIntakeRejectsBadSignature(t *testing.T) {
	intake, _, fixture := newIntakeFixture(t, false)
	body := planeEnvelope(t, fixture.issue)
	if _, err := intake.HandleWebhook(t.Context(), body, "deadbeef", "d1"); err == nil {
		t.Fatal("bad signature must be rejected")
	}
	if _, err := intake.HandleWebhook(t.Context(), body, signPlane("wrong", body), "d1"); err == nil {
		t.Fatal("signature from another secret must be rejected")
	}
}

func TestPlaneIntakeImportExistingStateFiles(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, filepath.Join(directory, "plane-repos.json"), `{"_identifiers":{"p1":"MACH"},"p1":"OhMyMachinist"}`)
	writeFile(t, filepath.Join(directory, "plane-supervisors.json"), `{"OMM":"Om","_default":"LocalAI"}`)
	writeFile(t, filepath.Join(directory, "plane-jobs", ".seen.json"), `{"i1":"abcd1234"}`)
	writeFile(t, filepath.Join(directory, "plane-jobs", ".state-sync.json"), `{"job_x":{"start":"start: no change","end":"review: patched","notify":"queued LocalAI"}}`)
	writeFile(t, filepath.Join(directory, "alerts", ".plane-states.json"), `{"i1":{"id":"st-done","name":"Done","group":"completed"}}`)
	writeFile(t, filepath.Join(directory, "plane-jobs", "MACH-1-abcd1234", "meta.json"), `{"key":"MACH-1","issue_id":"i1","hash":"abcd1234","job_id":"job_x","source":"webhook"}`)

	store := openTestStore(t, filepath.Join(directory, "machinist.db"))
	imported, err := store.ImportPlaneState(t.Context(), directory)
	if err != nil || !imported {
		t.Fatalf("import = %v, %v", imported, err)
	}
	settings, err := store.PlaneSettings(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(settings["plane.repositories"], "OhMyMachinist") || !strings.Contains(settings["plane.identifiers"], "MACH") {
		t.Fatalf("repo settings = %#v", settings)
	}
	if !strings.Contains(settings["plane.supervisors"], "OMM") {
		t.Fatalf("supervisor settings = %#v", settings)
	}
	intakeRecords, err := store.PlaneIntakeAll(t.Context())
	if err != nil || intakeRecords["i1"].SpecHash != "abcd1234" {
		t.Fatalf("intake records = %#v, %v", intakeRecords, err)
	}
	syncRecords, err := store.PlaneSyncAll(t.Context())
	if err != nil || syncRecords["job_x"].Started != "start: no change" || syncRecords["job_x"].Notified != "queued LocalAI" {
		t.Fatalf("sync records = %#v, %v", syncRecords, err)
	}
	states, err := store.PlaneAlertStates(t.Context())
	if err != nil || states["i1"].StateID != "st-done" {
		t.Fatalf("alert states = %#v, %v", states, err)
	}
	scriptJobs, err := store.PlaneScriptJobs(t.Context())
	if err != nil || len(scriptJobs) != 1 || scriptJobs[0].Key != "MACH-1" {
		t.Fatalf("script jobs = %#v, %v", scriptJobs, err)
	}
}

func TestPlaneIntakeWebhookEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("MACHINIST_HOME", home)
	t.Setenv("PLANE_WEBHOOK_SECRET", "s3cret")
	t.Setenv("PLANE_API_KEY", "key")
	fixture := newPlaneIntakeFixture(t)
	t.Setenv("PLANE_URL", fixture.server.URL)
	t.Setenv("PLANE_WORKSPACE", "ws")

	directory := t.TempDir()
	definitionPath := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(definitionPath, []byte("[commands.plan]\nexecutor = \"test\"\ntimeout = \"1m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, filepath.Join(directory, "machinist.db"))
	if err := store.PutSetting(t.Context(), "plane.shadow", "0"); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSetting(t.Context(), "plane.repositories", `{"p1":"OhMyMachinist"}`); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSetting(t.Context(), "plane.command", "plan"); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(store, definitionPath, "secret", 0, config.AuthTokens)
	if err != nil {
		t.Fatal(err)
	}
	web := httptest.NewServer(server.Handler())
	t.Cleanup(web.Close)

	body := planeEnvelope(t, fixture.issue)
	request, err := http.NewRequest(http.MethodPost, web.URL+"/api/v1/webhooks/issues", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Plane-Signature", signPlane("s3cret", body))
	request.Header.Set("X-Plane-Delivery", "delivery-9")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", response.StatusCode)
	}
	snapshot, _ := store.Snapshot(t.Context())
	if len(snapshot.Jobs) != 1 || snapshot.Jobs[0].IssueKey != "MACH-9" {
		t.Fatalf("jobs = %#v", snapshot.Jobs)
	}

	bad, _ := http.NewRequest(http.MethodPost, web.URL+"/api/v1/webhooks/issues", strings.NewReader(string(body)))
	bad.Header.Set("X-Plane-Signature", "deadbeef")
	badResponse, err := http.DefaultClient.Do(bad)
	if err != nil {
		t.Fatal(err)
	}
	badResponse.Body.Close()
	if badResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad signature status = %d, want 401", badResponse.StatusCode)
	}
}

func TestPlaneIntakeStateSyncNeverTouchesDoneTickets(t *testing.T) {
	intake, store, fixture := newIntakeFixture(t, false)
	// A finished job whose ticket already sits in Done: the review sync must
	// not move the ticket.
	jobID, err := store.CreatePlaneJob(t.Context(), PlaneAdmission{
		Prompt: "p", Title: "MACH-9: Fix", Repository: "OhMyMachinist",
		Command: testAgent("build-pr", "p"), IssueKey: "MACH-9", Subject: "https://plane/ws/browse/MACH-9/",
		Source: "webhook:d1", SpecHash: "hash",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO plane_intake(issue_id,spec_hash,key,project_id,job_id,source,delivery_id,updated_at) VALUES('i1','hash','MACH-9','p1',?,'webhook:d1','d1',?)`, jobID, "2026-10-11T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE runs SET state='succeeded',started_at='2026-10-11T00:00:00Z',events=? WHERE job_id=?`, `{"type":"process.output","data":"deliver-pr: PR: https://github.com/dan/repo/pull/3"}`, jobID); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	fixture.issue = planeIssueJSON("i1", "MACH", 9, "Done", "completed", "factory")
	fixture.mu.Unlock()

	if err := intake.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, request := range fixture.writes() {
		if request.Method == http.MethodPatch && strings.Contains(request.Body, `"state"`) {
			t.Fatalf("sync moved a Done ticket: %#v", request)
		}
	}
	// The PR-opened alert is still written for the supervisor.
	pending := filepath.Join(intake.homeDirectory, "alerts", "pending")
	entries, err := os.ReadDir(pending)
	if err != nil {
		t.Fatalf("read alert spool: %v", err)
	}
	foundAlert := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "pr-MACH-9-") {
			foundAlert = true
		}
	}
	if !foundAlert {
		t.Fatalf("PR alert not written: %#v", entries)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
