package managedworker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/controlplane"
	"github.com/owainlewis/machinist/internal/protocol"
)

func TestManagedWorkerExecutesControlPlaneRun(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repository")
	if output, err := exec.Command("git", "init", "--quiet", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	scriptDirectory := filepath.Join(repository, "scripts")
	if err := os.MkdirAll(scriptDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nset -eu\ninput=$(cat)\n[ \"$input\" = \"managed request\" ]\n[ \"$(pwd)\" = \"$MACHINIST_REPOSITORY\" ]\nprintf managed-output\nprintf managed-error >&2\nprintf 2468 > \"$MACHINIST_TOKEN_USAGE_PATH\"\n"
	if err := os.WriteFile(filepath.Join(scriptDirectory, "workflow.sh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	definitionPath := filepath.Join(directory, "config.toml")
	promptPath := filepath.Join(directory, "plan.md")
	if err := os.WriteFile(promptPath, []byte("{{machinist.prompt}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(definitionPath, []byte("[commands.plan]\nexecutor=\"test\"\nprompt_file=\"plan.md\"\ntimeout=\"5s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := controlplane.OpenStore(filepath.Join(directory, "machinist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	agent, err := config.LoadCommand(definitionPath, "plan")
	if err != nil {
		t.Fatal(err)
	}
	agent, err = config.RenderPrompt(agent, "managed request")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateJob(t.Context(), "managed request", "machinist", "plan", agent); err != nil {
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
		Name:          "local-test",
		DataDirectory: filepath.Join(directory, "worker-data"),
		ControlPlane:  config.ControlPlane{URL: httpServer.URL, TokenFile: tokenPath},
		Executors:     map[string]config.Executor{"test": {Command: []string{"./scripts/workflow.sh"}}},
		Repositories:  map[string]config.Repository{"machinist": {Path: repository}},
	}, os.Stdout, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, err := store.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Jobs) == 1 && snapshot.Jobs[0].State == "succeeded" {
			run := snapshot.Jobs[0].Runs[0]
			output, outputErr := store.RunOutput(t.Context(), run.ID)
			if run.State != "succeeded" || outputErr != nil || output.Events == "" || output.Result == "" || run.WorkerName != "local-test" || run.DurationMillis == nil || run.TokenUsage == nil || *run.TokenUsage != 2468 {
				t.Fatalf("completed run = %#v", run)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job did not complete: %#v", snapshot.Jobs)
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestManagedWorkerExecutesQueuedCommandWithRenderedPrompt(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repository")
	if output, err := exec.Command("git", "init", "--quiet", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	definitionPath := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(filepath.Join(directory, "audit.md"), []byte("Scheduled request:\n{{machinist.prompt}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	definition := `[commands.audit]
executor = "test"
prompt_file = "audit.md"
timeout = "5s"
`
	if err := os.WriteFile(definitionPath, []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	command, err := config.LoadCommand(definitionPath, "audit")
	if err != nil {
		t.Fatal(err)
	}
	command, err = config.RenderPrompt(command, "Perform at most 2 mutating actions in this run.")
	if err != nil {
		t.Fatal(err)
	}
	store, err := controlplane.OpenStore(filepath.Join(directory, "machinist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.CreateJob(t.Context(), "queued audit", "disposable", "audit", command); err != nil {
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
		Name:          "audit-worker",
		DataDirectory: filepath.Join(directory, "worker-data"),
		ControlPlane:  config.ControlPlane{URL: httpServer.URL, TokenFile: tokenPath},
		Executors:     map[string]config.Executor{"test": {Command: []string{"/bin/sh", "-c", `input=$(cat); case "$input" in *"at most 2 mutating actions"*) exit 0;; *) exit 7;; esac`}}},
		Repositories:  map[string]config.Repository{"disposable": {Path: repository}},
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, err := store.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Jobs) == 1 && snapshot.Jobs[0].State == "succeeded" {
			if snapshot.Jobs[0].Runs[0].Command != "audit" {
				t.Fatalf("queued job = %#v", snapshot.Jobs[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("queued job did not complete: %#v", snapshot.Jobs)
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestManagedWorkerReportsPreflightFailureWithoutStartingRun(t *testing.T) {
	directory := t.TempDir()
	worker := &Worker{
		config: config.Worker{
			DataDirectory: directory,
			Executors:     map[string]config.Executor{},
			Repositories:  map[string]config.Repository{},
		},
		instanceID: "worker-test",
	}
	completion := worker.execute(t.Context(), protocol.RunSpec{ID: "run_0123456789abcdef01234567", Executor: "missing", Repository: "missing", LeaseToken: "lease"})
	if completion.State != "failed" || completion.Error == "" || completion.Result != nil || completion.Events != "" {
		t.Fatalf("completion = %#v", completion)
	}
	if _, err := os.Stat(filepath.Join(directory, "runs")); !os.IsNotExist(err) {
		t.Fatalf("runner started during preflight: %v", err)
	}
}

func TestManagedWorkerRequiresHTTPSForRemoteControlPlane(t *testing.T) {
	_, err := New(config.Worker{
		Name:         "remote",
		ControlPlane: config.ControlPlane{URL: "http://10.0.0.5:7331", TokenFile: "unused"},
		Executors:    map[string]config.Executor{"test": {Command: []string{"agent"}}},
		Repositories: map[string]config.Repository{"machinist": {Path: "."}},
	}, os.Stdout, os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "must use https") {
		t.Fatalf("error = %v", err)
	}
}

func TestControlPlaneEndpointRejectsQueryAndFragment(t *testing.T) {
	for _, endpoint := range []string{
		"https://control.example.com?tenant=machinist",
		"https://control.example.com?",
		"https://control.example.com#api",
		"https://control.example.com#",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := controlPlaneEndpoint(endpoint); err == nil {
				t.Fatalf("controlPlaneEndpoint(%q) succeeded", endpoint)
			}
		})
	}
}

func TestCompletionDoesNotRetryPermanentClientError(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(response, "too large", http.StatusRequestEntityTooLarge)
	}))
	defer server.Close()
	worker := &Worker{
		config:     config.Worker{ControlPlane: config.ControlPlane{URL: server.URL}},
		instanceID: "worker-test",
		client:     newClient(server.URL, "secret", server.Client()),
		stderr:     io.Discard,
	}
	err := worker.deliver(t.Context(), "run_0123456789abcdef01234567", protocol.Completion{})
	if err == nil || requests.Load() != 1 {
		t.Fatalf("error = %v, requests = %d", err, requests.Load())
	}
}

func TestManagedWorkerPollsAgainAfterCompletionConflict(t *testing.T) {
	var polls atomic.Int32
	polledAgain := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/workers/poll":
			if polls.Add(1) == 1 {
				if err := json.NewEncoder(response).Encode(protocol.PollResponse{Run: &protocol.RunSpec{ID: "run-test", LeaseToken: "lease-test"}}); err != nil {
					t.Errorf("encode poll response: %v", err)
				}
				return
			}
			close(polledAgain)
			if err := json.NewEncoder(response).Encode(protocol.PollResponse{}); err != nil {
				t.Errorf("encode poll response: %v", err)
			}
		case "/api/v1/runs/run-test/heartbeat":
			response.WriteHeader(http.StatusNoContent)
		case "/api/v1/runs/run-test/complete":
			http.Error(response, "run lease does not match", http.StatusConflict)
		default:
			t.Errorf("unexpected request path %q", request.URL.Path)
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	var stderr strings.Builder
	worker := &Worker{
		config:     config.Worker{ControlPlane: config.ControlPlane{URL: server.URL}},
		instanceID: "worker-test",
		client:     newClient(server.URL, "secret", server.Client()),
		stderr:     &stderr,
		executeRun: func(context.Context, protocol.RunSpec) protocol.Completion {
			return protocol.Completion{InstanceID: "worker-test", LeaseToken: "lease-test", State: "succeeded"}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	select {
	case <-polledAgain:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not poll again after completion conflict")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("worker returned error: %v", err)
	}
	if !strings.Contains(stderr.String(), "machinist: report run run-test: control plane returned HTTP 409 Conflict") {
		t.Fatalf("completion conflict was not logged: %q", stderr.String())
	}
}

func TestManagedWorkerHeartbeatsDuringExecutionAndContinuesAfterFailure(t *testing.T) {
	if heartbeatInterval != 10*time.Second {
		t.Fatalf("heartbeat interval = %v", heartbeatInterval)
	}
	var requests atomic.Int32
	heartbeats := make(chan protocol.Heartbeat, 2)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/runs/run-test/heartbeat" || request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("request = %s authorization %q", request.URL.Path, request.Header.Get("Authorization"))
		}
		var heartbeat protocol.Heartbeat
		if err := json.NewDecoder(request.Body).Decode(&heartbeat); err != nil {
			t.Errorf("decode heartbeat: %v", err)
		}
		heartbeats <- heartbeat
		if requests.Add(1) == 1 {
			http.Error(response, "temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ticks := make(chan time.Time, 2)
	started := make(chan struct{})
	release := make(chan struct{})
	var stderr strings.Builder
	worker := &Worker{
		config:         config.Worker{ControlPlane: config.ControlPlane{URL: server.URL}},
		instanceID:     "worker-test",
		client:         newClient(server.URL, "secret", server.Client()),
		stderr:         &stderr,
		heartbeatTicks: ticks,
		executeRun: func(context.Context, protocol.RunSpec) protocol.Completion {
			close(started)
			<-release
			return protocol.Completion{State: "succeeded"}
		},
	}
	spec := protocol.RunSpec{ID: "run-test", LeaseToken: "lease-test"}
	done := make(chan protocol.Completion, 1)
	go func() { done <- worker.executeWithHeartbeats(t.Context(), spec) }()
	<-started

	for index := 0; index < 2; index++ {
		ticks <- time.Time{}
		heartbeat := <-heartbeats
		if heartbeat.InstanceID != "worker-test" || heartbeat.LeaseToken != "lease-test" {
			t.Fatalf("heartbeat = %#v", heartbeat)
		}
	}
	select {
	case completion := <-done:
		t.Fatalf("agent stopped after heartbeat failure: %#v", completion)
	default:
	}
	close(release)
	completion := <-done
	if completion.State != "succeeded" || requests.Load() != 2 {
		t.Fatalf("completion = %#v, requests = %d", completion, requests.Load())
	}
	if !strings.Contains(stderr.String(), "machinist: heartbeat run run-test") {
		t.Fatalf("heartbeat failure was not logged: %q", stderr.String())
	}
}

func TestManagedWorkerHeartbeatsUntilCompletionIsAcknowledged(t *testing.T) {
	heartbeats := make(chan protocol.Heartbeat, 2)
	completionStarted := make(chan struct{})
	releaseCompletion := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(releaseCompletion) }) })
	var completionRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/api/v1/runs/run-test/heartbeat":
			var heartbeat protocol.Heartbeat
			if err := json.NewDecoder(request.Body).Decode(&heartbeat); err != nil {
				t.Errorf("decode heartbeat: %v", err)
			}
			heartbeats <- heartbeat
			response.WriteHeader(http.StatusNoContent)
		case "/api/v1/runs/run-test/complete":
			if completionRequests.Add(1) == 1 {
				close(completionStarted)
				<-releaseCompletion
				http.Error(response, "temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
			response.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request path %q", request.URL.Path)
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	ticks := make(chan time.Time, 1)
	worker := &Worker{
		config:         config.Worker{ControlPlane: config.ControlPlane{URL: server.URL}},
		instanceID:     "worker-test",
		client:         newClient(server.URL, "secret", server.Client()),
		stderr:         io.Discard,
		heartbeatTicks: ticks,
	}
	spec := protocol.RunSpec{ID: "run-test", LeaseToken: "lease-test"}
	done := make(chan error, 1)
	go func() {
		done <- worker.deliverWithHeartbeats(t.Context(), spec, protocol.Completion{InstanceID: "worker-test", LeaseToken: "lease-test", State: "succeeded"})
	}()

	immediate := <-heartbeats
	if immediate.InstanceID != "worker-test" || immediate.LeaseToken != "lease-test" {
		t.Fatalf("immediate heartbeat = %#v", immediate)
	}
	<-completionStarted
	ticks <- time.Time{}
	duringRetry := <-heartbeats
	if duringRetry.InstanceID != "worker-test" || duringRetry.LeaseToken != "lease-test" {
		t.Fatalf("completion heartbeat = %#v", duringRetry)
	}
	releaseOnce.Do(func() { close(releaseCompletion) })
	if err := <-done; err != nil || completionRequests.Load() != 2 {
		t.Fatalf("delivery error = %v, completion requests = %d", err, completionRequests.Load())
	}
}

func TestManagedWorkerSeparatesRedispatchedLeaseArtifacts(t *testing.T) {
	directory := t.TempDir()
	repository := filepath.Join(directory, "repository")
	if output, err := exec.Command("git", "init", "--quiet", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	worker := &Worker{
		config: config.Worker{
			DataDirectory: directory,
			Executors:     map[string]config.Executor{"test": {Command: []string{"/bin/sh", "-c", "cat >/dev/null"}}},
			Repositories:  map[string]config.Repository{"machinist": {Path: repository}},
		},
		instanceID: "worker-test",
		stdout:     io.Discard,
		stderr:     io.Discard,
	}
	runID := "run_0123456789abcdef01234567"
	for _, lease := range []string{"lease_first", "lease_second"} {
		completion := worker.execute(t.Context(), protocol.RunSpec{
			ID:             runID,
			Command:        "plan",
			CommandHash:    "plan-hash",
			Executor:       "test",
			Repository:     "machinist",
			RenderedPrompt: "managed prompt",
			TimeoutMillis:  5000,
			LeaseToken:     lease,
		})
		if completion.State != "succeeded" || completion.Result == nil || completion.Events == "" {
			t.Fatalf("completion for %s = %#v", lease, completion)
		}
		for _, name := range []string{"events.jsonl", "result.json"} {
			if _, err := os.Stat(filepath.Join(directory, "runs", runID, lease, name)); err != nil {
				t.Fatalf("artifact %s for %s: %v", name, lease, err)
			}
		}
	}
}

func TestManagedWorkerStopsHeartbeatLoopWhenTerminated(t *testing.T) {
	ticks := make(chan time.Time, 1)
	started := make(chan struct{})
	exited := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	worker := &Worker{
		heartbeatTicks: ticks,
		executeRun: func(ctx context.Context, _ protocol.RunSpec) protocol.Completion {
			close(started)
			<-ctx.Done()
			close(exited)
			return protocol.Completion{State: "cancelled", ExitCode: 130}
		},
	}
	done := make(chan protocol.Completion, 1)
	go func() { done <- worker.executeWithHeartbeats(ctx, protocol.RunSpec{}) }()
	<-started
	cancel()
	<-exited
	completion := <-done
	if completion.State != "cancelled" || completion.ExitCode != 130 {
		t.Fatalf("completion = %#v", completion)
	}
	ticks <- time.Time{}
}

func TestManagedWorkerPausesLaneAfterUnsavableDirtyCheckout(t *testing.T) {
	directory := t.TempDir()
	definitionPath := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(definitionPath, []byte("[commands.plan]\nexecutor=\"test\"\nprompt_file=\"plan.md\"\ntimeout=\"5s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "plan.md"), []byte("{{machinist.prompt}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := controlplane.OpenStore(filepath.Join(directory, "machinist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	command := config.ResolvedCommand{Name: "plan", Executor: "test", Command: []string{"/bin/sh", "-c", "true"}, Timeout: time.Second, Definition: "test", Hash: "hash"}
	for _, prompt := range []string{"first", "second"} {
		if _, err := store.CreateJob(t.Context(), prompt, "repo", "plan", command); err != nil {
			t.Fatal(err)
		}
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
		Name:          "lane-1",
		DataDirectory: filepath.Join(directory, "worker-data"),
		ControlPlane:  config.ControlPlane{URL: httpServer.URL, TokenFile: tokenPath},
		Executors:     map[string]config.Executor{"test": {Command: []string{"/bin/sh", "-c", "true"}}},
		Repositories:  map[string]config.Repository{"repo": {Path: directory}},
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	worker.executeRun = func(_ context.Context, spec protocol.RunSpec) protocol.Completion {
		return protocol.Completion{InstanceID: worker.instanceID, LeaseToken: spec.LeaseToken, State: "failed", ExitCode: 1,
			Error: "lane checkout is dirty and could not be saved: boom"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	countRuns := func() (failed, queued int) {
		snapshot, err := store.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, job := range snapshot.Jobs {
			for _, run := range job.Runs {
				switch run.State {
				case "failed":
					failed++
				case "queued":
					queued++
				}
			}
		}
		return failed, queued
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if failed, _ := countRuns(); failed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first run did not fail")
		}
		time.Sleep(25 * time.Millisecond)
	}
	// MACH-59: the lane pauses on an unsavable dirty checkout instead of
	// claiming and failing the next queued job.
	time.Sleep(1500 * time.Millisecond)
	failed, queued := countRuns()
	if failed != 1 || queued != 1 {
		t.Fatalf("lane drained the queue: failed=%d queued=%d", failed, queued)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestWorkerLaneLockTracksTheFinalSteps(t *testing.T) {
	directory := t.TempDir()
	definitionPath := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(definitionPath, []byte("[commands.plan]\nexecutor=\"test\"\nprompt_file=\"plan.md\"\ntimeout=\"5s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "plan.md"), []byte("{{machinist.prompt}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := controlplane.OpenStore(filepath.Join(directory, "machinist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	command := config.ResolvedCommand{Name: "plan", Executor: "test", Command: []string{"/bin/sh", "-c", "true"}, Timeout: time.Second, Definition: "test", Hash: "hash"}
	if _, err := store.CreateJob(t.Context(), "one", "repo", "plan", command); err != nil {
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
	dataDirectory := filepath.Join(directory, "worker-data")
	worker, err := New(config.Worker{
		Name:          "lane-1",
		DataDirectory: dataDirectory,
		ControlPlane:  config.ControlPlane{URL: httpServer.URL, TokenFile: tokenPath},
		Executors:     map[string]config.Executor{"test": {Command: []string{"/bin/sh", "-c", "true"}}},
		Repositories:  map[string]config.Repository{"repo": {Path: directory}},
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(dataDirectory, "lane.lock")
	seenBusy := make(chan bool, 1)
	worker.executeRun = func(_ context.Context, spec protocol.RunSpec) protocol.Completion {
		_, statErr := os.Stat(lockPath)
		seenBusy <- statErr == nil
		return protocol.Completion{InstanceID: worker.instanceID, LeaseToken: spec.LeaseToken, State: "succeeded"}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()

	select {
	case busy := <-seenBusy:
		if !busy {
			t.Fatal("lane lock missing while the run executes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not start")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		snapshot, err := store.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Jobs) == 1 && snapshot.Jobs[0].State == "succeeded" {
			if _, statErr := os.Stat(lockPath); !os.IsNotExist(statErr) {
				t.Fatalf("lane lock not cleared after the final steps: %v", statErr)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not complete")
		}
		time.Sleep(25 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCompletionReportSurvivesShutdown(t *testing.T) {
	// MACH-59: the completion report is the run's final step, so a
	// restart-when-idle must not abandon it. deliver() detaches from ctx.
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete") {
			select {
			case received <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker := &Worker{client: newClient(server.URL, "secret", server.Client()), stderr: io.Discard, instanceID: "worker"}
	if err := worker.deliver(ctx, "run_0123456789abcdef01234567", protocol.Completion{InstanceID: "worker", LeaseToken: "lease", State: "succeeded"}); err != nil {
		t.Fatalf("deliver after shutdown: %v", err)
	}
	select {
	case <-received:
	case <-time.After(2 * time.Second):
		t.Fatal("completion was not reported after shutdown")
	}
}

func TestWorkflowStopsProcessAfterLeaseRejection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusConflict) }))
	defer server.Close()
	ticks := make(chan time.Time, 1)
	started := make(chan struct{})
	worker := &Worker{client: newClient(server.URL, "secret", server.Client()), stderr: io.Discard, instanceID: "worker", heartbeatTicks: ticks,
		executeRun: func(ctx context.Context, spec protocol.RunSpec) protocol.Completion {
			close(started)
			<-ctx.Done()
			return protocol.Completion{State: "cancelled", ExitCode: 130}
		},
	}
	done := make(chan protocol.Completion, 1)
	go func() { done <- worker.executeWithHeartbeats(t.Context(), protocol.RunSpec{ID: "run", Workflow: true}) }()
	<-started
	ticks <- time.Now()
	select {
	case result := <-done:
		if result.State != "cancelled" {
			t.Fatal(result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("workflow kept executing after definitive lease rejection")
	}
}

func TestInfrastructureFailureClassification(t *testing.T) {
	cases := []struct {
		name       string
		completion protocol.Completion
		wantClass  string
		wantPause  bool
		wantOK     bool
	}{
		{"oom class", protocol.Completion{State: "failed", ErrorClass: "oom"}, "oom", false, true},
		{"oom message", protocol.Completion{State: "failed", ExitCode: 137, Error: "out of memory in omp, peak 24.0 GB"}, "oom", false, true},
		{"unsavable dirty", protocol.Completion{State: "failed", Error: "lane checkout is dirty and could not be saved"}, "lane-dirty", true, true},
		{"preflight exit 4", protocol.Completion{State: "failed", ExitCode: 4, Error: "command exited with status 4", Events: "omp-glm-pr: refusing to start: has uncommitted changes"}, "lane-dirty", true, true},
		{"ordinary failure", protocol.Completion{State: "failed", ExitCode: 1, Error: "compile error"}, "", false, false},
		{"success", protocol.Completion{State: "succeeded"}, "", false, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			class, pause, ok := infrastructureFailure(testCase.completion)
			if class != testCase.wantClass || pause != testCase.wantPause || ok != testCase.wantOK {
				t.Fatalf("got (%q,%v,%v), want (%q,%v,%v)", class, pause, ok, testCase.wantClass, testCase.wantPause, testCase.wantOK)
			}
		})
	}
}

func TestCircuitBreakerTripsOncePerBurst(t *testing.T) {
	worker := &Worker{config: config.Worker{Name: "lane-1", DataDirectory: t.TempDir()}, stderr: io.Discard}
	if worker.recordLaneFailure("lane-dirty") {
		t.Fatal("breaker tripped before the threshold")
	}
	if worker.recordLaneFailure("lane-dirty") {
		t.Fatal("breaker tripped before the threshold")
	}
	if !worker.recordLaneFailure("lane-dirty") {
		t.Fatal("breaker did not trip at the threshold")
	}
	// The burst is cleared, so a fourth failure does not alert again.
	if worker.recordLaneFailure("lane-dirty") {
		t.Fatal("breaker alerted twice in one burst")
	}
	// A different class has its own count.
	if worker.recordLaneFailure("oom") {
		t.Fatal("unrelated class counted toward the dirty burst")
	}
}

func TestCircuitBreakerPausesLaneAndRaisesOneAlert(t *testing.T) {
	directory := t.TempDir()
	worker := &Worker{config: config.Worker{Name: "lane-1", DataDirectory: directory}, stderr: io.Discard}
	spec := protocol.RunSpec{ID: "run_x", JobID: "job_x"}
	completion := protocol.Completion{State: "failed", ErrorClass: "oom", Error: "out of memory in omp, peak 24.0 GB"}
	for i := 0; i < breakerFailureThreshold; i++ {
		worker.handleLaneFailure(spec, completion)
	}
	if worker.pausedUntil.IsZero() || !time.Now().Before(worker.pausedUntil) {
		t.Fatalf("lane not paused: %v", worker.pausedUntil)
	}
	alerts, err := filepath.Glob(filepath.Join(directory, "alerts", "circuit-breaker-*.json"))
	if err != nil || len(alerts) != 1 {
		t.Fatalf("breaker alerts = %v, %v", alerts, err)
	}
}

func TestManagedWorkerPausesLaneOnPreflightRefusal(t *testing.T) {
	directory := t.TempDir()
	definitionPath := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(definitionPath, []byte("[commands.plan]\nexecutor=\"test\"\nprompt_file=\"plan.md\"\ntimeout=\"5s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "plan.md"), []byte("{{machinist.prompt}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := controlplane.OpenStore(filepath.Join(directory, "machinist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	command := config.ResolvedCommand{Name: "plan", Executor: "test", Command: []string{"/bin/sh", "-c", "true"}, Timeout: time.Second, Definition: "test", Hash: "hash"}
	for _, prompt := range []string{"first", "second"} {
		if _, err := store.CreateJob(t.Context(), prompt, "repo", "plan", command); err != nil {
			t.Fatal(err)
		}
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
		Name:          "lane-1",
		DataDirectory: filepath.Join(directory, "worker-data"),
		ControlPlane:  config.ControlPlane{URL: httpServer.URL, TokenFile: tokenPath},
		Executors:     map[string]config.Executor{"test": {Command: []string{"/bin/sh", "-c", "true"}}},
		Repositories:  map[string]config.Repository{"repo": {Path: directory}},
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	worker.executeRun = func(_ context.Context, spec protocol.RunSpec) protocol.Completion {
		return protocol.Completion{InstanceID: worker.instanceID, LeaseToken: spec.LeaseToken, State: "failed", ExitCode: 4,
			Error: "command exited with status 4", Events: `{"type":"process.output","stream":"stderr","data":"omp-glm-pr: refusing to start: repo has uncommitted changes"}`}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	countRuns := func() (failed, queued int) {
		snapshot, err := store.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, job := range snapshot.Jobs {
			for _, run := range job.Runs {
				switch run.State {
				case "failed":
					failed++
				case "queued":
					queued++
				}
			}
		}
		return failed, queued
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if failed, _ := countRuns(); failed == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first run did not fail")
		}
		time.Sleep(25 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond)
	failed, queued := countRuns()
	if failed != 1 || queued != 1 {
		t.Fatalf("lane drained the queue after preflight refusal: failed=%d queued=%d", failed, queued)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestManagedWorkerKeepsLaneUsableAfterOOM(t *testing.T) {
	directory := t.TempDir()
	definitionPath := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(definitionPath, []byte("[commands.plan]\nexecutor=\"test\"\nprompt_file=\"plan.md\"\ntimeout=\"5s\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "plan.md"), []byte("{{machinist.prompt}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := controlplane.OpenStore(filepath.Join(directory, "machinist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	command := config.ResolvedCommand{Name: "plan", Executor: "test", Command: []string{"/bin/sh", "-c", "true"}, Timeout: time.Second, Definition: "test", Hash: "hash"}
	for _, prompt := range []string{"first", "second"} {
		if _, err := store.CreateJob(t.Context(), prompt, "repo", "plan", command); err != nil {
			t.Fatal(err)
		}
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
		Name:          "lane-1",
		DataDirectory: filepath.Join(directory, "worker-data"),
		ControlPlane:  config.ControlPlane{URL: httpServer.URL, TokenFile: tokenPath},
		Executors:     map[string]config.Executor{"test": {Command: []string{"/bin/sh", "-c", "true"}}},
		Repositories:  map[string]config.Repository{"repo": {Path: directory}},
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	worker.executeRun = func(_ context.Context, spec protocol.RunSpec) protocol.Completion {
		calls++
		if calls == 1 {
			// MACH-62: only the run fails; the worker survives and the lane
			// stays usable, so the next queued job runs normally.
			return protocol.Completion{InstanceID: worker.instanceID, LeaseToken: spec.LeaseToken, State: "failed", ExitCode: 137,
				Error: "out of memory in plan, peak 24.0 GB", ErrorClass: "oom", PeakMemoryBytes: 24 << 30}
		}
		return protocol.Completion{InstanceID: worker.instanceID, LeaseToken: spec.LeaseToken, State: "succeeded", ExitCode: 0}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	countRuns := func() (failed, succeeded int) {
		snapshot, err := store.Snapshot(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, job := range snapshot.Jobs {
			for _, run := range job.Runs {
				switch run.State {
				case "failed":
					failed++
				case "succeeded":
					succeeded++
				}
			}
		}
		return failed, succeeded
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		failed, succeeded := countRuns()
		if failed == 1 && succeeded == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lane did not run the next job after the OOM: %#v", store)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !worker.pausedUntil.IsZero() {
		t.Fatalf("lane paused after an OOM: %v", worker.pausedUntil)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
