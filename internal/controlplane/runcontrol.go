package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/protocol"
)

// Run controls (MACH-45). Restart, Resume and Poke share one implementation so
// the board card, run page, transcript bar, activity sidebar, HTTP API and MCP
// tools all drive the same state machine.

// ErrRunControlInvalid reports a control that the job's current state does not
// allow. Callers map it to HTTP 409.
var ErrRunControlInvalid = errors.New("control is not available for this job state")

// Control names.
const (
	ControlRestart = "restart"
	ControlResume  = "resume"
	ControlPoke    = "poke"
)

// DefaultPokeMessage is the nudge text Poke sends when the operator does not
// edit it.
const DefaultPokeMessage = "Continue. Report status."

// maxPokeBytes bounds one steer message so it cannot flood the transcript.
const maxPokeBytes = 4 << 10

// StallReasonLLMUnavailable is the cause class recorded when the model backend
// stops answering. It aligns with the "infra" class in self-healing.
const StallReasonLLMUnavailable = "LLM unavailable"

// Setting keys read by the stall watchdog.
const (
	SettingStallTimeout   = "stall_timeout"
	SettingStallHealthURL = "stall_health_url"
	SettingStallEndpoint  = "model_endpoint"
	SettingStallAutoResume = "stall_auto_resume"
)

// defaultStallTimeout is the idle threshold when the setting is unset: the run
// has produced no output and reported no tokens for this long.
const defaultStallTimeout = 5 * time.Minute

// RunControlOptions carries the operator input for one control action.
type RunControlOptions struct {
	// Message is the Poke text; empty falls back to DefaultPokeMessage.
	Message string
	// Actor names the principal that made the change for the activity sidebar.
	Actor string
}

// RunControlResult names the action taken and the run it created or nudged.
type RunControlResult struct {
	Action string `json:"action"`
	RunID  string `json:"run_id"`
}

// RunControlAvailability reports which controls the given job state allows.
// sessionLive is true while a worker still holds the job's latest run, which is
// what makes a stalled job's agent session poked-able.
func RunControlAvailability(state string, sessionLive bool) map[string]bool {
	switch state {
	case "running":
		return map[string]bool{ControlRestart: true, ControlPoke: true}
	case "stalled":
		return map[string]bool{ControlRestart: true, ControlResume: true, ControlPoke: sessionLive}
	case "failed", "timed_out", "cancelled", "interrupted", "blocked":
		return map[string]bool{ControlRestart: true, ControlResume: true}
	case "succeeded", "resolved", "done":
		return map[string]bool{ControlRestart: true}
	default:
		// queued (cancel only) and any unknown state offer nothing.
		return map[string]bool{}
	}
}

// latestRunRow is the run Restart, Resume and Poke act on.
type latestRunRow struct {
	ID             string
	Command        string
	CommandHash    string
	Executor       string
	Model          string
	Repository     string
	RenderedPrompt string
	State          string
	SessionID      string
	TimeoutMillis  int64
}

func latestRun(ctx context.Context, tx *sql.Tx, jobID string) (latestRunRow, error) {
	var run latestRunRow
	err := tx.QueryRowContext(ctx, `SELECT id,command,command_hash,executor,model,repository,rendered_prompt,state,COALESCE(session_id,''),timeout_ms FROM runs WHERE job_id=? ORDER BY rowid DESC LIMIT 1`, jobID).
		Scan(&run.ID, &run.Command, &run.CommandHash, &run.Executor, &run.Model, &run.Repository, &run.RenderedPrompt, &run.State, &run.SessionID, &run.TimeoutMillis)
	return run, err
}

