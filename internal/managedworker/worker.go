package managedworker

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/protocol"
	"github.com/owainlewis/machinist/internal/runner"
	"github.com/owainlewis/machinist/internal/skills"
)

type Worker struct {
	config         config.Worker
	instanceID     string
	client         *Client
	stdout         io.Writer
	stderr         io.Writer
	heartbeatTicks <-chan time.Time
	executeRun     func(context.Context, protocol.RunSpec) protocol.Completion
	// memory is the per-job memory cap applied to every run (MACH-62).
	memory runner.MemoryLimit
	// pausedUntil stops this lane claiming more runs after a checkout it could
	// not clean, so one broken lane cannot drain the whole queue (MACH-59).
	pausedUntil time.Time
	// laneFailures records recent infrastructure failures so a lane that keeps
	// failing the same way trips the circuit breaker (MACH-62).
	laneFailures []laneFailure
}

// laneFailure is one infrastructure failure on this lane, used by the circuit
// breaker.
type laneFailure struct {
	at    time.Time
	class string
}

const heartbeatInterval = 10 * time.Second

// lanePauseCooldown is how long a lane stops claiming runs after it fails to
// save and clean a dirty checkout, giving an operator time to fix the lane
// instead of failing every queued job behind it.
const lanePauseCooldown = 5 * time.Minute

// breakerFailureWindow and breakerFailureThreshold trip the circuit breaker:
// this many infrastructure failures of the same class within the window pause
// the lane and raise one alert instead of draining the queue (MACH-62).
const (
	breakerFailureWindow    = time.Minute
	breakerFailureThreshold = 3
	breakerPauseCooldown    = 15 * time.Minute
)

func New(workerConfig config.Worker, stdout, stderr io.Writer) (*Worker, error) {
	if strings.TrimSpace(workerConfig.Name) == "" {
		return nil, errors.New("worker name is required")
	}
	if len(workerConfig.Executors) == 0 || len(workerConfig.Repositories) == 0 {
		return nil, errors.New("managed worker requires at least one executor and repository")
	}
	client, err := NewClient(workerConfig)
	if err != nil {
		return nil, err
	}
	instanceID, err := randomID("worker", 16)
	if err != nil {
		return nil, err
	}
	resolvedMemory, err := workerConfig.ResolveMemory()
	if err != nil {
		return nil, err
	}
	return &Worker{
		config:     workerConfig,
		instanceID: instanceID,
		client:     client,
		stdout:     stdout,
		stderr:     stderr,
		memory: runner.MemoryLimit{
			High:    resolvedMemory.High,
			Max:     resolvedMemory.Max,
			SwapMax: resolvedMemory.SwapMax,
		},
	}, nil
}

func (w *Worker) Run(ctx context.Context) error {
	// MACH-62: before clearing anything, check whether this process restarted
	// after an out-of-memory kill so the run it was executing is failed with
	// the OOM class instead of being re-leased into another lane.
	w.reportOOMDeath(ctx)
	// Drop any stale marker left by a previous process so the lane starts idle.
	w.clearLaneBusy()
	for {
		if until := w.pausedUntil; !until.IsZero() && time.Now().Before(until) {
			if !wait(ctx, time.Second) {
				return nil
			}
			continue
		}
		run, err := w.poll(ctx)
		if err != nil {
			if !wait(ctx, time.Second) {
				return nil
			}
			fmt.Fprintf(w.stderr, "machinist: worker poll: %v\n", err)
			continue
		}
		if run == nil {
			if !wait(ctx, time.Second) {
				return nil
			}
			continue
		}
		// Mark the lane busy for the whole run, including the final steps, so
		// a restart-when-idle never restarts it mid-final-step (MACH-59).
		w.markLaneBusy(run.ID)
		completion := w.executeWithHeartbeats(ctx, *run)
		w.handleLaneFailure(*run, completion)
		err = w.deliverWithHeartbeats(ctx, *run, completion)
		w.clearLaneBusy()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var responseErr *ResponseError
			if errors.As(err, &responseErr) && responseErr.Status == 409 {
				fmt.Fprintf(w.stderr, "machinist: report run %s: %v\n", run.ID, err)
				continue
			}
			return err
		}
	}
}

