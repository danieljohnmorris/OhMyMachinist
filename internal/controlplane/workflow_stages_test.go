package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/owainlewis/machinist/internal/config"
)

func TestDeriveStageWebPRLifecycle(t *testing.T) {
	jobType, _ := DefaultWorkflowConfig().Type("web_pr")
	stageIDs := func(progress StageProgress) []string {
		ids := make([]string, 0, len(progress.Stages))
		for _, stage := range progress.Stages {
			ids = append(ids, stage.ID)
		}
		return ids
	}
	cases := []struct {
		name      string
		input     StageInput
		current   string
		round     int
		attention bool
	}{
		{name: "queued", input: StageInput{State: "queued"}, current: "queued"},
		{name: "planning", input: StageInput{State: "running", Stage: "planning"}, current: "planning"},
		{name: "coding", input: StageInput{State: "running", Stage: "coding"}, current: "coding"},
		{name: "review round two", input: StageInput{State: "running", Stage: "review", ReviewRound: 2}, current: "review", round: 2},
		{name: "review exhausted", input: StageInput{State: "blocked", Stage: "review", ReviewRound: 3}, current: "review", round: 3, attention: true},
		{name: "review over budget needs attention", input: StageInput{State: "running", Stage: "review", ReviewRound: 4}, current: "review", round: 4, attention: true},
		// A succeeded PR job waits in UAT: it is not finished until merge/Plane Done.
		{name: "succeeded awaits UAT", input: StageInput{State: "succeeded"}, current: "uat"},
		{name: "resolved is finished", input: StageInput{State: "resolved", ResolutionKind: "pr"}, current: "finished"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			progress := DeriveStage(jobType, testCase.input)
			if progress.Current != testCase.current {
				t.Fatalf("current = %q, want %q", progress.Current, testCase.current)
			}
			if progress.Round != testCase.round {
				t.Fatalf("round = %d, want %d", progress.Round, testCase.round)
			}
			if progress.Attention != testCase.attention {
				t.Fatalf("attention = %v, want %v", progress.Attention, testCase.attention)
			}
			if len(progress.Stages) != 6 {
				t.Fatalf("stages = %v", stageIDs(progress))
			}
			// Everything before the current stage is ticked.
			for i, stage := range progress.Stages {
				if stage.ID == testCase.current {
					break
				}
				if stage.State != "complete" {
					t.Fatalf("stage %q = %q, want complete", stage.ID, stage.State)
				}
				_ = i
			}
		})
	}
}

func TestDeriveStageConfigOpsHasNoReview(t *testing.T) {
	jobType, ok := DefaultWorkflowConfig().Type("config_ops")
	if !ok {
		t.Fatal("config_ops template missing")
	}
	progress := DeriveStage(jobType, StageInput{State: "succeeded"})
	if progress.Current != "approve" {
		t.Fatalf("succeeded current = %q, want approve", progress.Current)
	}
	ids := make([]string, 0, len(progress.Stages))
	for _, stage := range progress.Stages {
		ids = append(ids, stage.ID)
		if stage.ID == "review" || stage.ID == "uat" {
			t.Fatalf("config/ops strip must not contain %q: %v", stage.ID, ids)
		}
	}
	want := []string{"queued", "planning", "apply", "verify", "approve", "finished"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("strip = %v, want %v", ids, want)
	}
	finished := DeriveStage(jobType, StageInput{State: "resolved"})
	if finished.Current != "finished" {
		t.Fatalf("resolved current = %q, want finished", finished.Current)
	}
}

