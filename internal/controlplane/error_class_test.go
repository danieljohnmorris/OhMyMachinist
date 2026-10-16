package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/config"
)

// The MACH-60 incident transcript: omp auto_retry_end with a connection-type
// finalError after ten attempts, then an agent_end that produced nothing.
const llmUnavailableEvents = `{"sequence":1,"type":"agent.auto_retry_end","message":"Unable to connect. Is the computer able to access the url?","failed":true}
{"sequence":2,"type":"agent.agent_end","message":"output_tokens=0","failed":true}`

// A shim or proxy refusing the connection, with the agent having produced
// nothing: the SPARK-10 signature.
const shimRefusedEvents = `{"sequence":1,"type":"process.output","stream":"stderr","data":"ConnectionRefusedError: [Errno 111] Connection refused"}
{"sequence":2,"type":"agent.agent_end","message":"output_tokens=0","failed":true}`

func TestClassifyErrorClassFixture(t *testing.T) {
	class, reason := classifyErrorClass("command exited with status 1", llmUnavailableEvents)
	if class != ErrorClassLLMUnavailable {
		t.Fatalf("class = %q, want llm_unavailable", class)
	}
	if !strings.Contains(reason, "Unable to connect") {
		t.Fatalf("reason = %q, want the auto_retry_end final error", reason)
	}
}

func TestClassifyErrorClassNormalFailureNotClassified(t *testing.T) {
	events := `{"sequence":1,"type":"agent.tool_result","tool":"bash","message":"--- FAIL: TestSidebar (0.00s)","failed":true}`
	if class, _ := classifyErrorClass("command exited with status 1", events); class != "" {
		t.Fatalf("normal test failure classified as %q", class)
	}
}

func TestClassifyErrorClassShimRefusalWithZeroTokens(t *testing.T) {
	class, reason := classifyErrorClass("command exited with status 1", shimRefusedEvents)
	if class != ErrorClassLLMUnavailable {
		t.Fatalf("class = %q, want llm_unavailable", class)
	}
	if !strings.Contains(reason, "zero-output-token") {
		t.Fatalf("reason = %q", reason)
	}
}

// A connection error without an agent that produced nothing is not enough: a
// test that logged a refused connection must not classify.
func TestClassifyErrorClassConnectionErrorWithoutZeroTokens(t *testing.T) {
	events := `{"sequence":1,"type":"process.output","stream":"stderr","data":"connection refused from test double port"}
{"sequence":2,"type":"agent.agent_end","message":"output_tokens=7"}`
	if class, _ := classifyErrorClass("command exited with status 1", events); class != "" {
		t.Fatalf("connection error without zero tokens classified as %q", class)
	}
}

func TestHealRunClassifiesLLMUnavailableWithoutBudget(t *testing.T) {
	server, store, requests := newHealingTestServer(t)
	server.llmUnavailable = config.LLMUnavailable{Endpoint: "http://model-host.local:8000", Model: "deepseek-v4.1-flash"}

	jobID, runID := seedFailedRun(t, store, "https://plane.example/MACH-6/", "command exited with status 1", llmUnavailableEvents)
	server.HealRun(context.Background(), runID)

	job, err := store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	// Not requeued by healing: the scheduled pass owns the wait.
	if job.State != "failed" || len(job.Runs) != 1 {
		t.Fatalf("llm_unavailable healed as a retry: state=%q runs=%d", job.State, len(job.Runs))
	}
	if !strings.Contains(job.Prompt, "Fix the sidebar.") || strings.Contains(job.Prompt, "Classified cause") {
		t.Fatalf("prompt was rewritten: %s", job.Prompt)
	}
	if job.Metadata == nil || job.Metadata.ErrorClass == nil || job.Metadata.ErrorClass.Class != ErrorClassLLMUnavailable {
		t.Fatalf("error class not recorded: %+v", job.Metadata)
	}
	if job.Metadata.ErrorClass.Provider != "test" || job.Metadata.ErrorClass.Endpoint == "" {
		t.Fatalf("provider/endpoint not recorded: %+v", job.Metadata.ErrorClass)
	}
	if _, err := time.Parse(time.RFC3339, job.Metadata.ErrorClass.At); err != nil {
		t.Fatalf("classification time missing: %v", err)
	}
	// The Plane comment says LLM unavailable; the ticket is not moved to
	// Needs attention and no failed-label patch is sent.
	var commented, stateSet bool
	for _, request := range *requests {
		if request.Method == "POST" && strings.Contains(request.Path, "/comments") && strings.Contains(request.Body, "LLM unavailable") {
			commented = true
		}
		if request.Method == "PATCH" && strings.Contains(request.Body, "Needs attention") {
			stateSet = true
		}
	}
	if !commented {
		t.Fatalf("no llm_unavailable comment posted: %+v", *requests)
	}
	if stateSet {
		t.Fatal("ticket moved to Needs attention for an llm_unavailable outage")
	}
}