// RunControl performs one Restart, Resume or Poke against a job's latest run.
// All three keep the job and its card; attempts are linked run to run through
// previous_run_id so the run page follows the chain.
func (s *Store) RunControl(ctx context.Context, jobID, action string, options RunControlOptions) (RunControlResult, error) {
	action = strings.TrimSpace(action)
	if action != ControlRestart && action != ControlResume && action != ControlPoke {
		return RunControlResult{}, fmt.Errorf("unknown run control %q", action)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RunControlResult{}, err
	}
	defer tx.Rollback()

	var jobState, repository string
	if err := tx.QueryRowContext(ctx, `SELECT state,repository FROM jobs WHERE id=?`, jobID).Scan(&jobState, &repository); err != nil {
		return RunControlResult{}, err
	}
	run, err := latestRun(ctx, tx, jobID)
	if err != nil {
		return RunControlResult{}, err
	}
	sessionLive := run.State == "running"
	if !RunControlAvailability(jobState, sessionLive)[action] {
		return RunControlResult{}, fmt.Errorf("%w: %s on a %s job", ErrRunControlInvalid, action, jobState)
	}
	now := s.now().UTC()
	nowText := now.Format(time.RFC3339Nano)
	result := RunControlResult{Action: action}

	switch action {
	case ControlPoke:
		message := strings.TrimSpace(options.Message)
		if message == "" {
			message = DefaultPokeMessage
		}
		if len(message) > maxPokeBytes {
			return RunControlResult{}, fmt.Errorf("poke message is too long (limit %d bytes)", maxPokeBytes)
		}
		line, err := json.Marshal(struct {
			Type    string    `json:"type"`
			Message string    `json:"message"`
			Actor   string    `json:"actor,omitempty"`
			Time    time.Time `json:"time"`
		}{Type: "run.poke", Message: message, Actor: options.Actor, Time: now})
		if err != nil {
			return RunControlResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET events=COALESCE(events,'')||? WHERE id=?`, string(line)+"\n", run.ID); err != nil {
			return RunControlResult{}, fmt.Errorf("append poke to transcript: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO run_pokes(run_id,message,actor,created_at) VALUES(?,?,?,?)`, run.ID, message, options.Actor, nowText); err != nil {
			return RunControlResult{}, fmt.Errorf("store poke: %w", err)
		}
		if err := recordActivity(ctx, tx, jobID, run.ID, ControlPoke, options.Actor, message, now); err != nil {
			return RunControlResult{}, err
		}
		result.RunID = run.ID

	default: // restart, resume
		// Stop the current attempt. A queued run is dropped outright; a running
		// one is marked cancelled so its eventual completion is ignored, and the
		// stalled record is cleared while the new attempt is prepared.
		if run.State == "queued" || run.State == "running" {
			reason := "Restarted by operator"
			if action == ControlResume {
				reason = "Resumed by operator"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE runs SET state='cancelled',exit_code=130,error=?,lease_expires_at=NULL,completed_at=? WHERE id=? AND state IN ('queued','running')`, reason, nowText, run.ID); err != nil {
				return RunControlResult{}, fmt.Errorf("stop attempt: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state='queued',stall_reason='',stall_endpoint='',stalled_at=NULL,updated_at=? WHERE id=?`, nowText, jobID); err != nil {
			return RunControlResult{}, fmt.Errorf("requeue job: %w", err)
		}
		session := ""
		prompt := run.RenderedPrompt
		detail := "restarted the current attempt"
		if action == ControlResume {
			session = run.SessionID
			detail = "resumed the current attempt"
			if session == "" {
				detail = "resumed as a fresh attempt (agent reported no session)"
			}
		}
		block, err := priorRunContext(ctx, tx, run.ID)
		if err != nil {
			return RunControlResult{}, err
		}
		prompt = appendPriorRunContext(prompt, block)
		newRunID, err := s.insertAttemptRun(ctx, tx, jobID, repository, run, prompt, session, nowText)
		if err != nil {
			return RunControlResult{}, err
		}
		if err := recordActivity(ctx, tx, jobID, newRunID, action, options.Actor, detail, now); err != nil {
			return RunControlResult{}, err
		}
		result.RunID = newRunID
	}
	if err := tx.Commit(); err != nil {
		return RunControlResult{}, err
	}
	return result, nil
}

// insertAttemptRun queues a new attempt for a job. Workflow jobs re-enqueue the
// current step through the existing step machinery so task inputs, gates and
// shared outputs stay correct; plain jobs insert one run directly.
func (s *Store) insertAttemptRun(ctx context.Context, tx *sql.Tx, jobID, repository string, prior latestRunRow, prompt, session, now string) (string, error) {
	var plan string
	var index int
	err := tx.QueryRowContext(ctx, `SELECT plan,current_step FROM workflow_jobs WHERE job_id=?`, jobID).Scan(&plan, &index)
	if errors.Is(err, sql.ErrNoRows) {
		runID, err := randomID("run", 12)
		if err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id,job_id,command,command_hash,executor,model,repository,rendered_prompt,timeout_ms,state,previous_run_id,session_id) VALUES(?,?,?,?,?,?,?,?,?,'queued',?,?)`,
			runID, jobID, prior.Command, prior.CommandHash, prior.Executor, prior.Model, prior.Repository, prompt, prior.TimeoutMillis, prior.ID, session); err != nil {
			return "", fmt.Errorf("queue attempt run: %w", err)
		}
		return runID, nil
	}
	if err != nil {
		return "", err
	}
	var steps []config.WorkflowStep
	if err := json.Unmarshal([]byte(plan), &steps); err != nil {
		return "", err
	}
	if index < 0 || index >= len(steps) {
		return "", errors.New("invalid workflow step")
	}
	runID, err := enqueueStep(ctx, tx, jobID, repository, index, steps[index], false, now)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET rendered_prompt=?,previous_run_id=?,session_id=? WHERE id=?`, prompt, prior.ID, session, runID); err != nil {
		return "", fmt.Errorf("attach attempt context: %w", err)
	}
	return runID, nil
}

