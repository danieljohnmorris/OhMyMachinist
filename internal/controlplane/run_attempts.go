package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/config"
)

// Run attempts are the same model as the job-level attempts used for requeues
// (attempt number, status, timings, transcript): a run relaunched after a lease
// expiry keeps its ID and gains another attempt rather than a new job, so the
// attempts must be recorded per run. Every process launch writes one row here.
//
// Attempt end reasons describe why one process launch stopped and are shown on
// the transcript header, the card and the activity sidebar.
const (
	endReasonLeaseExpired  = "lease expired"
	endReasonWorkerRestart = "worker restart"
	endReasonTimeout       = "timeout"
	endReasonCancelled     = "cancelled"
	endReasonRelaunchLimit = "relaunch limit"
)

// laneGrace is how long a lane may be silent before a pinned run is allowed to
// move to another worker. A lane that restarts re-registers under the same name
// within this window, so the relaunch stays on the same checkout.
const laneGrace = 5 * time.Minute

// maxRelaunches returns the configured relaunch cap, falling back to the
// built-in default when the store was not given one.
func (s *Store) maxRelaunches() int {
	if s.relaunchLimit > 0 {
		return s.relaunchLimit
	}
	return config.DefaultMaxRelaunches
}

// SetRelaunchLimit overrides how many in-place relaunches a run may have before
// it fails with reason "relaunch limit". Non-positive values keep the default.
func (s *Store) SetRelaunchLimit(limit int) {
	if limit > 0 {
		s.relaunchLimit = limit
	}
}

// startRunAttempt records a new process launch for a run and returns its
// 1-based number. It also pins the run to the worker's lane the first time so a
// relaunch cannot silently move to another checkout.
func startRunAttempt(ctx context.Context, tx *sql.Tx, runID, workerName, model, now string) (int, error) {
	var attempt int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(attempt),0)+1 FROM run_attempts WHERE run_id=?`, runID).Scan(&attempt); err != nil {
		return 0, fmt.Errorf("next run attempt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_attempts(run_id,attempt,worker_name,model,started_at) VALUES(?,?,?,?,?)`, runID, attempt, workerName, model, now); err != nil {
		return 0, fmt.Errorf("record run attempt: %w", err)
	}
	if workerName != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE runs SET lane=? WHERE id=? AND lane=''`, workerName, runID); err != nil {
			return 0, fmt.Errorf("pin run lane: %w", err)
		}
	}
	return attempt, nil
}

// currentAttempt returns the latest attempt number for a run and whether it is
// still open (has no end time). A run with no recorded attempts returns 0.
func currentAttempt(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, runID string) (int, bool, error) {
	var attempt int
	var endedAt sql.NullString
	err := q.QueryRowContext(ctx, `SELECT attempt,ended_at FROM run_attempts WHERE run_id=? ORDER BY attempt DESC LIMIT 1`, runID).Scan(&attempt, &endedAt)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read current run attempt: %w", err)
	}
	return attempt, !endedAt.Valid, nil
}

// endRunAttempt closes an attempt with its end reason and, when provided, the
// transcript captured for that attempt. A missing attempt is a no-op so callers
// can end attempts on paths that may predate the attempt model.
func endRunAttempt(ctx context.Context, tx *sql.Tx, runID string, attempt int, reason, transcript, endedAt string) error {
	if attempt == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE run_attempts SET ended_at=?,end_reason=?,transcript=? WHERE run_id=? AND attempt=?`, endedAt, reason, transcript, runID, attempt); err != nil {
		return fmt.Errorf("end run attempt: %w", err)
	}
	return nil
}

// endLatestAttempt closes the newest open attempt of a run, if any, using the
// given reason. It returns the attempt number that was closed.
func endLatestAttempt(ctx context.Context, tx *sql.Tx, runID, reason, transcript, endedAt string) (int, error) {
	attempt, open, err := currentAttempt(ctx, tx, runID)
	if err != nil || !open {
		return attempt, err
	}
	if err := endRunAttempt(ctx, tx, runID, attempt, reason, transcript, endedAt); err != nil {
		return attempt, err
	}
	return attempt, nil
}

// endReasonForCompletion maps a terminal completion to the attempt's end reason.
func endReasonForCompletion(completionState string, exitCode int) string {
	switch completionState {
	case "timed_out":
		return endReasonTimeout
	case "cancelled":
		return endReasonCancelled
	default:
		return fmt.Sprintf("exit code %d", exitCode)
	}
}

// leaseEndReason decides why an expired lease ended an attempt: a worker that
// registered under a newer instance (a restart) versus a lease that simply
// lapsed while the worker went silent.
func leaseEndReason(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, runID, workerInstance string) string {
	if workerInstance == "" {
		return endReasonLeaseExpired
	}
	var name string
	if err := q.QueryRowContext(ctx, `SELECT name FROM workers WHERE instance_id=?`, workerInstance).Scan(&name); err != nil {
		// The worker row is gone; a restarted control plane prunes old instances,
		// so treat the missing row as a restart.
		return endReasonWorkerRestart
	}
	var restarted int
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workers WHERE name=? AND instance_id<>? AND julianday(last_seen_at)>=julianday((SELECT last_seen_at FROM workers WHERE instance_id=?)))`, name, workerInstance, workerInstance).Scan(&restarted); err != nil {
		return endReasonLeaseExpired
	}
	if restarted == 1 {
		return endReasonWorkerRestart
	}
	return endReasonLeaseExpired
}

// activeLanes returns the lane (worker) names that polled within the lane
// grace window. A pinned run only leaves its lane when that lane is absent, so
// one snapshot read is enough for a whole poll.
func activeLanes(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, now time.Time) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT DISTINCT name FROM workers WHERE julianday(last_seen_at)>=julianday(?)`, now.Add(-laneGrace).Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lanes := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		lanes[name] = true
	}
	return lanes, rows.Err()
}

