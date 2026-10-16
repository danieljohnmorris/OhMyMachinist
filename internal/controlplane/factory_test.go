package controlplane

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/owainlewis/machinist/internal/protocol"
)

// factoryPoll registers one lane and returns any run it is given.
func factoryPoll(t *testing.T, store *Store, instance, name string) *protocol.RunSpec {
	t.Helper()
	run, err := store.Poll(t.Context(), protocol.PollRequest{
		InstanceID: instance, Name: name, Executors: []string{"codex"}, Repositories: []string{"machinist"},
	})
	if err != nil {
		t.Fatalf("poll %s: %v", instance, err)
	}
	return run
}

func newFactoryJobs(t *testing.T, store *Store, prompts ...string) {
	t.Helper()
	for _, prompt := range prompts {
		if _, err := store.CreateJob(t.Context(), prompt, "machinist", "plan", testAgent("plan", "Plan request")); err != nil {
			t.Fatal(err)
		}
	}
}

// putJSON issues a PUT with a JSON body and the given headers.
func putJSON(t *testing.T, endpoint string, body any, headers map[string]string) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPut, endpoint, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func TestPauseStopsNewDispatchButKeepsRunningJob(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	newFactoryJobs(t, store, "first", "second")

	running := factoryPoll(t, store, "instance-a", "lane-a")
	if running == nil {
		t.Fatal("lane-a received no job before the pause")
	}

	if err := store.PauseFactory(t.Context(), "dan", "spark build", "drain"); err != nil {
		t.Fatal(err)
	}

	// A fresh idle lane gets nothing while stopped.
	if blocked := factoryPoll(t, store, "instance-b", "lane-b"); blocked != nil {
		t.Fatalf("dispatch continued while paused: %#v", blocked)
	}
	// The lane with the running job still gets its active run back, so the
	// worker stays connected and the job drains.
	active := factoryPoll(t, store, "instance-a", "lane-a")
	if active == nil || active.ID != running.ID {
		t.Fatalf("running job did not continue while draining: %#v", active)
	}

	status, err := store.FactoryStatus(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != FactoryDraining || status.Reason != "spark build" || status.Actor != "dan" || status.Since == nil {
		t.Fatalf("paused state = %#v", status)
	}

	if err := store.ResumeFactory(t.Context()); err != nil {
		t.Fatal(err)
	}
	if resumed := factoryPoll(t, store, "instance-b", "lane-b"); resumed == nil {
		t.Fatal("dispatch did not resume")
	}
}

func TestPauseCancelCancelsRunningJob(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	newFactoryJobs(t, store, "only")
	if run := factoryPoll(t, store, "instance-a", "lane-a"); run == nil {
		t.Fatal("lane-a received no job")
	}

	if err := store.PauseFactory(t.Context(), "dan", "stop now", "cancel"); err != nil {
		t.Fatal(err)
	}

	status, err := store.FactoryStatus(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != FactoryPaused {
		t.Fatalf("state = %q, want %q", status.State, FactoryPaused)
	}
	for _, lane := range status.Lanes {
		if lane.JobID != "" || lane.State == "busy" {
			t.Fatalf("lane still busy after cancel: %#v", lane)
		}
	}
	jobs, err := store.ListJobs(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || jobs[0].State != "cancelled" {
		t.Fatalf("jobs = %#v, want the running job cancelled", jobs)
	}
}

func TestJobCapClampsToEnabledLanes(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	factoryPoll(t, store, "instance-a", "lane-a")
	factoryPoll(t, store, "instance-b", "lane-b")

	if clamped, err := store.SetFactoryJobCap(t.Context(), 5); err != nil || clamped != 2 {
		t.Fatalf("set cap = %d, %v; want 2 clamped to the two ready lanes", clamped, err)
	}

	if err := store.SetLaneEnabled(t.Context(), "lane-b", false); err != nil {
		t.Fatal(err)
	}
	status, err := store.FactoryStatus(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if status.MaxConcurrentJobs != 1 {
		t.Fatalf("cap = %d after disabling a lane, want 1", status.MaxConcurrentJobs)
	}
	if status.ReadyLanes != 1 || status.EnabledLanes != 1 {
		t.Fatalf("lane counts = %#v", status)
	}
}

func TestDisabledLaneGetsNoNewJobs(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	newFactoryJobs(t, store, "first", "second")

	if err := store.SetLaneEnabled(t.Context(), "lane-a", false); err != nil {
		t.Fatal(err)
	}
	if run := factoryPoll(t, store, "instance-a", "lane-a"); run != nil {
		t.Fatalf("disabled lane received a job: %#v", run)
	}
	if run := factoryPoll(t, store, "instance-b", "lane-b"); run == nil {
		t.Fatal("enabled lane received no job")
	}

	status, err := store.FactoryStatus(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	disabled := false
	for _, lane := range status.Lanes {
		if lane.Name == "lane-a" {
			disabled = !lane.Enabled && lane.State == "disabled"
		}
	}
	if !disabled {
		t.Fatalf("lane-a is not reported disabled: %#v", status.Lanes)
	}
}

func TestFactoryStateSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machinist.db")
	store := openTestStore(t, path)
	factoryPoll(t, store, "instance-a", "lane-a")
	factoryPoll(t, store, "instance-b", "lane-b")
	if err := store.PauseFactory(t.Context(), "dan", "overnight", "drain"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetFactoryJobCap(t.Context(), 4); err != nil {
		t.Fatal(err)
	}

	restarted := openTestStore(t, path)
	status, err := restarted.FactoryStatus(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != FactoryDraining || status.Reason != "overnight" || status.Actor != "dan" {
		t.Fatalf("pause state did not survive restart: %#v", status)
	}
	if status.MaxConcurrentJobs != 2 {
		t.Fatalf("job cap = %d after restart, want the clamped 2", status.MaxConcurrentJobs)
	}
}

func TestFactoryHTTPControlsAndAudit(t *testing.T) {
	server, webServer := newTestHTTPServer(t)
	defer webServer.Close()
	factoryPoll(t, server.store, "instance-a", "lane-a")
	factoryPoll(t, server.store, "instance-b", "lane-b")
	newFactoryJobs(t, server.store, "first")

	bearer := map[string]string{"Authorization": "Bearer secret"}

	stop := postJSON(t, webServer.URL+"/api/v1/factory/stop", map[string]string{"mode": "drain", "reason": "deploy"}, bearer)
	if stop.StatusCode != http.StatusOK {
		stop.Body.Close()
		t.Fatalf("stop status = %d", stop.StatusCode)
	}
	var stopped FactoryState
	if err := json.NewDecoder(stop.Body).Decode(&stopped); err != nil {
		t.Fatal(err)
	}
	stop.Body.Close()
	if stopped.State != FactoryDraining {
		t.Fatalf("stop response = %#v", stopped)
	}
	if blocked := factoryPoll(t, server.store, "instance-c", "lane-c"); blocked != nil {
		t.Fatalf("HTTP pause did not stop dispatch: %#v", blocked)
	}

	capResponse := putJSON(t, webServer.URL+"/api/v1/factory/max-concurrent-jobs", map[string]int{"value": 5}, bearer)
	if capResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(capResponse.Body)
		capResponse.Body.Close()
		t.Fatalf("cap status = %d: %s", capResponse.StatusCode, body)
	}
	var capped FactoryState
	if err := json.NewDecoder(capResponse.Body).Decode(&capped); err != nil {
		t.Fatal(err)
	}
	capResponse.Body.Close()
	if capped.MaxConcurrentJobs != 3 {
		t.Fatalf("cap = %d, want 3 clamped to the ready lanes", capped.MaxConcurrentJobs)
	}

	laneResponse := putJSON(t, webServer.URL+"/api/v1/factory/lanes/lane-b", map[string]bool{"enabled": false}, bearer)
	if laneResponse.StatusCode != http.StatusOK {
		laneResponse.Body.Close()
		t.Fatalf("lane status = %d", laneResponse.StatusCode)
	}
	laneResponse.Body.Close()

	getFactory := func() FactoryState {
		t.Helper()
		request, err := http.NewRequest(http.MethodGet, webServer.URL+"/api/v1/factory", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer secret")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var state FactoryState
		if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	if state := getFactory(); state.State != FactoryDraining {
		t.Fatalf("GET factory state = %q", state.State)
	}

	// Settings writes cannot bypass the factory controls.
	settings := putJSON(t, webServer.URL+"/api/v1/settings/factory.max_concurrent_jobs", map[string]string{"value": "9"}, bearer)
	if settings.StatusCode != http.StatusInternalServerError {
		settings.Body.Close()
		t.Fatalf("reserved settings write status = %d, want 500", settings.StatusCode)
	}
	settings.Body.Close()

	start := postJSON(t, webServer.URL+"/api/v1/factory/start", nil, bearer)
	if start.StatusCode != http.StatusOK {
		start.Body.Close()
		t.Fatalf("start status = %d", start.StatusCode)
	}
	start.Body.Close()
	if state := getFactory(); state.State != FactoryRunning {
		t.Fatalf("state after resume = %q", state.State)
	}

	entries, err := server.store.AuditLog(t.Context(), 50)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, entry := range entries {
		if action, ok := entry["action"].(string); ok {
			actions[action] = true
		}
	}
	for _, want := range []string{"factory.drain", "factory.job_cap", "factory.lane_disable", "factory.start"} {
		if !actions[want] {
			t.Fatalf("audit log is missing %q: %#v", want, entries)
		}
	}
}
