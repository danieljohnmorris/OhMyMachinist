package controlplane

import (
	"encoding/json"
	"testing"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/protocol"
)

func TestWorkflowStepMetadataIsStoredOnJob(t *testing.T) {
	store := openTestStore(t, t.TempDir()+"/db")
	_, err := store.createLegacyWorkflowJob(t.Context(), "issue URL", "machinist", "deliver", []config.WorkflowStep{{Command: testAgent("build", "issue URL")}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Poll(t.Context(), workflowWorker())
	if err != nil || run == nil {
		t.Fatalf("poll %v %v", run, err)
	}
	body, err := json.Marshal(map[string]any{"step_result": map[string]any{
		"outcome": "complete", "summary": "Ready for review",
		"metadata": map[string]any{
			"issue_url":      "https://plane.example.com/workspace/issue/ABC-12",
			"branch":         "factory/ABC-12",
			"pr_url":         "https://github.com/example/repo/pull/1",
			"review_verdict": map[string]any{"verdict": "approve", "summary": "Tests cover the change."},
			"screenshots":    []any{map[string]any{"label": "Desktop", "thumbnail_url": "https://cdn.example.com/desktop-small.png", "url": "https://cdn.example.com/desktop.png"}},
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Complete(t.Context(), run.ID, protocol.Completion{InstanceID: "worker-a", LeaseToken: run.LeaseToken, State: "succeeded", Result: body}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	metadata := snapshot.Jobs[0].Metadata
	if metadata == nil || metadata.IssueURL != "https://plane.example.com/workspace/issue/ABC-12" || metadata.Branch != "factory/ABC-12" ||
		metadata.PRURL != "https://github.com/example/repo/pull/1" || metadata.ReviewVerdict != `{"summary":"Tests cover the change.","verdict":"approve"}` ||
		len(metadata.Screenshots) != 1 || metadata.Screenshots[0].URL != "https://cdn.example.com/desktop.png" {
		t.Fatalf("metadata = %#v", metadata)
	}
}
