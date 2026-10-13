package managedworker

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/controlplane"
)

// A command that outlives its configured timeout ends timed_out (exit 124) and
// spools exactly one pending fail alert with reason "timed_out".
func TestTimedOutRunSpoolsPendingFailAlert(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repository")
	if output, err := exec.Command("git", "init", "--quiet", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	alertsDirectory := filepath.Join(directory, "alerts")
	definitionPath := filepath.Join(directory, "config.toml")
	definition := `[commands.plan]
executor = "test"
prompt_file = "plan.md"
timeout = "1s"

[alerts]
directory = "` + alertsDirectory + `"
`
	if err := os.WriteFile(definitionPath, []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "plan.md"), []byte("{{machinist.prompt}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	command, err := config.LoadCommand(definitionPath, "plan")
	if err != nil {
		t.Fatal(err)
	}
	command, err = config.RenderPrompt(command, "sleep past the timeout")
	if err != nil {
		t.Fatal(err)
	}
	store, err := controlplane.OpenStore(filepath.Join(directory, "machinist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateJob(t.Context(), "timed out request", "disposable", "plan", command); err != nil {
		t.Fatal(err)
	}
	server, err := controlplane.NewServer(store, definitionPath, "secret", 0, config.AuthTokens)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	tokenPath := filepath.Join(directory, "token")
	if err := os.WriteFile(tokenPath, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	worker, err := New(config.Worker{
		Name:          "timeout-worker",
		DataDirectory: filepath.Join(directory, "worker-data"),
		ControlPlane:  config.ControlPlane{URL: httpServer.URL, TokenFile: tokenPath},
		Executors:     map[string]config.Executor{"test": {Command: []string{"/bin/sh", "-c", "sleep 30"}}},
		Repositories:  map[string]config.Repository{"disposable": {Path: repository}},
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	deadline := time.Now().Add(15 * time.Second)
	var timedOut bool
	var snapshot controlplane.Snapshot
	for !timedOut {
		var err error
		snapshot, err = store.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Jobs) == 1 && len(snapshot.Jobs[0].Runs) == 1 && snapshot.Jobs[0].Runs[0].State == "timed_out" {
			timedOut = true
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not time out: %#v", snapshot.Jobs)
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	job := snapshot.Jobs[0]
	run := job.Runs[0]
	if run.State != "timed_out" || run.ExitCode == nil || *run.ExitCode != 124 {
		t.Fatalf("run = %#v", run)
	}
	// The run state commits before the completion handler spools the alert.
	deadline = time.Now().Add(5 * time.Second)
	var entries []os.DirEntry
	for {
		entries, err = os.ReadDir(alertsDirectory)
		if err == nil && len(entries) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("spooled %d alerts, want exactly one", len(entries))
		}
		time.Sleep(25 * time.Millisecond)
	}
	if len(entries) != 1 {
		t.Fatalf("spooled %d alerts, want exactly one", len(entries))
	}
	body, err := os.ReadFile(filepath.Join(alertsDirectory, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var alert controlplane.FailAlert
	if err := json.Unmarshal(body, &alert); err != nil {
		t.Fatal(err)
	}
	if alert.JobID != job.ID || alert.RunID != run.ID {
		t.Fatalf("alert ids = %s/%s, want %s/%s", alert.JobID, alert.RunID, job.ID, run.ID)
	}
	if alert.Reason != "timed_out" || alert.ExitCode != 124 || alert.Command != "plan" {
		t.Fatalf("unexpected alert: %#v", alert)
	}
	if alert.TimeoutMillis != 1000 {
		t.Fatalf("timeout millis = %d, want 1000", alert.TimeoutMillis)
	}
	if alert.ElapsedMillis <= 0 {
		t.Fatalf("elapsed millis = %d, want positive", alert.ElapsedMillis)
	}
}