// priorRunContext describes one run's outcome and transcript for the next
// attempt's prompt, bounded like the requeue context (MACH-16).
func priorRunContext(ctx context.Context, tx *sql.Tx, runID string) (string, error) {
	var command, state, runError, result, events string
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(command,''),state,COALESCE(error,''),COALESCE(result,''),COALESCE(events,'') FROM runs WHERE id=?`, runID).
		Scan(&command, &state, &runError, &result, &events)
	if err != nil {
		return "", fmt.Errorf("read prior run: %w", err)
	}
	var summary strings.Builder
	summary.WriteString("### Prior run")
	if command != "" {
		summary.WriteString(" `" + command + "`")
	}
	summary.WriteString(" — " + state)
	if state == "timed_out" {
		summary.WriteString(" (hit its timeout)")
	}
	if runError != "" {
		summary.WriteString("\nError: " + bound(runError, maxPriorErrorBytes))
	}
	if trimmed := strings.TrimSpace(result); trimmed != "" {
		summary.WriteString("\nResult: " + bound(trimmed, maxPriorResultBytes))
	}
	if transcript := strings.Join(decodeEventTranscript(events), "\n"); transcript != "" {
		transcript = bound(transcript, maxPriorRunTranscript)
		summary.WriteString("\n\nTranscript:\n```\n" + transcript + "\n```")
	}
	return priorAttemptHeader + "\n\n" + summary.String(), nil
}

// appendPriorRunContext appends the prior-run block to a run prompt. Stream
// attempts hold their own prompt, so there is no task spec to mirror.
func appendPriorRunContext(prompt, block string) string {
	if block == "" {
		return prompt
	}
	return prompt + "\n\n" + block
}

// pokeEventLines renders a run's recorded pokes as transcript JSONL, oldest
// first, so completion keeps them after the live log is replaced.
func pokeEventLines(ctx context.Context, tx *sql.Tx, runID string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT actor,message,created_at FROM run_pokes WHERE run_id=? ORDER BY id`, runID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var actor, message, createdAt string
		if err := rows.Scan(&actor, &message, &createdAt); err != nil {
			return "", err
		}
		line, err := json.Marshal(struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Actor   string `json:"actor,omitempty"`
			Time    string `json:"time"`
		}{Type: "run.poke", Message: message, Actor: actor, Time: createdAt})
		if err != nil {
			return "", err
		}
		lines = append(lines, string(line))
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}