// seedLLMFailedJob seeds a failed job already classified llm_unavailable and
// returns its id plus the spool directory for alerts.
func seedLLMFailedJob(t *testing.T, store *Store, at time.Time, requeues int) string {
	t.Helper()
	jobID, err := store.CreateJobWithOptions(context.Background(), "Fix the sidebar.", "sparkdash", "plan", config.ResolvedCommand{Name: "plan", Timeout: time.Minute}, CreateJobOptions{Title: "MACH-6 sidebar", IssueKey: "MACH-6"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE jobs SET state='failed' WHERE id=?`, jobID); err != nil {
		t.Fatal(err)
	}
	runID, err := randomID("run", 12)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO runs(id,job_id,command,command_hash,executor,model,repository,rendered_prompt,timeout_ms,state,exit_code,error,completed_at) VALUES(?,?,?,?,?,?,?,?,?,'failed',1,'command exited with status 1',?)`,
		runID, jobID, "plan", "", "test", "", "sparkdash", "Fix the sidebar.", int64(60000), at.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DELETE FROM runs WHERE job_id=? AND id<>?`, jobID, runID); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveErrorClassMetadata(context.Background(), jobID, ErrorClassRecord{
		Class: ErrorClassLLMUnavailable, Reason: "omp auto_retry_end failed: Unable to connect",
		Model: "deepseek-v4.1-flash", Endpoint: "http://model-host.local:8000",
		At: at.Format(time.RFC3339), Requeues: requeues,
	}); err != nil {
		t.Fatal(err)
	}
	return jobID
}

func llmTestServer(t *testing.T, endpoint, model string, alerts string) *Server {
	t.Helper()
	server, store := newResolutionTestServer(t)
	_ = store
	server.llmUnavailable = config.LLMUnavailable{Endpoint: endpoint, Model: model}
	if alerts != "" {
		server.failAlerts = &failAlertWriter{directory: alerts}
	}
	return server
}

func modelEndpoint(t *testing.T, served ...string) *httptest.Server {
	t.Helper()
	endpoint := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if !strings.HasSuffix(request.URL.Path, "/v1/models") {
			http.Error(response, "not found", http.StatusNotFound)
			return
		}
		ids := make([]map[string]string, 0, len(served))
		for _, id := range served {
			ids = append(ids, map[string]string{"id": id})
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(map[string]any{"data": ids})
	}))
	t.Cleanup(endpoint.Close)
	return endpoint
}

func TestLLMRequeuesOnceWhenEndpointHealthy(t *testing.T) {
	endpoint := modelEndpoint(t, "deepseek-v4.1-flash")
	server := llmTestServer(t, endpoint.URL, "deepseek-v4.1-flash", t.TempDir())
	jobID := seedLLMFailedJob(t, server.store, server.now().UTC().Add(-time.Minute), 0)

	server.healLLMUnavailablePass(context.Background())

	job, err := server.store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "queued" || len(job.Runs) != 2 {
		t.Fatalf("expected one requeue, got state=%q runs=%d", job.State, len(job.Runs))
	}
	if job.Metadata == nil || job.Metadata.ErrorClass == nil || job.Metadata.ErrorClass.Requeues != 1 || !job.Metadata.ErrorClass.Resolved {
		t.Fatalf("requeue not recorded once: %+v", job.Metadata)
	}

	// The pass never requeues twice: the record is resolved.
	server.healLLMUnavailablePass(context.Background())
	job, err = server.store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(job.Runs) != 2 {
		t.Fatalf("duplicate requeue: runs=%d", len(job.Runs))
	}
}

func TestLLMRequeueDedupesActiveTicketAttempt(t *testing.T) {
	endpoint := modelEndpoint(t, "deepseek-v4.1-flash")
	server := llmTestServer(t, endpoint.URL, "deepseek-v4.1-flash", t.TempDir())
	jobID := seedLLMFailedJob(t, server.store, server.now().UTC().Add(-time.Minute), 0)
	// A second attempt of the same ticket is already queued.
	if _, err := server.store.db.Exec(`INSERT INTO jobs(id,prompt,repository,command,state,issue_key,created_at,updated_at) VALUES('job_other','w','sparkdash','plan','queued','MACH-6',?,?)`,
		server.now().UTC().Format(time.RFC3339Nano), server.now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	server.healLLMUnavailablePass(context.Background())

	job, err := server.store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "failed" || len(job.Runs) != 1 {
		t.Fatalf("requeued despite an active attempt: state=%q runs=%d", job.State, len(job.Runs))
	}
	if job.Metadata == nil || job.Metadata.ErrorClass == nil || !strings.Contains(job.Metadata.ErrorClass.Stopped, "another attempt") {
		t.Fatalf("dedupe not recorded: %+v", job.Metadata)
	}
}

func TestLLMRequeueAlertsAfterMaxWait(t *testing.T) {
	endpoint := modelEndpoint(t) // nothing served: endpoint is down
	alerts := t.TempDir()
	server := llmTestServer(t, endpoint.URL, "deepseek-v4.1-flash", alerts)
	jobID := seedLLMFailedJob(t, server.store, server.now().UTC().Add(-2*time.Hour), 0)

	server.healLLMUnavailablePass(context.Background())

	job, err := server.store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "failed" || len(job.Runs) != 1 {
		t.Fatalf("requeued after max wait: state=%q runs=%d", job.State, len(job.Runs))
	}
	if job.Metadata == nil || job.Metadata.ErrorClass == nil || !job.Metadata.ErrorClass.Resolved || !strings.Contains(job.Metadata.ErrorClass.Stopped, "max wait") {
		t.Fatalf("max wait not recorded: %+v", job.Metadata)
	}
	body, err := os.ReadFile(filepath.Join(alerts, "llm-unavailable-"+jobID+".json"))
	if err != nil {
		t.Fatalf("no alert spooled: %v", err)
	}
	if !strings.Contains(string(body), "left_for_human") {
		t.Fatalf("alert payload = %s", body)
	}
}

func TestLLMRequeueStopsWhenAllowanceExhausted(t *testing.T) {
	// The automatic attempt failed as llm_unavailable again (Requeues=1).
	endpoint := modelEndpoint(t, "deepseek-v4.1-flash")
	alerts := t.TempDir()
	server := llmTestServer(t, endpoint.URL, "deepseek-v4.1-flash", alerts)
	jobID := seedLLMFailedJob(t, server.store, server.now().UTC().Add(-time.Minute), 1)

	server.healLLMUnavailablePass(context.Background())

	job, err := server.store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "failed" || len(job.Runs) != 1 {
		t.Fatalf("requeued past the allowance: state=%q runs=%d", job.State, len(job.Runs))
	}
	body, err := os.ReadFile(filepath.Join(alerts, "llm-unavailable-"+jobID+".json"))
	if err != nil {
		t.Fatalf("no alert spooled: %v", err)
	}
	if !strings.Contains(string(body), "allowance") {
		t.Fatalf("alert payload = %s", body)
	}
}

func TestLLMRequeueWaitsForHealthyEndpoint(t *testing.T) {
	// Endpoint down: the pass keeps waiting — no requeue, no alert, the
	// record stays unresolved so a later pass can requeue.
	endpoint := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Error(response, "down", http.StatusBadGateway)
	}))
	t.Cleanup(endpoint.Close)
	alerts := t.TempDir()
	server := llmTestServer(t, endpoint.URL, "deepseek-v4.1-flash", alerts)
	jobID := seedLLMFailedJob(t, server.store, server.now().UTC().Add(-time.Minute), 0)

	server.healLLMUnavailablePass(context.Background())

	job, err := server.store.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != "failed" || len(job.Runs) != 1 || job.Metadata.ErrorClass.Resolved {
		t.Fatalf("pass acted while the endpoint was down: %+v", job.Metadata)
	}
	if _, err := os.Stat(filepath.Join(alerts, "llm-unavailable-"+jobID+".json")); !os.IsNotExist(err) {
		t.Fatal("alert spooled while still waiting")
	}
}

func TestProbeModelEndpointReportsMismatch(t *testing.T) {
	endpoint := modelEndpoint(t, "glm-5.3") // replacement model, not the expected one
	server := llmTestServer(t, endpoint.URL, "deepseek-v4.1-flash", "")

	result := server.probeModelEndpoint(context.Background())
	if result.Healthy || !result.Mismatch {
		t.Fatalf("mismatch not reported: %+v", result)
	}
	if !strings.Contains(result.Detail, "glm-5.3") {
		t.Fatalf("detail = %q", result.Detail)
	}

	// With the block lifted the endpoint counts as usable, still logged.
	server.llmUnavailable.BlockOnModelMismatch = new(bool)
	result = server.probeModelEndpoint(context.Background())
	if !result.Healthy || !result.Mismatch {
		t.Fatalf("mismatch override not applied: %+v", result)
	}
}

func TestProbeModelEndpointHealthy(t *testing.T) {
	endpoint := modelEndpoint(t, "deepseek-v4.1-flash", "glm-5.3")
	server := llmTestServer(t, endpoint.URL, "deepseek-v4.1-flash", "")

	result := server.probeModelEndpoint(context.Background())
	if !result.Healthy || result.Mismatch {
		t.Fatalf("expected healthy: %+v", result)
	}
}

func TestFailAlertCarriesErrorClass(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	jobID, _ := seedFailedRun(t, store, "https://plane.example/MACH-6/", "command exited with status 1", llmUnavailableEvents)
	if err := store.SaveErrorClassMetadata(context.Background(), jobID, ErrorClassRecord{Class: ErrorClassLLMUnavailable}); err != nil {
		t.Fatal(err)
	}
	runID, err := store.LatestRunID(context.Background(), jobID)
	if err != nil {
		t.Fatal(err)
	}
	alert, ok, err := store.pendingFailAlert(context.Background(), runID)
	if err != nil || !ok {
		t.Fatalf("pendingFailAlert: ok=%v err=%v", ok, err)
	}
	if alert.ErrorClass != string(ErrorClassLLMUnavailable) {
		t.Fatalf("alert error class = %q", alert.ErrorClass)
	}
}

func TestJobErrorClassReader(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	jobID, _ := seedFailedRun(t, store, "https://plane.example/MACH-6/", "", "")
	if class := store.jobErrorClass(context.Background(), jobID); class != "" {
		t.Fatalf("unclassified job read as %q", class)
	}
	if err := store.SaveErrorClassMetadata(context.Background(), jobID, ErrorClassRecord{Class: ErrorClassLLMUnavailable}); err != nil {
		t.Fatal(err)
	}
	if class := store.jobErrorClass(context.Background(), jobID); class != ErrorClassLLMUnavailable {
		t.Fatalf("class = %q", class)
	}
}