// reportOOMDeath tells the control plane that this worker restarted after an
// out-of-memory kill, so the run it was executing is failed with the OOM class
// and never re-leased into another lane (MACH-62). It is a no-op off Linux and
// when no OOM death is detected.
func (w *Worker) reportOOMDeath(ctx context.Context) {
	runID, peakBytes, died := workerOOMDeath(w.config.DataDirectory)
	if !died {
		return
	}
	fmt.Fprintf(w.stderr, "machinist: worker restarted after an OOM kill (peak %d bytes); run %q will not be re-leased\n", peakBytes, runID)
	if runID == "" {
		return
	}
	report := protocol.OOMReport{InstanceID: w.instanceID, Name: w.config.Name, PeakMemoryBytes: peakBytes,
		Detail: "worker was OOM-killed while running this job"}
	if err := w.client.Post(ctx, "/api/v1/runs/"+url.PathEscape(runID)+"/oom", report, nil); err != nil {
		fmt.Fprintf(w.stderr, "machinist: report OOM death for run %s: %v\n", runID, err)
	}
}

// laneLockPath is the lane's in-flight marker. A restart-when-idle script
// checks it so it never restarts the worker during a job's final steps.
func (w *Worker) laneLockPath() string {
	if strings.TrimSpace(w.config.DataDirectory) == "" {
		return ""
	}
	return filepath.Join(w.config.DataDirectory, "lane.lock")
}

// markLaneBusy records the in-flight run so the lane is not seen as idle.
func (w *Worker) markLaneBusy(runID string) {
	path := w.laneLockPath()
	if path == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	_ = os.WriteFile(path, []byte(runID+"\n"), 0o600)
}

// clearLaneBusy removes the in-flight marker once the run and its final steps
// are done, so the lane is idle again.
func (w *Worker) clearLaneBusy() {
	if path := w.laneLockPath(); path != "" {
		_ = os.Remove(path)
	}
}

func (w *Worker) executeWithHeartbeats(ctx context.Context, spec protocol.RunSpec) protocol.Completion {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go w.streamLiveEvents(ctx, spec)
	execute := w.executeRun
	if execute == nil {
		execute = w.execute
	}
	return withHeartbeats(ctx, w, spec, "", func() protocol.Completion { return execute(ctx, spec) }, cancel)
}

// liveEventsInterval controls how often the worker pushes new event log lines
// while a run executes. The run page polls the control plane about as often.
const liveEventsInterval = time.Second

// streamLiveEvents tails the run's event log and pushes new complete lines to
// the control plane so the run page can show a live transcript. Pushes are
// best effort: a lost chunk leaves a gap until the run's completion delivers
// the full event log.
func (w *Worker) streamLiveEvents(ctx context.Context, spec protocol.RunSpec) {
	eventsPath := runner.EventsFilePath(w.config.DataDirectory, spec.ID, spec.LeaseToken)
	ticker := time.NewTicker(liveEventsInterval)
	defer ticker.Stop()
	var sent int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		file, err := os.Open(eventsPath)
		if err != nil {
			continue
		}
		chunk, next, readErr := readCompleteLines(file, sent)
		_ = file.Close()
		if readErr != nil || next == sent {
			continue
		}
		err = w.client.Post(ctx, "/api/v1/runs/"+url.PathEscape(spec.ID)+"/events", protocol.LiveEvents{
			InstanceID: w.instanceID,
			LeaseToken: spec.LeaseToken,
			Events:     chunk,
		}, nil)
		if err != nil {
			if ctx.Err() != nil || stopLiveEvents(err) {
				return
			}
			continue
		}
		sent = next
	}
}

// stopLiveEvents reports whether a live events push failed in a way retrying
// cannot fix: the lease moved on, the log is full, or the control plane does
// not offer live events.
func stopLiveEvents(err error) bool {
	var responseErr *ResponseError
	if !errors.As(err, &responseErr) {
		return false
	}
	switch responseErr.Status {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusConflict, http.StatusRequestEntityTooLarge:
		return true
	}
	return false
}

