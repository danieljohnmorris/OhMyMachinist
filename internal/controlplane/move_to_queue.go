package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/owainlewis/machinist/internal/protocol"
)

// ErrMoveNotAvailable reports a move-to-queue request the store refuses: the
// job is not running, or it is a workflow job whose steps have their own
// retry/cancel actions.
var ErrMoveNotAvailable = errors.New("move to queue is only available for a running plain job")

// moveReason is the cancellation reason recorded on the stopped attempt.
const moveReason = "moved to queue"

// MoveToQueue cancels a running or stalled job's current attempt and requeues
// the same work as the next attempt on the same card (MACH-65). The current
// run is marked cancelled with the reason "moved to queue"; its lease stays
// recorded so the worker that holds it receives a lease-revoked answer on its
// next heartbeat, kills its agent process group, saves the lane's uncommitted
// work (tracked and untracked) and reports where it was saved. The lease is
// already released here, so a silent or hung agent never blocks the action:
// the new attempt is queued immediately, at the back of the queue, as attempt
// N+1 with the prior transcript attached (MACH-16).
//
// The operation is idempotent: a second call returns the same new attempt id
// instead of creating a duplicate job. Workflow jobs are refused; they carry
// their own retry and cancel actions. Unknown jobs return sql.ErrNoRows.
func (s *Store) MoveToQueue(ctx context.Context, jobID string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var state, supersededBy, title, triggerSubject, issueKey string
	var attempt int
	err = tx.QueryRowContext(ctx, `SELECT state,COALESCE(superseded_by,''),attempt,title,COALESCE(trigger_subject,''),COALESCE(issue_key,'') FROM jobs WHERE id=?`, jobID).Scan(&state, &supersededBy, &attempt, &title, &triggerSubject, &issueKey)
	if err != nil {
		return "", err
	}
	// Idempotency: a move already happened for this job, whether through this
	// action or a later submission for the same card. Return that attempt.
	// A running job's superseded_by is only the next job in its ticket chain
	// (attempt_links), not a completed move: the run still has to be
	// cancelled and requeued below, and the workflow refusal must apply.
	if state != "running" && supersededBy != "" {
		return supersededBy, nil
	}
	var workflow bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_jobs WHERE job_id=?)`, jobID).Scan(&workflow); err != nil {
		return "", err
	}
	if workflow {
		return "", ErrMoveNotAvailable
	}
	if state != "running" {
		return "", ErrMoveNotAvailable
	}
	var runID, command, commandHash, executor, model, repository, renderedPrompt string
	var timeoutMillis int64
	err = tx.QueryRowContext(ctx, `SELECT id,command,command_hash,executor,model,repository,rendered_prompt,timeout_ms FROM runs WHERE job_id=? ORDER BY rowid DESC LIMIT 1`, jobID).
		Scan(&runID, &command, &commandHash, &executor, &model, &repository, &renderedPrompt, &timeoutMillis)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrMoveNotAvailable
	}
	if err != nil {
		return "", err
	}

	newJobID, err := randomID("job", 12)
	if err != nil {
		return "", err
	}
	newRunID, err := randomID("run", 12)
	if err != nil {
		return "", err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)

	// Cancel the attempt. worker_instance and lease_token stay so the
	// revocation acknowledgment can be verified; lease_expires_at is
	// cleared so the reclaim never touches the run again. The old job
	// leaves the active states before the requeued job is inserted, so
	// the per-subject active-job index never sees both.
	result, err := tx.ExecContext(ctx, `UPDATE runs SET state='cancelled',exit_code=130,error=?,lease_expires_at=NULL,completed_at=? WHERE id=? AND state='running'`, moveReason, now, runID)
	if err != nil {
		return "", fmt.Errorf("cancel run: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return "", ErrMoveNotAvailable
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state='cancelled',superseded_by=?,updated_at=? WHERE id=?`, newJobID, now, jobID); err != nil {
		return "", fmt.Errorf("supersede job: %w", err)
	}

	// The prior transcript becomes the new prompt's context (MACH-16); the
	// cancelled attempt is already terminal, so its outcome, the move
	// reason and its transcript are part of the block.
	block, err := priorAttemptContext(ctx, tx, jobID)
	if err != nil {
		return "", err
	}
	prompt, _ := appendPriorContext(renderedPrompt, "", block)

	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,title,prompt,repository,command,trigger_subject,issue_key,state,created_at,updated_at,previous_job_id,attempt) VALUES(?,?,?,?,?,?,?,'queued',?,?,?,?)`,
		newJobID, title, prompt, repository, command, triggerSubject, issueKey, now, now, jobID, attempt+1); err != nil {
		return "", fmt.Errorf("insert requeued job: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id,job_id,command,command_hash,executor,model,repository,rendered_prompt,timeout_ms,state) VALUES(?,?,?,?,?,?,?,?,?,'queued')`,
		newRunID, newJobID, command, commandHash, executor, model, repository, prompt, timeoutMillis); err != nil {
		return "", fmt.Errorf("insert requeued run: %w", err)
	}
	// Carry the task spec and metadata (project, intake source, issue URL)
	// so the requeued attempt is the same job on the same card.
	if _, err := tx.ExecContext(ctx, `INSERT INTO task_inputs(job_id,task) SELECT ?,task FROM task_inputs WHERE job_id=?`, newJobID, jobID); err != nil {
		return "", fmt.Errorf("carry task inputs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_metadata(job_id,metadata) SELECT ?,metadata FROM job_metadata WHERE job_id=?`, newJobID, jobID); err != nil {
		return "", fmt.Errorf("carry job metadata: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit move to queue: %w", err)
	}
	return newJobID, nil
}

// RecordRevokedWIP stores where the worker saved the lane's uncommitted work
// after a move to queue. Only the worker that held the revoked lease may
// report; anything else is a lease conflict.
func (s *Store) RecordRevokedWIP(ctx context.Context, runID string, revoked protocol.LeaseRevoked) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, instanceID, leaseToken string
	if err := tx.QueryRowContext(ctx, `SELECT state,COALESCE(worker_instance,''),COALESCE(lease_token,'') FROM runs WHERE id=?`, runID).Scan(&state, &instanceID, &leaseToken); err != nil {
		return err
	}
	if state != "cancelled" {
		return ErrRunState
	}
	if instanceID != revoked.InstanceID || leaseToken != revoked.LeaseToken {
		return ErrLeaseConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET wip=? WHERE id=?`, revoked.WIP, runID); err != nil {
		return err
	}
	return tx.Commit()
}
