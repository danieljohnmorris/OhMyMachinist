package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/protocol"
)

// PlaneAdmission is one ticket's work, already resolved into a Machinist job.
// It is admitted through the same dedupe as issue triggers: the caller checks
// ActiveTriggerJob first, updates a queued job's spec, and creates a job only
// when no active job exists.
type PlaneAdmission struct {
	Prompt     string
	Title      string
	Repository string
	Command    config.ResolvedCommand
	Task       *protocol.Task
	Project    string
	IssueKey   string
	// Subject is the ticket browse URL, used to dedupe active jobs and to
	// chain attempts, matching issue-trigger subjects.
	Subject string
	// Source records where the event came from: webhook, poll or approve-bridge.
	Source string
	// DeliveryID is the Plane webhook delivery id, empty for polled events.
	DeliveryID string
	// SpecHash is the ticket spec hash shown on the run page.
	SpecHash string
}

// CreatePlaneJob inserts a queued Plane-intake job with its first run, intake
// metadata and prior-attempt links.
func (s *Store) CreatePlaneJob(ctx context.Context, admission PlaneAdmission) (string, error) {
	if admission.Command.Name == "" {
		return "", errors.New("plane job must contain one command")
	}
	if admission.Repository == "" {
		return "", errors.New("plane job repository is required")
	}
	jobID, err := randomID("job", 12)
	if err != nil {
		return "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,title,prompt,repository,command,trigger_subject,issue_key,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,'queued',?,?)`,
		jobID, admission.Title, admission.Prompt, admission.Repository, admission.Command.Name, admission.Subject, admission.IssueKey, now, now); err != nil {
		return "", fmt.Errorf("insert plane job: %w", err)
	}
	command := admission.Command
	previous, err := s.linkPriorAttempt(ctx, tx, jobID, admission.Subject, admission.Repository)
	if err != nil {
		return "", err
	}
	if previous != "" {
		block, blockErr := priorAttemptContext(ctx, tx, previous)
		if blockErr != nil {
			return "", blockErr
		}
		admission.Prompt, _ = appendPriorContext(admission.Prompt, "", block)
		command.Prompt += "\n\n" + block
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET prompt=? WHERE id=?`, admission.Prompt, jobID); err != nil {
			return "", fmt.Errorf("add prior attempt context: %w", err)
		}
	}
	if admission.Task != nil {
		raw, err := json.Marshal(*admission.Task)
		if err != nil {
			return "", fmt.Errorf("encode plane task: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_inputs(job_id,task) VALUES(?,?)`, jobID, string(raw)); err != nil {
			return "", fmt.Errorf("insert plane task: %w", err)
		}
	}
	metadata := map[string]any{
		"intake_source":      admission.Source,
		"intake_delivery_id": admission.DeliveryID,
		"intake_spec_hash":   admission.SpecHash,
	}
	if admission.Subject != "" {
		metadata["issue_url"] = admission.Subject
	}
	if admission.Project != "" {
		metadata["project"] = admission.Project
	}
	if err := saveJobMetadata(ctx, tx, jobID, metadata); err != nil {
		return "", fmt.Errorf("store plane job metadata: %w", err)
	}
	runID, err := randomID("run", 12)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id,job_id,command,command_hash,executor,model,repository,rendered_prompt,timeout_ms,state) VALUES(?,?,?,?,?,?,?,?,?,'queued')`,
		runID, jobID, command.Name, command.Hash, command.Executor, command.Model, admission.Repository, command.Prompt, command.Timeout.Milliseconds()); err != nil {
		return "", fmt.Errorf("insert plane run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit plane job: %w", err)
	}
	return jobID, nil
}

// PlaneSyncCandidate is one plane-intake job's run as seen by the state sync.
type PlaneSyncCandidate struct {
	JobID     string
	RunID     string
	RunState  string
	StartedAt string
	Events    string
	IssueKey  string
	IssueID   string
	ProjectID string
	SpecHash  string
	Source    string
	Repo      string
}

// PlaneSyncCandidates lists plane-intake jobs whose run started at or after
// since, with the intake record and repository needed to patch the ticket.
func (s *Store) PlaneSyncCandidates(ctx context.Context, since time.Time) ([]PlaneSyncCandidate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.job_id,r.id,r.state,COALESCE(r.started_at,''),COALESCE(r.events,''),
 j.issue_key,COALESCE(i.issue_id,''),COALESCE(i.project_id,''),COALESCE(i.spec_hash,''),COALESCE(i.source,'')
FROM runs r
JOIN jobs j ON j.id=r.job_id
JOIN plane_intake i ON i.job_id=j.id
WHERE r.started_at IS NOT NULL AND r.started_at<>'' AND r.started_at>=?
ORDER BY r.started_at`, since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("read plane sync candidates: %w", err)
	}
	defer rows.Close()
	candidates := []PlaneSyncCandidate{}
	for rows.Next() {
		var candidate PlaneSyncCandidate
		if err := rows.Scan(&candidate.JobID, &candidate.RunID, &candidate.RunState, &candidate.StartedAt, &candidate.Events,
			&candidate.IssueKey, &candidate.IssueID, &candidate.ProjectID, &candidate.SpecHash, &candidate.Source); err != nil {
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	return candidates, rows.Err()
}

// PlaneIntakeJob returns the intake record for a job, used by the sync loop to
// find the ticket behind a run.
func (s *Store) PlaneIntakeJob(ctx context.Context, jobID string) (string, PlaneIntakeRecord, bool, error) {
	var issueID string
	var record PlaneIntakeRecord
	err := s.db.QueryRowContext(ctx, `SELECT issue_id,spec_hash,key,project_id,job_id,source,delivery_id FROM plane_intake WHERE job_id=? ORDER BY updated_at DESC LIMIT 1`, jobID).
		Scan(&issueID, &record.SpecHash, &record.Key, &record.ProjectID, &record.JobID, &record.Source, &record.DeliveryID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", PlaneIntakeRecord{}, false, nil
	}
	if err != nil {
		return "", PlaneIntakeRecord{}, false, fmt.Errorf("read plane intake for job: %w", err)
	}
	return issueID, record, true, nil
}
