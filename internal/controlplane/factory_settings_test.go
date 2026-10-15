package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/adapters"
	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/issues"
)

func testFactoryServer(t *testing.T, definitionPath string) (*Server, *Store) {
	t.Helper()
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	return &Server{store: store, definitionPath: definitionPath, now: time.Now}, store
}

func TestFactorySettingPrecedence(t *testing.T) {
	directory := t.TempDir()
	definitionPath := filepath.Join(directory, "config.toml")
	body := "[factory.projects.machinist]\ntracker = \"plane\"\n"
	if err := writeFile(definitionPath, body); err != nil {
		t.Fatal(err)
	}
	server, store := testFactoryServer(t, definitionPath)
	ctx := t.Context()

	// Config default is the last resort.
	if setting, source, err := server.resolveFactorySetting(ctx, "machinist"); err != nil {
		t.Fatal(err)
	} else if source != "config project" || setting.Tracker != adapters.TrackerPlane {
		t.Fatalf("config project = %q %+v", source, setting)
	}

	// Global Settings beats the config project layer.
	if err := store.SaveSetting(ctx, settingFactoryDefault, `{"tracker":"linear"}`); err != nil {
		t.Fatal(err)
	}
	if setting, source, err := server.resolveFactorySetting(ctx, "machinist"); err != nil {
		t.Fatal(err)
	} else if source != "global" || setting.Tracker != adapters.TrackerLinear {
		t.Fatalf("global = %q %+v", source, setting)
	}

	// Per-project Settings beats the global default.
	if err := store.SaveProjectFactorySetting(ctx, "machinist", adapters.FactorySetting{
		CodingAgent: adapters.AgentSpec{Kind: adapters.AgentOMP, Executor: "omp"},
	}); err != nil {
		t.Fatal(err)
	}
	if setting, source, err := server.resolveFactorySetting(ctx, "machinist"); err != nil {
		t.Fatal(err)
	} else if source != "project" || setting.CodingAgent.Executor != "omp" {
		t.Fatalf("project = %q %+v", source, setting)
	}

	// Deleting the project override falls back to the global default.
	if err := store.DeleteProjectFactorySetting(ctx, "machinist"); err != nil {
		t.Fatal(err)
	}
	if _, source, err := server.resolveFactorySetting(ctx, "machinist"); err != nil {
		t.Fatal(err)
	} else if source != "global" {
		t.Fatalf("after delete source = %q, want global", source)
	}
}

