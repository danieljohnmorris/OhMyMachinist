package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/protocol"
)

func outputEvent(stream, text string) string {
	raw, err := json.Marshal(map[string]string{
		"type": "process.output", "stream": stream, "encoding": "base64",
		"data": base64.StdEncoding.EncodeToString([]byte(text)),
	})
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func TestRequeuedTaskJobPromptIncludesPriorAttempt(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	steps := []config.WorkflowStep{{Command: testAgent("build", "{{task.spec}}")}}
	const issue = "https://github.com/example/repo/issues/11"
	task := protocol.Task{Title: "Fix the thing", SourceURL: issue, Spec: "Build it."}

	if _, err := store.CreateTaskJob(t.Context(), task, "machinist", "deliver", "", steps); err != nil {
		t.Fatal(err)
	}
	worker := workflowWorker()
	worker.Artifacts = true
	run, err := store.Poll(t.Context(), worker)
	if err != nil || run == nil {
		t.Fatalf("poll first attempt: %v %v", run, err)
	}
	events := strings.Join([]string{
		outputEvent("stdout", "running tests\n"),
		outputEvent("stderr", "panic: connection refused\n"),
	}, "\n")
	if err := store.Complete(t.Context(), run.ID, protocol.Completion{
		InstanceID: "worker-a", LeaseToken: run.LeaseToken,
		State: "failed", ExitCode: 1, Error: "tests failed",
		Result: json.RawMessage(`{"summary":"connection refused"}`),
		Events: events,
	}); err != nil {
		t.Fatal(err)
	}

	// Requeuing the same issue puts the prior attempt's transcript and error
	// summary into the new coder prompt.
	if _, err := store.CreateTaskJob(t.Context(), task, "machinist", "deliver", "", steps); err != nil {
		t.Fatal(err)
	}
	retry, err := store.Poll(t.Context(), worker)
	if err != nil || retry == nil {
		t.Fatalf("poll retry: %v %v", retry, err)
	}
	if retry.Task == nil {
		t.Fatal("retry run has no task")
	}
	for _, want := range []string{
		"## Prior attempt", "Build it.", "tests failed",
		"panic: connection refused", "running tests", "connection refused",
	} {
		if !strings.Contains(retry.Task.Spec, want) {
			t.Fatalf("retry task spec missing %q:\n%s", want, retry.Task.Spec)
		}
	}
	snapshot, err := store.Snapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, job := range snapshot.Jobs {
		if job.ID == retry.JobID && !strings.Contains(job.Prompt, "## Prior attempt") {
			t.Fatalf("retry job prompt missing prior attempt:\n%s", job.Prompt)
		}
	}

	// A job on a different issue starts a fresh chain with no injected context.
	fresh := protocol.Task{Title: "Fresh", SourceURL: issue + "9", Spec: "Fresh build."}
	if _, err := store.CreateTaskJob(t.Context(), fresh, "machinist", "deliver", "", steps); err != nil {
		t.Fatal(err)
	}
	plainWorker := workflowWorker()
	plainWorker.Artifacts = true
	plainWorker.InstanceID = "worker-b"
	plain, err := store.Poll(t.Context(), plainWorker)
	if err != nil || plain == nil {
		t.Fatalf("poll fresh job: %v %v", plain, err)
	}
	if plain.Task == nil || !strings.Contains(plain.Task.Spec, "Fresh build.") {
		t.Fatalf("fresh task spec = %#v", plain.Task)
	}
	if strings.Contains(plain.Task.Spec, "## Prior attempt") {
		t.Fatalf("unrelated job got prior attempt context:\n%s", plain.Task.Spec)
	}
}

func TestRequeuedTriggeredJobPromptIncludesPriorAttempt(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	if err := store.SyncTriggers(t.Context(), []TriggerDefinition{{Identity: "issue/github", Family: "issue", ConfigSignature: "signature"}}); err != nil {
		t.Fatal(err)
	}
	generation := mustTriggerGeneration(t, store, "issue/github")
	const subject = "https://github.com/example/repo/issues/9"
	admission := func(occurrence string) TriggerAdmission {
		return TriggerAdmission{
			Identity: "issue/github", Family: "issue", ConfigSignature: "signature", ConfigGeneration: generation,
			OccurrenceKey: occurrence, Subject: subject, Title: "Fix it", ScheduledAt: store.now().UTC(),
			Prompt: "Build it.", Repository: "machinist", SelectionName: "deliver",
			Command: testAgent("build", "Build it."),
		}
	}

	first, created, err := store.CreateTriggeredJob(t.Context(), admission("event-1"))
	if err != nil || !created {
		t.Fatalf("first admission = %q, %v, %v", first, created, err)
	}
	run, err := store.Poll(t.Context(), pollRequest("worker-a", []string{"codex"}, []string{"machinist"}))
	if err != nil || run == nil {
		t.Fatalf("poll first attempt: %v %v", run, err)
	}
	if run.RenderedPrompt != "Build it." {
		t.Fatalf("first attempt prompt = %q", run.RenderedPrompt)
	}
	if err := store.Complete(t.Context(), run.ID, protocol.Completion{
		InstanceID: "worker-a", LeaseToken: run.LeaseToken,
		State: "failed", ExitCode: 1, Error: "tests failed",
		Events: outputEvent("stdout", "error: tests failed\n"),
	}); err != nil {
		t.Fatal(err)
	}

	// The Factory Alerts requeue path admits through a trigger; the requeued
	// coder prompt must carry the prior attempt's transcript and error.
	second, created, err := store.CreateTriggeredJob(t.Context(), admission("event-2"))
	if err != nil || !created {
		t.Fatalf("requeue admission = %q, %v, %v", second, created, err)
	}
	retry, err := store.Poll(t.Context(), pollRequest("worker-a", []string{"codex"}, []string{"machinist"}))
	if err != nil || retry == nil {
		t.Fatalf("poll retry: %v %v", retry, err)
	}
	for _, want := range []string{"## Prior attempt", "Build it.", "tests failed", "error: tests failed"} {
		if !strings.Contains(retry.RenderedPrompt, want) {
			t.Fatalf("retry prompt missing %q:\n%s", want, retry.RenderedPrompt)
		}
	}
}