// DeliverPokes returns the steer messages queued for a run the worker holds,
// marking them delivered. It enforces the same lease check as heartbeats.
func (s *Store) DeliverPokes(ctx context.Context, runID string, deliveries protocol.PokeDeliveries) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var state, instanceID, leaseToken string
	if err := tx.QueryRowContext(ctx, `SELECT state,COALESCE(worker_instance,''),COALESCE(lease_token,'') FROM runs WHERE id=?`, runID).Scan(&state, &instanceID, &leaseToken); err != nil {
		return nil, err
	}
	if state != "running" {
		return nil, ErrRunState
	}
	if !constantTimeEquals(instanceID, deliveries.InstanceID) || !constantTimeEquals(leaseToken, deliveries.LeaseToken) {
		return nil, ErrLeaseConflict
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,message FROM run_pokes WHERE run_id=? AND delivered_at='' ORDER BY id`, runID)
	if err != nil {
		return nil, err
	}
	var ids []int64
	var messages []string
	for rows.Next() {
		var id int64
		var message string
		if err := rows.Scan(&id, &message); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		messages = append(messages, message)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, tx.Commit()
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE run_pokes SET delivered_at=? WHERE id=?`, now, id); err != nil {
			return nil, err
		}
	}
	return messages, tx.Commit()
}

// StallOptions configures the stall watchdog.
type StallOptions struct {
	Now      time.Time
	// Idle marks only runs whose last progress is older than this. Zero marks
	// every running job (used when the endpoint health check fails).
	Idle     time.Duration
	Endpoint string
	Reason   string
	Actor    string
}

// StallRunningJobs marks running jobs stalled: their LLM backend stopped
// answering, so the job is infra-blocked rather than failing. It records the
// reason, endpoint and time, and logs the stall in the activity sidebar.
func (s *Store) StallRunningJobs(ctx context.Context, options StallOptions) (int64, error) {
	now := options.Now
	if now.IsZero() {
		now = s.now().UTC()
	}
	reason := strings.TrimSpace(options.Reason)
	if reason == "" {
		reason = StallReasonLLMUnavailable
	}
	actor := strings.TrimSpace(options.Actor)
	if actor == "" {
		actor = "scheduler"
	}
	nowText := now.Format(time.RFC3339Nano)
	query := `SELECT j.id,COALESCE(r.id,'') FROM jobs j JOIN runs r ON r.job_id=j.id
WHERE j.state='running' AND r.state='running' AND r.rowid=(SELECT MAX(rowid) FROM runs WHERE job_id=j.id)`
	args := []any{}
	if options.Idle > 0 {
		query += ` AND COALESCE(r.last_progress_at,r.started_at) <= ?`
		args = append(args, now.Add(-options.Idle).Format(time.RFC3339Nano))
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	type stalledJob struct{ jobID, runID string }
	var stalled []stalledJob
	for rows.Next() {
		var entry stalledJob
		if err := rows.Scan(&entry.jobID, &entry.runID); err != nil {
			rows.Close()
			return 0, err
		}
		stalled = append(stalled, entry)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, entry := range stalled {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state='stalled',stall_reason=?,stall_endpoint=?,stalled_at=?,updated_at=? WHERE id=?`, reason, options.Endpoint, nowText, nowText, entry.jobID); err != nil {
			return 0, err
		}
		if err := recordActivity(ctx, tx, entry.jobID, entry.runID, "stall", actor, reason, now); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(stalled)), nil
}

// StalledJobIDs lists jobs waiting on a stalled LLM backend, so the watchdog
// can resume them once the endpoint recovers.
func (s *Store) StalledJobIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM jobs WHERE state='stalled' ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("list stalled jobs: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}