// infrastructureFailure classifies a run failure as lane-level infrastructure
// trouble (MACH-62). pause is true when the lane itself must stop claiming
// jobs (a dirty checkout or a refused executor preflight); an out-of-memory
// kill fails only the run, so its lane stays usable. The OOM class matches
// both the recorded error_class and the "out of memory" message.
func infrastructureFailure(completion protocol.Completion) (class string, pause bool, ok bool) {
	if completion.State != "failed" {
		return "", false, false
	}
	if completion.ErrorClass == "oom" || strings.Contains(completion.Error, "out of memory in ") {
		return "oom", false, true
	}
	text := completion.Error + "\n" + completion.Events
	if completion.ExitCode == 4 || strings.Contains(text, "refusing to start") || strings.Contains(completion.Error, "lane checkout is dirty") {
		return "lane-dirty", true, true
	}
	return "", false, false
}

// laneCheckoutUnrecoverable reports whether a run failed because the lane
// checkout was dirty, either because it could not be saved and cleaned
// (MACH-59) or because the executor preflight refused it (exit 4, MACH-62).
func laneCheckoutUnrecoverable(completion protocol.Completion) bool {
	class, pause, ok := infrastructureFailure(completion)
	return ok && pause && class == "lane-dirty"
}

// handleLaneFailure pauses a lane that failed for lane-level infrastructure
// reasons and trips the circuit breaker when the same failure repeats within a
// minute (MACH-62), instead of draining the queue.
func (w *Worker) handleLaneFailure(spec protocol.RunSpec, completion protocol.Completion) {
	class, pause, ok := infrastructureFailure(completion)
	if !ok {
		return
	}
	if w.recordLaneFailure(class) {
		w.pauseLane(breakerPauseCooldown, "circuit breaker: "+class+" failures within a minute")
		w.raiseBreakerAlert(spec, class)
		return
	}
	if pause {
		w.pauseLane(lanePauseCooldown, "after a refused dirty checkout")
	}
}

// pauseLane stops this lane claiming runs for cooldown, never shortening an
// existing pause.
func (w *Worker) pauseLane(cooldown time.Duration, reason string) {
	until := time.Now().Add(cooldown)
	if until.After(w.pausedUntil) {
		w.pausedUntil = until
	}
	fmt.Fprintf(w.stderr, "machinist: lane %s paused for %s %s\n", w.config.Name, cooldown, reason)
}

// recordLaneFailure records one infrastructure failure and reports whether the
// circuit breaker should trip. It returns true once per burst: the matching
// class is cleared so three failures pause the lane and raise exactly one
// alert.
func (w *Worker) recordLaneFailure(class string) bool {
	now := time.Now()
	cutoff := now.Add(-breakerFailureWindow)
	recent := w.laneFailures[:0]
	for _, failure := range w.laneFailures {
		if failure.at.After(cutoff) {
			recent = append(recent, failure)
		}
	}
	w.laneFailures = append(recent, laneFailure{at: now, class: class})
	count := 0
	for _, failure := range w.laneFailures {
		if failure.class == class {
			count++
		}
	}
	if count < breakerFailureThreshold {
		return false
	}
	remaining := w.laneFailures[:0]
	for _, failure := range w.laneFailures {
		if failure.class != class {
			remaining = append(remaining, failure)
		}
	}
	w.laneFailures = remaining
	return true
}

// raiseBreakerAlert spools one alert for a tripped circuit breaker into the
// worker's alert directory so an operator sees why the lane stopped.
func (w *Worker) raiseBreakerAlert(spec protocol.RunSpec, class string) {
	fmt.Fprintf(w.stderr, "machinist: lane %s circuit breaker tripped (%s); alerting\n", w.config.Name, class)
	if strings.TrimSpace(w.config.DataDirectory) == "" {
		return
	}
	directory := filepath.Join(w.config.DataDirectory, "alerts")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		fmt.Fprintf(w.stderr, "machinist: create alert directory: %v\n", err)
		return
	}
	alert := struct {
		Type       string `json:"type"`
		Lane       string `json:"lane"`
		Class      string `json:"error_class"`
		RunID      string `json:"run_id"`
		JobID      string `json:"job_id"`
		DetectedAt string `json:"detected_at"`
	}{
		Type: "machinist.lane.circuit_breaker", Lane: w.config.Name, Class: class,
		RunID: spec.ID, JobID: spec.JobID, DetectedAt: time.Now().UTC().Format(time.RFC3339),
	}
	body, err := json.MarshalIndent(alert, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(directory, "circuit-breaker-"+w.config.Name+"-"+time.Now().UTC().Format("20060102-150405")+".json")
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		fmt.Fprintf(w.stderr, "machinist: write circuit breaker alert: %v\n", err)
	}
}

