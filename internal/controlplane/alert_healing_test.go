package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/protocol"
)

// webhookRecorder captures the POSTs the alert webhook delivers.
type webhookRecorder struct {
	mu       sync.Mutex
	requests int
	header   http.Header
	body     string
}

func (r *webhookRecorder) record(request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests++
	r.header = request.Header.Clone()
	r.body = string(body)
}

func (r *webhookRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests
}

// waitWebhook fails unless the recorder has seen at least n requests.
func waitWebhook(t *testing.T, recorder *webhookRecorder, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if recorder.count() >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("webhook saw %d requests, want %d", recorder.count(), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// newAlertsServer builds a server whose alert spool points at a temp
// directory; "{{alerts}}" in alertsTemplate is replaced with the directory.
func newAlertsServer(t *testing.T, alertsTemplate string) (*Server, *Store, string) {
	t.Helper()
	directory := t.TempDir()
	alertsDirectory := filepath.Join(directory, "alerts")
	alertsTable := strings.ReplaceAll(alertsTemplate, "{{alerts}}", alertsDirectory)
	promptPath := filepath.Join(directory, "plan.md")
	if err := os.WriteFile(promptPath, []byte("{{machinist.prompt}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	definitionPath := filepath.Join(directory, "config.toml")
	definition := "[commands.plan]\nexecutor = \"test\"\nprompt_file = \"plan.md\"\ntimeout = \"1m\"\n\n" + alertsTable
	if err := os.WriteFile(definitionPath, []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, filepath.Join(directory, "machinist.db"))
	server, err := NewServer(store, definitionPath, "secret", 0, config.AuthTokens)
	if err != nil {
		t.Fatal(err)
	}
	return server, store, alertsDirectory
}

// A failed run whose self-healing gives up produces exactly one pending alert
// file, one webhook POST, and none of either on a re-delivered completion.
// The job has no Plane client here: jobs without a tracker still alert.
func TestFailAlertWebhookFiresOncePerRun(t *testing.T) {
	recorder := &webhookRecorder{}
	webhook := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		response.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()
	t.Setenv("MACHINIST_TEST_ALERT_SECRET", "hunter2")
	server, store, alertsDirectory := newAlertsServer(t, fmt.Sprintf(`[alerts]
directory = "{{alerts}}"
webhook_url = %q
webhook_secret_env = "MACHINIST_TEST_ALERT_SECRET"

[healing]
max_retries = -1
`, webhook.URL))
	webServer := httptest.NewServer(server.Handler())
	defer webServer.Close()
	auth := map[string]string{"Authorization": "Bearer secret"}

	jobID, err := store.CreateJobWithOptions(t.Context(), "request", "machinist", "plan", testAgent("plan", "Plan request"), CreateJobOptions{Title: "MACH-10 Boop the build"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Poll(t.Context(), pollRequest("worker-a", []string{"codex"}, []string{"machinist"}))
	if err != nil || run == nil {
		t.Fatalf("poll = %#v, %v", run, err)
	}
	endpoint := webServer.URL + "/api/v1/runs/" + run.ID + "/complete"
	completion := postJSON(t, endpoint, protocol.Completion{
		InstanceID: "worker-a", LeaseToken: run.LeaseToken, State: "failed", ExitCode: 7, Error: "go test: FAIL example.com/app",
	}, auth)
	defer completion.Body.Close()
	if completion.StatusCode != http.StatusNoContent {
		t.Fatalf("failed completion status = %d", completion.StatusCode)
	}

	waitSpooled(t, alertsDirectory, "fail-"+run.ID+".json")
	waitWebhook(t, recorder, 1)
	if got := recorder.header.Get("X-Machinist-Alert-Token"); got != "hunter2" {
		t.Fatalf("webhook secret header = %q, want hunter2", got)
	}
	var alert FailAlert
	if err := json.Unmarshal([]byte(recorder.body), &alert); err != nil {
		t.Fatalf("decode webhook body: %v", err)
	}
	if alert.RunID != run.ID || alert.JobID != jobID || alert.ExitCode != 7 {
		t.Fatalf("webhook alert = %#v", alert)
	}
	if alert.Classification != string(CauseTest) {
		t.Fatalf("classification = %q, want %q", alert.Classification, CauseTest)
	}
	if !strings.Contains(alert.StderrTail, "FAIL example.com/app") {
		t.Fatalf("stderr tail = %q", alert.StderrTail)
	}
	if alert.Healing == nil || !alert.Healing.NeedsHuman {
		t.Fatalf("webhook alert has no needs-human diagnosis: %+v", alert.Healing)
	}

	// A re-delivered completion neither rewrites the file nor re-posts.
	repeat := postJSON(t, endpoint, protocol.Completion{
		InstanceID: "worker-a", LeaseToken: run.LeaseToken, State: "failed", ExitCode: 7, Error: "go test: FAIL example.com/app",
	}, auth)
	defer repeat.Body.Close()
	if repeat.StatusCode != http.StatusNoContent {
		t.Fatalf("repeat completion status = %d", repeat.StatusCode)
	}
	time.Sleep(200 * time.Millisecond)
	if got := recorder.count(); got != 1 {
		t.Fatalf("webhook posts = %d, want exactly one", got)
	}
	if alerts := spooledFailAlerts(t, alertsDirectory); len(alerts) != 1 {
		t.Fatalf("spooled %d alerts, want exactly one", len(alerts))
	}
}

func TestAlertWebhookRetriesUntilSuccess(t *testing.T) {
	recorder := &webhookRecorder{}
	webhook := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if recorder.count() < 2 {
			recorder.record(request)
			response.WriteHeader(http.StatusInternalServerError)
			return
		}
		recorder.record(request)
		response.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()
	directory := t.TempDir()
	writer := &failAlertWriter{directory: directory, webhook: &alertWebhook{
		url: webhook.URL, client: &http.Client{Timeout: time.Second},
	}}
	if err := writer.write(FailAlert{JobID: "job_1", RunID: "run_retry", Reason: "failed"}); err != nil {
		t.Fatal(err)
	}
	waitWebhook(t, recorder, 3)
	if _, err := os.Stat(filepath.Join(directory, "fail-run_retry.json")); err != nil {
		t.Fatalf("alert file missing after retries: %v", err)
	}
}

func TestAlertWebhookTimeoutKeepsFileAndGivesUp(t *testing.T) {
	recorder := &webhookRecorder{}
	webhook := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		time.Sleep(100 * time.Millisecond)
		response.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()
	directory := t.TempDir()
	writer := &failAlertWriter{directory: directory, webhook: &alertWebhook{
		url: webhook.URL, client: &http.Client{Timeout: 10 * time.Millisecond},
	}}
	if err := writer.write(FailAlert{JobID: "job_1", RunID: "run_slow", Reason: "failed"}); err != nil {
		t.Fatal(err)
	}
	waitWebhook(t, recorder, webhookAttempts)
	if _, err := os.Stat(filepath.Join(directory, "fail-run_slow.json")); err != nil {
		t.Fatalf("alert file missing despite webhook timeouts: %v", err)
	}
}

// A failure that self-healing requeues produces no needs-human alert, only
// the optional healing alert when that is enabled.
func TestRequeuedRunSpoolsHealingAlertNotFailAlert(t *testing.T) {
	server, store, alertsDirectory := newAlertsServer(t, `[alerts]
directory = "{{alerts}}"
healing_alerts = true
`)
	jobID, runID := seedFailedRun(t, store, "https://plane.example/MACH-6/", "go test: FAIL example.com/app", "")

	server.HealRun(context.Background(), runID)

	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "queued" {
		t.Fatalf("job state = %q, want queued", job.State)
	}
	if _, err := os.Stat(filepath.Join(alertsDirectory, "fail-"+runID+".json")); !os.IsNotExist(err) {
		t.Fatalf("requeued run spooled a needs-human alert: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(alertsDirectory, "healing-"+runID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var alert FailAlert
	if err := json.Unmarshal(body, &alert); err != nil {
		t.Fatal(err)
	}
	if alert.Healing == nil || alert.Healing.NeedsHuman {
		t.Fatalf("healing alert is a needs-human alert: %+v", alert.Healing)
	}
	if alert.Healing.Cause != CauseTest || alert.Healing.Reason == "" {
		t.Fatalf("unexpected healing diagnosis: %+v", alert.Healing)
	}
}

// The give-up path never writes the optional healing alert.
func TestExhaustedRunSpoolsFailAlertButNoHealingAlert(t *testing.T) {
	server, store, alertsDirectory := newAlertsServer(t, `[alerts]
directory = "{{alerts}}"
healing_alerts = true

[healing]
max_retries = -1
`)
	jobID, runID := seedFailedRun(t, store, "https://plane.example/MACH-6/", "FAIL", "")

	server.HealRun(context.Background(), runID)

	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "failed" {
		t.Fatalf("job state = %q, want failed (budget spent)", job.State)
	}
	if _, err := os.Stat(filepath.Join(alertsDirectory, "fail-"+runID+".json")); err != nil {
		t.Fatalf("fail alert missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(alertsDirectory, "healing-"+runID+".json")); !os.IsNotExist(err) {
		t.Fatalf("give-up run spooled a healing alert: %v", err)
	}
}

// Workflow jobs cannot be requeued by healing; they alert the watcher.
func TestWorkflowJobSpoolsFailAlert(t *testing.T) {
	server, store, alertsDirectory := newAlertsServer(t, "[alerts]\ndirectory = \"{{alerts}}\"\n")
	jobID, runID := seedFailedRun(t, store, "https://plane.example/MACH-6/", "go test: FAIL example.com/app", "")
	if _, err := store.db.Exec(`INSERT INTO workflow_jobs(job_id,name,plan) VALUES(?,?,?)`, jobID, "plan", `[]`); err != nil {
		t.Fatal(err)
	}

	server.HealRun(context.Background(), runID)

	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "failed" || len(job.Runs) != 1 {
		t.Fatalf("workflow job was healed: state=%q runs=%d", job.State, len(job.Runs))
	}
	body, err := os.ReadFile(filepath.Join(alertsDirectory, "fail-"+runID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var alert FailAlert
	if err := json.Unmarshal(body, &alert); err != nil {
		t.Fatal(err)
	}
	if alert.Healing == nil || !alert.Healing.NeedsHuman || !strings.Contains(alert.Healing.Reason, "workflow") {
		t.Fatalf("unexpected workflow alert: %+v", alert.Healing)
	}
	if alert.Classification != string(CauseTest) {
		t.Fatalf("classification = %q, want %q", alert.Classification, CauseTest)
	}
}

// Health alerts land in the same spool and wake the same webhook; recovery
// posts nothing.
func TestHealthAlertPostsToWebhook(t *testing.T) {
	recorder := &webhookRecorder{}
	webhook := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			recorder.record(request)
		}
		response.WriteHeader(http.StatusOK)
	}))
	defer webhook.Close()
	directory := t.TempDir()
	checker := &healthChecker{
		alerts:  &failAlertWriter{directory: directory, webhook: &alertWebhook{url: webhook.URL, client: &http.Client{Timeout: time.Second}}},
		timeout: 2 * time.Second,
		now:     time.Now,
	}
	if err := checker.checkOne(context.Background(), "glm", "http://127.0.0.1:1/unreachable", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	waitWebhook(t, recorder, 1)
	var alert healthAlert
	if err := json.Unmarshal([]byte(recorder.body), &alert); err != nil {
		t.Fatalf("decode health webhook body: %v", err)
	}
	if alert.Name != "glm" || alert.Error == "" {
		t.Fatalf("health alert = %#v", alert)
	}
	if _, err := os.Stat(filepath.Join(directory, "health-glm.json")); err != nil {
		t.Fatalf("health alert file missing: %v", err)
	}
	// Recovery clears the file and posts nothing.
	if err := checker.checkOne(context.Background(), "glm", webhook.URL, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "health-glm.json")); !os.IsNotExist(err) {
		t.Fatalf("recovered endpoint left its alert: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := recorder.count(); got != 1 {
		t.Fatalf("webhook posts = %d, want exactly one", got)
	}
}