func TestAdmitIssueUsesProjectCodingAgentAndModel(t *testing.T) {
	server, store := testFactoryServer(t, "")
	server.issueSources = map[string]issuePoller{
		"issue/platform": fakeIssuePoller{issues: []issues.Issue{{
			Key: "MACH-7", Title: "Add archive page", Description: "Build it.",
			URL: "https://plane.example.com/issues/MACH-7", Labels: []string{"factory"},
		}}},
	}
	if err := store.SaveProjectFactorySetting(t.Context(), "machinist", adapters.FactorySetting{
		CodingAgent: adapters.AgentSpec{Kind: adapters.AgentOMP, Executor: "omp"},
		Models:      map[string]adapters.ModelSpec{adapters.RoleCoding: {Model: "glm-5", Endpoint: adapters.EndpointCloud}},
	}); err != nil {
		t.Fatal(err)
	}
	trigger := config.ResolvedTrigger{
		Identity: "issue/platform", Family: "issue", Name: "platform", Repository: "machinist",
		Signature: "signature", Label: "factory", SourceKind: "plane",
		Command: testAgent("build", "{{machinist.prompt}}"), Model: "codex-default",
	}
	if err := store.SyncTriggers(t.Context(), []TriggerDefinition{{
		Identity: trigger.Identity, Family: trigger.Family, ConfigSignature: trigger.Signature,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := server.processIssueTrigger(t.Context(), trigger, mustTriggerGeneration(t, store, trigger.Identity)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Jobs) != 1 || len(snapshot.Jobs[0].Runs) != 1 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	run := snapshot.Jobs[0].Runs[0]
	if run.Executor != "omp" || run.Model != "glm-5" {
		t.Fatalf("run executor/model = %q/%q, want omp/glm-5", run.Executor, run.Model)
	}
}

// linearFixture serves the Linear GraphQL issues query with one labelled issue.
func linearFixture(t *testing.T, seen *[]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		*seen = append(*seen, string(body))
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"data":{"issues":{"nodes":[{"id":"uuid-9","identifier":"LIN-42","title":"Ship linear intake","description":"Spec","url":"https://linear.app/acme/issue/LIN-42","labels":[{"name":"factory"}],"team":{"id":"team-1"}}]}}}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func TestIssueTriggerUsesProjectLinearTracker(t *testing.T) {
	server, store := testFactoryServer(t, "")
	var seen []string
	fixture := linearFixture(t, &seen)
	client, err := issues.NewLinearClient(issues.LinearConfig{APIKey: "lin-key", Project: "Platform", Endpoint: fixture.URL})
	if err != nil {
		t.Fatal(err)
	}
	server.issueSources = map[string]issuePoller{"issue/platform": fakeIssuePoller{}}
	server.trackerFactory = func(context.Context, string, string) (adapters.Tracker, error) {
		return adapters.LinearTracker{Client: client}, nil
	}
	if err := store.SaveProjectFactorySetting(t.Context(), "machinist", adapters.FactorySetting{Tracker: adapters.TrackerLinear}); err != nil {
		t.Fatal(err)
	}
	trigger := config.ResolvedTrigger{
		Identity: "issue/platform", Family: "issue", Name: "platform", Repository: "machinist",
		Signature: "signature", Label: "factory", SourceKind: "plane",
		Command: testAgent("build", "{{machinist.prompt}}"),
	}
	if err := store.SyncTriggers(t.Context(), []TriggerDefinition{{
		Identity: trigger.Identity, Family: trigger.Family, ConfigSignature: trigger.Signature,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := server.processIssueTrigger(t.Context(), trigger, mustTriggerGeneration(t, store, trigger.Identity)); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 {
		t.Fatal("Linear GraphQL endpoint was not called")
	}
	if !bytes.Contains([]byte(seen[0]), []byte("factory")) || !bytes.Contains([]byte(seen[0]), []byte("Platform")) {
		t.Fatalf("Linear intake query = %s, want label and project filters", seen[0])
	}
	snapshot, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Jobs) != 1 || snapshot.Jobs[0].Title != "LIN-42: Ship linear intake" {
		t.Fatalf("snapshot = %#v, want one Linear ticket job", snapshot)
	}
}

func TestWorkerFactorySettingRendersReviewExecutor(t *testing.T) {
	server, store := testFactoryServer(t, "")
	if err := store.SaveProjectFactorySetting(t.Context(), "machinist", adapters.FactorySetting{
		CodingAgent: adapters.AgentSpec{Kind: adapters.AgentOMP, Executor: "omp"},
		ReviewAgent: adapters.AgentSpec{Kind: adapters.AgentCodex},
		Models: map[string]adapters.ModelSpec{
			adapters.RoleCoding: {Model: "glm-5"},
			adapters.RoleReview: {Model: "gpt-5", Effort: "high"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := server.workerFactorySetting(t.Context(), "machinist")
	if err != nil {
		t.Fatal(err)
	}
	if result.ReviewExecutor != "codex exec --model gpt-5" {
		t.Fatalf("review executor = %q, want rendered codex command", result.ReviewExecutor)
	}
	if result.CodingExecutor != "omp" || result.CodingModel != "glm-5" {
		t.Fatalf("coding = %q/%q, want omp/glm-5", result.CodingExecutor, result.CodingModel)
	}
}

func TestFactorySettingHTTPRoundTrip(t *testing.T) {
	server, _ := testFactoryServer(t, "")
	body, _ := json.Marshal(map[string]any{
		"project": "machinist",
		"setting": map[string]any{
			"tracker":      "linear",
			"coding_agent": map[string]string{"kind": "omp", "executor": "omp"},
			"models":       map[string]any{"coding": map[string]string{"model": "glm-5"}},
		},
	})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/factory-setting", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	server.saveFactorySetting(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var saved factorySettingResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Setting.Tracker != adapters.TrackerLinear || saved.Setting.CodingAgent.Executor != "omp" {
		t.Fatalf("saved = %+v", saved.Setting)
	}

	get := httptest.NewRequest(http.MethodGet, "/api/v1/factory-setting?project=machinist", nil)
	read := httptest.NewRecorder()
	server.getFactorySetting(read, get)
	var resolved factorySettingResponse
	if err := json.Unmarshal(read.Body.Bytes(), &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.Source != "project" || resolved.Setting.Models[adapters.RoleCoding].Model != "glm-5" {
		t.Fatalf("resolved = %+v (source %q)", resolved.Setting, resolved.Source)
	}

	del := httptest.NewRequest(http.MethodDelete, "/api/v1/factory-setting?project=machinist", nil)
	removed := httptest.NewRecorder()
	server.deleteFactorySetting(removed, del)
	if removed.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", removed.Code, removed.Body.String())
	}
	var after factorySettingResponse
	if err := json.Unmarshal(removed.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if after.Source != "built-in" || !after.Setting.IsZero() {
		t.Fatalf("after delete = %+v (source %q)", after.Setting, after.Source)
	}
}

func TestFactorySettingHTTPRejectsUnknownAdapter(t *testing.T) {
	server, _ := testFactoryServer(t, "")
	body, _ := json.Marshal(map[string]any{
		"project": "machinist",
		"setting": map[string]any{"tracker": "jira"},
	})
	request := httptest.NewRequest(http.MethodPut, "/api/v1/factory-setting", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	server.saveFactorySetting(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
