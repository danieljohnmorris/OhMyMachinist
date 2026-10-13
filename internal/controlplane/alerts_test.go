package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/protocol"
)

func newTestHTTPServerWithAlerts(t *testing.T) (*Server, *httptest.Server, string) {
	t.Helper()
	directory := t.TempDir()
	promptPath := filepath.Join(directory, "plan.md")
	if err := os.WriteFile(promptPath, []byte("Plan this request:\n{{machinist.prompt}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	alertsDirectory := filepath.Join(directory, "alerts")
	definitionPath := filepath.Join(directory, "config.toml")
	definition := "[commands.plan]\nexecutor = \"test\"\nprompt_file = \"plan.md\"\ntimeout = \"1m\"\n\n[alerts]\ndirectory = \"" + alertsDirectory + "\"\n"
	if err := os.WriteFile(definitionPath, []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, filepath.Join(directory, "machinist.db"))
	server, err := NewServer(store, definitionPath, "secret", 0, config.AuthTokens)
	if err != nil {
		t.Fatal(err)
	}
	return server, httptest.NewServer(server.Handler()), alertsDirectory
}

func spooledFailAlerts(t *testing.T, directory string) map[string]FailAlert {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	alerts := map[string]FailAlert{}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var alert FailAlert
		if err := json.Unmarshal(body, &alert); err != nil {
			t.Fatalf("decode %s: %v", entry.Name(), err)
		}
		alerts[entry.Name()] = alert
	}
	return alerts
}

func TestTimedOutRunSpoolsOnePendingFailAlert(t *testing.T) {
	server, webServer, alertsDirectory := newTestHTTPServerWithAlerts(t)
	defer webServer.Close()
	auth := map[string]string{"Authorization": "Bearer secret"}

	jobID, err := server.store.CreateJobWithOptions(t.Context(), "request", "machinist", "plan", testAgent("plan", "Plan request"), CreateJobOptions{Title: "MACH-10 Boop the build", Project: "MACH"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = server.store.db.ExecContext(t.Context(), `INSERT INTO job_metadata(job_id,metadata) VALUES(?,?) ON CONFLICT(job_id) DO UPDATE SET metadata=excluded.metadata`, jobID, `{"project":"MACH","issue_url":"https://tracker.example/issues/MACH-10"}`); err != nil {
		t.Fatal(err)
	}
	run, err := server.store.Poll(t.Context(), pollRequest("worker-a", []string{"codex"}, []string{"machinist"}))
	if err != nil || run == nil {
		t.Fatalf("poll = %#v, %v", run, err)
	}

	endpoint := webServer.URL + "/api/v1/runs/" + run.ID + "/complete"
	timeout := postJSON(t, endpoint, protocol.Completion{
		InstanceID: "worker-a", LeaseToken: run.LeaseToken, State: "timed_out", ExitCode: 124,
	}, auth)
	defer timeout.Body.Close()
	if timeout.StatusCode != http.StatusNoContent {
		t.Fatalf("timed out completion status = %d", timeout.StatusCode)
	}

	// A repeated delivery of the same terminal status must not duplicate the alert.
	repeat := postJSON(t, endpoint, protocol.Completion{
		InstanceID: "worker-a", LeaseToken: run.LeaseToken, State: "timed_out", ExitCode: 124,
	}, auth)
	defer repeat.Body.Close()
	if repeat.StatusCode != http.StatusNoContent {
		t.Fatalf("repeat completion status = %d", repeat.StatusCode)
	}

	alerts := spooledFailAlerts(t, alertsDirectory)
	if len(alerts) != 1 {
		t.Fatalf("spooled %d alerts, want exactly one: %#v", len(alerts), alerts)
	}
	alert, ok := alerts["fail-"+run.ID+".json"]
	if !ok {
		t.Fatalf("missing fail-%s.json: %#v", run.ID, alerts)
	}
	if alert.JobID != jobID || alert.RunID != run.ID {
		t.Fatalf("alert ids = %s/%s, want %s/%s", alert.JobID, alert.RunID, jobID, run.ID)
	}
	if alert.Reason != "timed_out" {
		t.Fatalf("reason = %q, want timed_out", alert.Reason)
	}
	if alert.ExitCode != 124 {
		t.Fatalf("exit code = %d, want 124", alert.ExitCode)
	}
	if alert.Command != "plan" {
		t.Fatalf("command = %q, want plan", alert.Command)
	}
	if alert.TimeoutMillis != int64(time.Minute.Milliseconds()) {
		t.Fatalf("timeout millis = %d, want %d", alert.TimeoutMillis, time.Minute.Milliseconds())
	}
	if alert.ElapsedMillis < 0 {
		t.Fatalf("elapsed millis = %d, want non-negative", alert.ElapsedMillis)
	}
	if alert.IssueKey != "MACH-10" {
		t.Fatalf("issue key = %q, want MACH-10", alert.IssueKey)
	}
	if alert.IssueURL != "https://tracker.example/issues/MACH-10" {
		t.Fatalf("issue url = %q", alert.IssueURL)
	}
}

func TestFailedRunSpoolsFailAlertAndCancelledDoesNot(t *testing.T) {
	server, webServer, alertsDirectory := newTestHTTPServerWithAlerts(t)
	defer webServer.Close()
	auth := map[string]string{"Authorization": "Bearer secret"}

	if _, err := server.store.CreateJob(t.Context(), "request", "machinist", "plan", testAgent("plan", "Plan request")); err != nil {
		t.Fatal(err)
	}
	run, err := server.store.Poll(t.Context(), pollRequest("worker-a", []string{"codex"}, []string{"machinist"}))
	if err != nil || run == nil {
		t.Fatalf("poll = %#v, %v", run, err)
	}
	endpoint := webServer.URL + "/api/v1/runs/" + run.ID + "/complete"
	failed := postJSON(t, endpoint, protocol.Completion{
		InstanceID: "worker-a", LeaseToken: run.LeaseToken, State: "failed", ExitCode: 7,
	}, auth)
	defer failed.Body.Close()
	if failed.StatusCode != http.StatusNoContent {
		t.Fatalf("failed completion status = %d", failed.StatusCode)
	}
	alerts := spooledFailAlerts(t, alertsDirectory)
	if len(alerts) != 1 {
		t.Fatalf("spooled %d alerts, want exactly one", len(alerts))
	}
	for _, alert := range alerts {
		if alert.Reason != "failed" || alert.ExitCode != 7 || alert.RunID != run.ID {
			t.Fatalf("unexpected alert %#v", alert)
		}
	}

	// A cancelled run is an operator action and never spools an alert.
	cancelled, err := server.store.CreateJob(t.Context(), "second", "machinist", "plan", testAgent("plan", "Second request"))
	if err != nil {
		t.Fatal(err)
	}
	cancelRun, err := server.store.Poll(t.Context(), pollRequest("worker-a", []string{"codex"}, []string{"machinist"}))
	if err != nil || cancelRun == nil {
		t.Fatalf("poll = %#v, %v", cancelRun, err)
	}
	cancelledCompletion := postJSON(t, webServer.URL+"/api/v1/runs/"+cancelRun.ID+"/complete", protocol.Completion{
		InstanceID: "worker-a", LeaseToken: cancelRun.LeaseToken, State: "cancelled", ExitCode: 130,
	}, auth)
	defer cancelledCompletion.Body.Close()
	if cancelledCompletion.StatusCode != http.StatusNoContent {
		t.Fatalf("cancelled completion status = %d", cancelledCompletion.StatusCode)
	}
	alerts = spooledFailAlerts(t, alertsDirectory)
	if len(alerts) != 1 {
		t.Fatalf("cancelled run spooled an alert: %#v (job %s)", alerts, cancelled)
	}
}

func TestFailAlertWriterWithoutDirectoryIsNoop(t *testing.T) {
	writer := &failAlertWriter{}
	if err := writer.write(FailAlert{RunID: "run_abc", Reason: "timed_out"}); err != nil {
		t.Fatalf("write with no directory: %v", err)
	}
	var nilWriter *failAlertWriter
	if err := nilWriter.write(FailAlert{RunID: "run_abc"}); err != nil {
		t.Fatalf("nil writer write: %v", err)
	}
}

func TestFailAlertWriterIsIdempotentPerRun(t *testing.T) {
	directory := t.TempDir()
	writer := &failAlertWriter{directory: directory}
	first := FailAlert{JobID: "job_1", RunID: "run_1", Command: "plan", TimeoutMillis: 60000, ExitCode: 124, Reason: "timed_out"}
	if err := writer.write(first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ExitCode = 1
	if err := writer.write(second); err != nil {
		t.Fatal(err)
	}
	alerts := spooledFailAlerts(t, directory)
	if len(alerts) != 1 {
		t.Fatalf("spooled %d alerts, want one", len(alerts))
	}
	if alerts["fail-run_1.json"].ExitCode != 124 {
		t.Fatalf("existing alert was overwritten: %#v", alerts)
	}
}