// completionReportTimeout bounds how long a finished run keeps trying to report
// its completion after shutdown was requested.
const completionReportTimeout = 2 * time.Minute

// readCompleteLines reads from offset and returns only bytes up to the last
// complete line, plus the offset to resume from. It returns the original
// offset when the file holds no new complete line.
func readCompleteLines(file *os.File, offset int64) (string, int64, error) {
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return "", offset, err
	}
	chunk, err := io.ReadAll(file)
	if err != nil {
		return "", offset, err
	}
	lastLine := bytes.LastIndexByte(chunk, '\n')
	if lastLine < 0 {
		return "", offset, nil
	}
	chunk = chunk[:lastLine+1]
	return string(chunk), offset + int64(len(chunk)), nil
}

func (w *Worker) deliverWithHeartbeats(ctx context.Context, spec protocol.RunSpec, completion protocol.Completion) error {
	if err := w.heartbeat(ctx, spec); err != nil {
		fmt.Fprintf(w.stderr, "machinist: heartbeat run %s before completion: %v\n", spec.ID, err)
	}
	return withHeartbeats(ctx, w, spec, " during completion", func() error { return w.deliver(ctx, spec.ID, completion) })
}

// withHeartbeats runs work in the background and keeps the run lease alive
// until it returns. Cancellation does not abandon the work; the work observes
// ctx itself and its result is always returned.
func withHeartbeats[T any](ctx context.Context, w *Worker, spec protocol.RunSpec, phase string, work func() T, stop ...context.CancelFunc) T {
	ticks := w.heartbeatTicks
	if ticks == nil {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	done := make(chan T, 1)
	go func() { done <- work() }()
	for {
		select {
		case result := <-done:
			return result
		case <-ticks:
			if err := w.heartbeat(ctx, spec); err != nil {
				fmt.Fprintf(w.stderr, "machinist: heartbeat run %s%s: %v\n", spec.ID, phase, err)
				var responseErr *ResponseError
				if spec.Workflow && len(stop) > 0 && errors.As(err, &responseErr) && (responseErr.Status == 409 || responseErr.Status == 404) {
					stop[0]()
				}
			}
		case <-ctx.Done():
			return <-done
		}
	}
}

func (w *Worker) poll(ctx context.Context) (*protocol.RunSpec, error) {
	request := protocol.PollRequest{
		SharedOutputs: true,
		Reviews:       true,
		Workflows:     true,
		Artifacts:     true,
		InstanceID:    w.instanceID,
		Name:          w.config.Name,
		Executors:     w.config.ExecutorNames(),
		Repositories:  w.config.RepositoryNames(),
		Models:        w.config.ModelCapabilities(),
	}
	var response protocol.PollResponse
	if err := w.client.Post(ctx, "/api/v1/workers/poll", request, &response); err != nil {
		return nil, err
	}
	return response.Run, nil
}

// reviewEnv exposes the control plane to trusted post-agent scripts (the
// built-in PR review step) so they can read per-project review instructions.
func (w *Worker) reviewEnv() []string {
	env := []string{"MACHINIST_CONTROL_PLANE_URL=" + strings.TrimSpace(w.config.ControlPlane.URL)}
	if token, err := w.config.WorkerToken(); err == nil {
		env = append(env, "MACHINIST_CONTROL_PLANE_TOKEN="+token)
	}
	return env
}

// ompAgentDirEnv points the omp executor wrapper at the per-run skills agent
// directory: skills/<name>/SKILL.md, RULES.md, and an omp config overlay.
const ompAgentDirEnv = "MACHINIST_OMP_AGENT_DIR"

// skillsSettingsResponse mirrors GET /api/v1/settings.
type skillsSettingsResponse struct {
	Settings map[string]string `json:"settings"`
}

// prepareSkills composes the run's skills from the repo at the base ref, the
// project setting skills.project.<project>, and the global setting
// skills.default, then injects them for the executor. omp gets a per-run agent
// directory; every other executor gets the SKILL.md text appended to its
// prompt. It returns the extra environment and the list recorded on the run.
// A required skill that cannot resolve fails the run; the optional layers
// degrade to the repo-scoped skills when the control plane is unreachable.
func (w *Worker) prepareSkills(ctx context.Context, spec protocol.RunSpec, command *config.ResolvedCommand, repository string) ([]string, *protocol.RunSkills, error) {
	bundle, err := skills.LoadRepoBundle(ctx, repository, "HEAD")
	if err != nil {
		return nil, nil, fmt.Errorf("read repository skills: %w", err)
	}
	var project, global []skills.Entry
	var settings skillsSettingsResponse
	if w.client != nil {
		if err := w.client.Get(ctx, "/api/v1/settings", &settings); err != nil {
			fmt.Fprintf(w.stderr, "machinist: run %s: read skill settings: %v\n", spec.ID, err)
		} else {
			if raw := strings.TrimSpace(settings.Settings["skills.default"]); raw != "" {
				if global, err = skills.ParseConfig([]byte(raw)); err != nil {
					return nil, nil, err
				}
			}
			if spec.Project != "" {
				if raw := strings.TrimSpace(settings.Settings["skills.project."+spec.Project]); raw != "" {
					if project, err = skills.ParseConfig([]byte(raw)); err != nil {
						return nil, nil, err
					}
				}
			}
		}
	}
	if len(bundle.Entries) == 0 && len(project) == 0 && len(global) == 0 {
		return nil, nil, nil
	}
	library, err := skills.Open("")
	if err != nil {
		fmt.Fprintf(w.stderr, "machinist: run %s: open skills library: %v\n", spec.ID, err)
	}
	resolver := &skills.Resolver{
		Library: library,
		Warn:    func(message string) { fmt.Fprintf(w.stderr, "machinist: run %s: %s\n", spec.ID, message) },
	}
	resolution, err := resolver.Resolve(skills.ResolveRequest{
		Project:        spec.Project,
		Kind:           skillKind(spec),
		Repo:           bundle.Entries,
		ProjectEntries: project,
		Global:         global,
		Bundle:         bundle,
	})
	if err != nil {
		return nil, nil, err
	}
	if len(resolution.Skills) == 0 && len(resolution.Warnings) == 0 {
		return nil, nil, nil
	}
	recorded := &protocol.RunSkills{Warnings: resolution.Warnings}
	if len(resolution.Skills) == 0 {
		return nil, recorded, nil
	}
	if strings.TrimSpace(w.config.DataDirectory) == "" {
		return nil, recorded, nil
	}
	native := runner.IsOmpExecutor(spec.Executor, command.Command)
	agentDir := filepath.Join(w.config.DataDirectory, "runs", spec.ID, "agent")
	injected, err := skills.BuildInjection(resolution, agentDir, native)
	if err != nil {
		return nil, nil, err
	}
	for _, skill := range injected.Skills {
		recorded.Skills = append(recorded.Skills, protocol.LoadedSkill{
			Name: skill.Name, Version: skill.Version, Scope: skill.Scope, Method: skill.Method,
		})
	}
	if native {
		return []string{ompAgentDirEnv + "=" + agentDir}, recorded, nil
	}
	command.Prompt = strings.TrimRight(command.Prompt, "\n") + "\n\n" + injected.Prompt
	return nil, recorded, nil
}

// skillKind classifies a run as a build or a review from its command and
// executor so an entry's applies_to list can select it.
func skillKind(spec protocol.RunSpec) string {
	if strings.Contains(strings.ToLower(spec.Command+" "+spec.Executor), "review") {
		return "review"
	}
	return "build"
}

func (w *Worker) execute(ctx context.Context, spec protocol.RunSpec) protocol.Completion {
	completion := protocol.Completion{InstanceID: w.instanceID, LeaseToken: spec.LeaseToken, State: "failed", ExitCode: 1}
	repository, err := w.config.ResolveRepository(spec.Repository)
	if err != nil {
		completion.Error = err.Error()
		return completion
	}
	command, err := w.config.ResolveCommandModel(config.ResolvedCommand{
		Name:       spec.Command,
		Executor:   spec.Executor,
		Prompt:     spec.RenderedPrompt,
		Timeout:    time.Duration(spec.TimeoutMillis) * time.Millisecond,
		Definition: "control-plane",
		Hash:       spec.CommandHash,
	}, spec.Model)
	if err != nil {
		completion.Error = err.Error()
		return completion
	}
	extraEnv := w.reviewEnv()
	skillsEnv, runSkills, err := w.prepareSkills(ctx, spec, &command, repository)
	if err != nil {
		completion.Error = err.Error()
		return completion
	}
	completion.Skills = runSkills
	extraEnv = append(extraEnv, skillsEnv...)
	result, runErr := runner.Execute(ctx, runner.Options{
		Revision: spec.Revision,
		Workflow: spec.Workflow, JobID: spec.JobID,
		Task: spec.Task, PrepareInputs: w.prepareInputs(spec),
		RunID:         spec.ID,
		ArtifactKey:   spec.LeaseToken,
		Command:       command,
		Repository:    repository,
		MemoryLimit:   w.memory,
		ExtraEnv:      extraEnv,
		DataDirectory: w.config.DataDirectory,
		Stdout:        w.stdout,
		Stderr:        w.stderr,
	})
	if result.ID != "" {
		completion.State = string(result.State)
		completion.ExitCode = result.ExitCode
		completion.ErrorClass = result.ErrorClass
		completion.PeakMemoryBytes = result.PeakMemoryBytes
		completion.Result, _ = os.ReadFile(filepath.Join(filepath.Dir(result.EventsPath), "result.json"))
		if events, readErr := os.ReadFile(result.EventsPath); readErr == nil {
			completion.Events = string(events)
		}
	}
	if spec.Task != nil && result.EventsPath != "" {
		// Publishing the run outputs is part of the job's final steps, so a
		// shutdown must not abandon it (MACH-59).
		publishCtx, cancelPublish := context.WithTimeout(context.WithoutCancel(ctx), completionReportTimeout)
		ids, err := w.publishOutputs(publishCtx, spec, filepath.Join(filepath.Dir(result.EventsPath), "outputs"))
		cancelPublish()
		completion.Artifacts = ids
		if err != nil {
			completion.PublicationError = err.Error()
		}
	}
	if runErr != nil {
		completion.Error = runErr.Error()
	}
	return completion
}

func (w *Worker) deliver(ctx context.Context, runID string, completion protocol.Completion) error {
	// The completion report is the run's final step, so a shutdown (e.g. a
	// restart-when-idle) must not abandon it: detach it from cancellation and
	// bound how long it may keep retrying (MACH-59).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), completionReportTimeout)
	defer cancel()
	backoff := 250 * time.Millisecond
	for {
		err := w.client.Post(ctx, "/api/v1/runs/"+url.PathEscape(runID)+"/complete", completion, nil)
		if err == nil {
			return nil
		}
		var responseErr *ResponseError
		if errors.As(err, &responseErr) && !responseErr.Retryable() {
			return err
		}
		fmt.Fprintf(w.stderr, "machinist: report run %s: %v\n", runID, err)
		if !wait(ctx, backoff) {
			return ctx.Err()
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

func (w *Worker) heartbeat(ctx context.Context, spec protocol.RunSpec) error {
	return w.client.Post(ctx, "/api/v1/runs/"+url.PathEscape(spec.ID)+"/heartbeat", protocol.Heartbeat{
		InstanceID: w.instanceID,
		LeaseToken: spec.LeaseToken,
	}, nil)
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func randomID(prefix string, byteCount int) (string, error) {
	body := make([]byte, byteCount)
	if _, err := rand.Read(body); err != nil {
		return "", err
	}
	return prefix + "_" + hex.EncodeToString(body), nil
}