func TestResolveJobTypeFromLabel(t *testing.T) {
	config := DefaultWorkflowConfig()
	jobType, err := ResolveJobType(config, nil, "")
	if err != nil || jobType.Name != "web_pr" {
		t.Fatalf("default = %q, %v", jobType.Name, err)
	}
	jobType, err = ResolveJobType(config, []string{"bug", "type:config"}, "")
	if err != nil || jobType.Name != "config_ops" {
		t.Fatalf("label type = %q, %v", jobType.Name, err)
	}
	jobType, err = ResolveJobType(config, []string{"type:config"}, "web_pr")
	if err != nil || jobType.Name != "web_pr" {
		t.Fatalf("explicit override = %q, %v", jobType.Name, err)
	}
	// An unknown explicit value falls back to the default rather than erroring.
	jobType, err = ResolveJobType(config, nil, "does_not_exist")
	if err != nil || jobType.Name != "web_pr" {
		t.Fatalf("unknown explicit = %q, %v", jobType.Name, err)
	}
}

func TestValidateWorkflowConfig(t *testing.T) {
	valid := DefaultWorkflowConfig()
	if err := ValidateWorkflowConfig(NormalizeWorkflowConfig(valid)); err != nil {
		t.Fatalf("shipped config invalid: %v", err)
	}
	cases := map[string]func(c *WorkflowConfig){
		"empty stages":       func(c *WorkflowConfig) { c.Types[0].Stages = nil },
		"duplicate type":     func(c *WorkflowConfig) { c.Types = append(c.Types, c.Types[0]) },
		"unknown kind":       func(c *WorkflowConfig) { c.Types[0].Stages[0].Kind = "teleport" },
		"bad session":        func(c *WorkflowConfig) { c.Types[0].Stages[1].Session = "sometimes" },
		"unknown default":    func(c *WorkflowConfig) { c.DefaultType = "nope" },
		"unknown label":      func(c *WorkflowConfig) { c.LabelMappings["type:ops"] = "nope" },
		"duplicate stage id": func(c *WorkflowConfig) { c.Types[0].Stages[1].ID = c.Types[0].Stages[0].ID },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			config := DefaultWorkflowConfig()
			mutate(&config)
			if err := ValidateWorkflowConfig(NormalizeWorkflowConfig(config)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestNormalizeWorkflowConfigFillsDefaults(t *testing.T) {
	normalized := NormalizeWorkflowConfig(WorkflowConfig{Types: []JobTypeDefinition{{
		Name: "custom",
		Stages: []StageDefinition{
			{ID: "start", Kind: StageKindQueued},
			{ID: "check", Name: "Check", Kind: StageKindReview},
		},
	}}})
	if normalized.DefaultType != "custom" {
		t.Fatalf("default = %q", normalized.DefaultType)
	}
	if normalized.Types[0].Stages[0].Session != StageSessionContinue || normalized.Types[0].Stages[1].Session != StageSessionFresh {
		t.Fatalf("session defaults = %+v", normalized.Types[0].Stages)
	}
	if normalized.Types[0].Stages[0].Name != "start" {
		t.Fatalf("name fallback = %q", normalized.Types[0].Stages[0].Name)
	}
}

// setJobMetadataForTest writes normalized job metadata through the production
// path so stage/job-type fields are validated the same way intake validates them.
func setJobMetadataForTest(t *testing.T, store *Store, jobID string, raw map[string]any) {
	t.Helper()
	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveJobMetadata(context.Background(), tx, jobID, raw); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreProjectsJobStage(t *testing.T) {
	store := openTestStore(t, t.TempDir()+"/machinist.db")
	// config_ops card: no PR review stage.
	opsID, err := store.CreateJobWithOptions(t.Context(), "Apply config.", "sparkdash", "plan", config.ResolvedCommand{Name: "plan"}, CreateJobOptions{Title: "SPARK-1: config", JobType: "config_ops"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET state='running' WHERE id=?`, opsID); err != nil {
		t.Fatal(err)
	}
	// web_pr card mid review, round 2.
	prID, err := store.CreateJobWithOptions(t.Context(), "Add page.", "sparkdash", "plan", config.ResolvedCommand{Name: "plan"}, CreateJobOptions{Title: "SPARK-2: page"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET state='running' WHERE id=?`, prID); err != nil {
		t.Fatal(err)
	}
	setJobMetadataForTest(t, store, prID, map[string]any{"stage": "review", "review_round": 2})

	ops := jobByID(t, store, opsID)
	if ops.JobType != "config_ops" || ops.Stage == nil || ops.Stage.JobType != "config_ops" {
		t.Fatalf("ops job = %+v", ops.Stage)
	}
	for _, stage := range ops.Stage.Stages {
		if stage.ID == "review" {
			t.Fatalf("ops strip contains review: %+v", ops.Stage.Stages)
		}
	}
	pr := jobByID(t, store, prID)
	if pr.JobType != "web_pr" || pr.Stage == nil || pr.Stage.Current != "review" || pr.Stage.Round != 2 {
		t.Fatalf("pr stage = %+v", pr.Stage)
	}

	// A succeeded PR job waits in UAT; only resolution finishes it.
	if _, err := store.db.Exec(`UPDATE jobs SET state='succeeded' WHERE id=?`, prID); err != nil {
		t.Fatal(err)
	}
	if pr = jobByID(t, store, prID); pr.Stage.Current != "uat" {
		t.Fatalf("succeeded stage = %+v", pr.Stage)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET state='resolved', resolution_kind='pr' WHERE id=?`, prID); err != nil {
		t.Fatal(err)
	}
	if pr = jobByID(t, store, prID); pr.Stage.Current != "finished" {
		t.Fatalf("resolved stage = %+v", pr.Stage)
	}
}

func TestStoreProjectWorkflowConfigRoundTrip(t *testing.T) {
	store := openTestStore(t, t.TempDir()+"/machinist.db")
	// Unset projects return the shipped default.
	config, err := store.ProjectWorkflowConfig(t.Context(), "sparkdash")
	if err != nil {
		t.Fatal(err)
	}
	if config.DefaultType != "web_pr" || len(config.Types) != 2 {
		t.Fatalf("default config = %+v", config)
	}
	custom := WorkflowConfig{
		DefaultType:   "docs",
		LabelMappings: map[string]string{"type:docs": "docs"},
		Types: []JobTypeDefinition{{
			Name: "docs",
			Stages: []StageDefinition{
				{ID: "queued", Kind: StageKindQueued},
				{ID: "write", Name: "Write", Kind: StageKindCoding},
				{ID: "done", Name: "Done", Kind: StageKindFinished},
			},
		}},
	}
	saved, err := store.SaveProjectWorkflowConfig(t.Context(), "sparkdash", custom)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Types[0].Stages[1].Session != StageSessionContinue {
		t.Fatalf("session default not applied: %+v", saved.Types[0].Stages)
	}
	read, err := store.ProjectWorkflowConfig(t.Context(), "sparkdash")
	if err != nil {
		t.Fatal(err)
	}
	if read.DefaultType != "docs" || len(read.Types) != 1 {
		t.Fatalf("round trip = %+v", read)
	}
	// Invalid configs are rejected, not stored.
	if _, err := store.SaveProjectWorkflowConfig(t.Context(), "sparkdash", WorkflowConfig{Types: []JobTypeDefinition{{Name: "bad", Stages: []StageDefinition{{ID: "x", Kind: "nope"}}}}}); err == nil {
		t.Fatal("expected invalid config to be rejected")
	}
	if err := store.DeleteProjectWorkflowConfig(t.Context(), "sparkdash"); err != nil {
		t.Fatal(err)
	}
	if read, _ = store.ProjectWorkflowConfig(t.Context(), "sparkdash"); read.DefaultType != "web_pr" {
		t.Fatalf("delete did not reset: %+v", read)
	}
}

func TestWorkflowConfigHTTP(t *testing.T) {
	server, web := newTestHTTPServer(t)
	defer web.Close()
	_, readSecret, err := server.store.CreateToken(t.Context(), "reader", TokenScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	_, writeSecret, err := server.store.CreateToken(t.Context(), "writer", TokenScopeWrite)
	if err != nil {
		t.Fatal(err)
	}

	// Default config is served read-only until a project saves its own.
	response := authRequest(t, http.MethodGet, web.URL+"/api/v1/workflows?project=machinist", readSecret, nil)
	var payload workflowConfigResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if !payload.Default || payload.Config.DefaultType != "web_pr" {
		t.Fatalf("default workflows = %+v", payload)
	}

	// Writes without a credential are rejected.
	unprotected := authRequest(t, http.MethodPut, web.URL+"/api/v1/workflows?project=machinist", "", DefaultWorkflowConfig())
	unprotected.Body.Close()
	if unprotected.StatusCode != http.StatusUnauthorized && unprotected.StatusCode != http.StatusForbidden {
		t.Fatalf("unprotected write = %d", unprotected.StatusCode)
	}
	// Read scope cannot write.
	denied := authRequest(t, http.MethodPut, web.URL+"/api/v1/workflows?project=machinist", readSecret, DefaultWorkflowConfig())
	denied.Body.Close()
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("read-scope write = %d", denied.StatusCode)
	}

	// A valid save persists, is audit-logged and is no longer the default.
	response = authRequest(t, http.MethodPut, web.URL+"/api/v1/workflows?project=machinist", writeSecret, DefaultWorkflowConfig())
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if payload.Default || payload.Config.DefaultType != "web_pr" {
		t.Fatalf("put workflows = %+v", payload)
	}
	entries, err := server.store.AuditLog(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	sawWrite := false
	for _, entry := range entries {
		if entry["action"] == "workflows.write" {
			sawWrite = true
		}
	}
	if !sawWrite {
		t.Fatalf("workflows.write was not audit-logged: %+v", entries)
	}
	response = authRequest(t, http.MethodGet, web.URL+"/api/v1/workflows?project=machinist", readSecret, nil)
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if payload.Default {
		t.Fatalf("saved config still reported as default: %+v", payload)
	}

	// Invalid configs are rejected.
	invalid := DefaultWorkflowConfig()
	invalid.Types[0].Stages[0].Kind = "teleport"
	response = authRequest(t, http.MethodPut, web.URL+"/api/v1/workflows?project=machinist", writeSecret, invalid)
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid put = %d", response.StatusCode)
	}

	// Job types can be added and deleted.
	jobType := JobTypeDefinition{Name: "hotfix", Stages: []StageDefinition{{ID: "queued", Kind: StageKindQueued}, {ID: "hotfix", Name: "Hotfix", Kind: StageKindCoding}, {ID: "finished", Name: "Finished", Kind: StageKindFinished}}}
	response = authRequest(t, http.MethodPut, web.URL+"/api/v1/job-types?project=machinist", writeSecret, jobType)
	var types jobTypesResponse
	if err := json.NewDecoder(response.Body).Decode(&types); err != nil {
		response.Body.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if _, ok := containsJobType(types.JobTypes, "hotfix"); !ok {
		t.Fatalf("job type not added: %+v", types.JobTypes)
	}
	deleted := authRequest(t, http.MethodDelete, web.URL+"/api/v1/job-types?project=machinist&name=hotfix", writeSecret, nil)
	defer deleted.Body.Close()
	if deleted.StatusCode != http.StatusOK {
		t.Fatalf("delete job type = %d", deleted.StatusCode)
	}
	if err := json.NewDecoder(deleted.Body).Decode(&types); err != nil {
		t.Fatal(err)
	}
	if _, ok := containsJobType(types.JobTypes, "hotfix"); ok {
		t.Fatalf("job type not deleted: %+v", types.JobTypes)
	}
}

func containsJobType(types []JobTypeDefinition, name string) (JobTypeDefinition, bool) {
	for _, jobType := range types {
		if jobType.Name == name {
			return jobType, true
		}
	}
	return JobTypeDefinition{}, false
}

// TestWorkflowStagesMCPTools exercises the MCP view of the workflow config:
// project validation, persistence, audit and job type deletion.
func TestWorkflowStagesMCPTools(t *testing.T) {
	store := openTestStore(t, t.TempDir()+"/machinist.db")
	definitionPath := t.TempDir() + "/config.toml"
	if err := os.WriteFile(definitionPath, []byte("[commands.plan]\nexecutor = \"test\"\ntimeout = \"1m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(store, definitionPath, "secret", 0, config.AuthNone)
	if err != nil {
		t.Fatal(err)
	}
	identity := TokenIdentity{Name: "tester"}
	call := func(name, arguments string) (any, error) {
		return server.invokeMCPTool(t.Context(), identity, name, json.RawMessage(arguments))
	}

	// Reading a project that has not saved a config returns the shipped types.
	result, err := call("workflows_get", `{"project":"sparkdash"}`)
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := result.(workflowConfigResponse)
	if !ok || payload.Config.DefaultType != "web_pr" {
		t.Fatalf("workflows_get = %#v", result)
	}

	// A full config write is validated and persisted.
	custom := `{"project":"sparkdash","config":{"default_type":"config_ops","label_mappings":{"type:config":"config_ops"},"types":[{"name":"config_ops","stages":[{"id":"queued","name":"Queued","kind":"queued"},{"id":"apply","name":"Apply","kind":"apply"},{"id":"finished","name":"Finished","kind":"finished"}]}]}}`
	if _, err := call("workflows_put", custom); err != nil {
		t.Fatal(err)
	}
	if result, err = call("job_types_list", `{"project":"sparkdash"}`); err != nil {
		t.Fatal(err)
	}
	if types, ok := result.(jobTypesResponse); !ok || len(types.JobTypes) != 1 || types.JobTypes[0].Name != "config_ops" {
		t.Fatalf("job_types_list = %#v", result)
	}

	// Unknown job types are rejected before storage.
	if _, err = call("job_types_put", `{"project":"sparkdash","name":"bad","stages":[{"id":"x","kind":"teleport"}]}`); err == nil {
		t.Fatal("expected validation error for unknown stage kind")
	}
	// Deleting the only type is refused.
	if _, err = call("job_types_delete", `{"project":"sparkdash","name":"config_ops"}`); err == nil {
		t.Fatal("expected refusal to delete the last job type")
	}
	entries, err := store.AuditLog(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	saw := false
	for _, entry := range entries {
		if entry["action"] == "workflows.write" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("workflows_put was not audit-logged: %+v", entries)
	}
}

// TestExecutorStageEventsDriveStage covers the executor-reported stage channel:
// a step result naming a stage and review round is recorded and then reflected
// in the derived strip.
func TestExecutorStageEventsDriveStage(t *testing.T) {
	store := openTestStore(t, t.TempDir()+"/machinist.db")
	jobID, err := store.CreateJobWithOptions(t.Context(), "Add page.", "sparkdash", "plan", config.ResolvedCommand{Name: "plan"}, CreateJobOptions{})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	recordStageMetadata(t.Context(), tx, jobID, json.RawMessage(`{"step_result":{"outcome":"complete","summary":"reviewing","stage":"review","review_round":2}}`))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET state='running' WHERE id=?`, jobID); err != nil {
		t.Fatal(err)
	}
	job := jobByID(t, store, jobID)
	if job.Stage == nil || job.Stage.Current != "review" || job.Stage.Round != 2 {
		t.Fatalf("stage = %+v", job.Stage)
	}
	// An invalid stage value is ignored rather than corrupting the job.
	tx, err = store.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	recordStageMetadata(t.Context(), tx, jobID, json.RawMessage(`{"step_result":{"outcome":"complete","summary":"x","stage":"not a stage"}}`))
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if job = jobByID(t, store, jobID); job.Stage.Current != "review" {
		t.Fatalf("invalid stage changed derivation: %+v", job.Stage)
	}
}