// relaunchPriorContext builds the prompt block injected into a relaunched
// attempt: one section per earlier attempt with its end reason and transcript.
// It is bounded so a noisy prior attempt cannot dominate the new prompt.
func relaunchPriorContext(ctx context.Context, tx *sql.Tx, runID string, attempt int) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT attempt,COALESCE(worker_name,''),COALESCE(model,''),COALESCE(ended_at,''),COALESCE(end_reason,''),COALESCE(transcript,'') FROM run_attempts WHERE run_id=? AND attempt<? ORDER BY attempt`, runID, attempt)
	if err != nil {
		return "", fmt.Errorf("read prior run attempts: %w", err)
	}
	defer rows.Close()
	var sections []string
	remaining := maxPriorTranscriptBytes
	for rows.Next() {
		var number int
		var worker, model, endedAt, reason, transcript string
		if err := rows.Scan(&number, &worker, &model, &endedAt, &reason, &transcript); err != nil {
			return "", fmt.Errorf("scan prior run attempt: %w", err)
		}
		var summary strings.Builder
		fmt.Fprintf(&summary, "### Attempt %d", number)
		if worker != "" {
			summary.WriteString(" — worker " + worker)
		}
		if model != "" {
			summary.WriteString(", model " + model)
		}
		if reason != "" {
			summary.WriteString(" — ended: " + reason)
		}
		if trimmed := strings.TrimSpace(strings.Join(decodeEventTranscript(transcript), "\n")); trimmed != "" {
			if len(trimmed) > remaining {
				trimmed = bound(trimmed, remaining)
			} else {
				remaining -= len(trimmed)
			}
			summary.WriteString("\n\nTranscript:\n```\n" + trimmed + "\n```")
		}
		sections = append(sections, summary.String())
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read prior run attempts: %w", err)
	}
	if len(sections) == 0 {
		return "", nil
	}
	return relaunchPriorHeader + "\n\n" + strings.Join(sections, "\n\n"), nil
}

const relaunchPriorHeader = `## Prior attempt

This run was relaunched after its previous process stopped. Diagnose the prior
attempt from its end reason and transcript below, then continue the task from
where it stopped instead of starting over.`

// loadRunAttempts attaches every run's attempt history, oldest first. It reads
// all attempts at once; runs without attempts keep a nil slice.
func loadRunAttempts(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, runs []*Run) error {
	if len(runs) == 0 {
		return nil
	}
	index := map[string]*Run{}
	placeholders := make([]string, 0, len(runs))
	args := make([]any, 0, len(runs))
	for _, run := range runs {
		if run.ID == "" {
			continue
		}
		index[run.ID] = run
		placeholders = append(placeholders, "?")
		args = append(args, run.ID)
	}
	if len(placeholders) == 0 {
		return nil
	}
	rows, err := q.QueryContext(ctx, `SELECT run_id,attempt,COALESCE(worker_name,''),COALESCE(model,''),COALESCE(started_at,''),COALESCE(ended_at,''),COALESCE(end_reason,'') FROM run_attempts WHERE run_id IN (`+strings.Join(placeholders, ",")+`) ORDER BY run_id,attempt`, args...)
	if err != nil {
		return fmt.Errorf("read run attempts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var runID, worker, model, started, ended, reason string
		var attempt int
		if err := rows.Scan(&runID, &attempt, &worker, &model, &started, &ended, &reason); err != nil {
			return fmt.Errorf("scan run attempt: %w", err)
		}
		run := index[runID]
		if run == nil {
			continue
		}
		entry := RunAttempt{Number: attempt, WorkerName: worker, Model: model, EndReason: reason}
		entry.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
		entry.EndedAt, _ = time.Parse(time.RFC3339Nano, ended)
		run.Attempts = append(run.Attempts, entry)
	}
	return rows.Err()
}

// runEventsFromAttempts returns a run's stored transcript as the concatenation
// of its attempts' transcripts, oldest first, so the full relaunch history
// survives completion instead of only the last attempt's log. A run with no
// attempt rows (created before the attempt model) keeps the fallback log.
func runEventsFromAttempts(ctx context.Context, tx *sql.Tx, runID, fallback string) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT transcript FROM run_attempts WHERE run_id=? ORDER BY attempt`, runID)
	if err != nil {
		return "", fmt.Errorf("read attempt transcripts: %w", err)
	}
	var transcripts []string
	for rows.Next() {
		var transcript string
		if err := rows.Scan(&transcript); err != nil {
			rows.Close()
			return "", fmt.Errorf("scan attempt transcript: %w", err)
		}
		transcripts = append(transcripts, transcript)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	rows.Close()
	if len(transcripts) == 0 {
		return fallback, nil
	}
	var builder strings.Builder
	for _, transcript := range transcripts {
		builder.WriteString(transcript)
		if !strings.HasSuffix(transcript, "\n") {
			builder.WriteString("\n")
		}
	}
	return builder.String(), nil
}
