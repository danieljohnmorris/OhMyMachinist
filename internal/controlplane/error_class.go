package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/runner"
)

// Error classification (MACH-69): a run that failed because the model
// endpoint was unreachable is llm_unavailable, not a code failure. It must
// not consume the self-healing retry budget, add the Plane failed label, or
// trigger review or fix-up flows (MACH-62 reuses the same error_class
// plumbing; MACH-45 shares the class name).

// ErrorClass names why a run failed.
type ErrorClass string

// ErrorClassLLMUnavailable marks a failure caused by the model endpoint being
// unreachable: omp auto_retry_end with a connection-type finalError, a shim
// or proxy connection refusal in the transcript, or a zero-output-token
// agent_end next to a connection error.
const ErrorClassLLMUnavailable ErrorClass = "llm_unavailable"

// errorClassMetadataKey is the job metadata key the classification owns.
const errorClassMetadataKey = "error_class"

// ErrorClassRecord is stored on the job so the board card, run page, failure
// alert and Plane comment show the class, the reason and the provider/model/
// endpoint/time of the outage, plus the automatic requeue state.
type ErrorClassRecord struct {
	Class    ErrorClass `json:"class"`
	Reason   string     `json:"reason,omitempty"`
	Provider string     `json:"provider,omitempty"`
	Model    string     `json:"model,omitempty"`
	Endpoint string     `json:"endpoint,omitempty"`
	At       string     `json:"at,omitempty"`
	// Requeues counts automatic requeues already done for this failure.
	Requeues int `json:"requeues,omitempty"`
	// Resolved ends automatic action: the requeue happened, the wait expired,
	// the requeue allowance is used up, or another attempt of the ticket is
	// already active. Stopped carries the human-readable why.
	Resolved bool   `json:"resolved,omitempty"`
	Stopped  string `json:"stopped,omitempty"`
}

// llmConnectionPattern matches connection-type errors from the model endpoint
// path: omp's "Unable to connect", refusals, resets, DNS, timeouts and
// gateway statuses from the model endpoint.
var llmConnectionPattern = regexp.MustCompile(`(?i)(unable to connect|connection refused|econnrefused|connection reset|no such host|network is unreachable|i/o timeout|dial tcp|connection timed out|502 bad gateway|503 service unavailable|504 gateway timeout)`)

// classifyErrorClass inspects a failed run's error text and event transcript
// for the llm_unavailable signals. The empty class means no classification.
//
// A failed auto_retry_end with a connection-type finalError classifies on its
// own. A connection error anywhere in the transcript needs corroboration — a
// zero-output-token agent_end, the agent having produced nothing — so a test
// that merely logged a refused connection (or a git push to a dead remote)
// does not classify as llm_unavailable. A failed auto_retry_end whose
// finalError is not connection-type is not corroboration either.
func classifyErrorClass(errorText, events string) (ErrorClass, string) {
	var agentEndZeroTokens bool
	for _, raw := range strings.Split(events, "\n") {
		if !strings.Contains(raw, `"agent.`) {
			continue
		}
		var event runner.Event
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			continue
		}
		switch event.Type {
		case "agent.auto_retry_end":
			if event.Failed && llmConnectionPattern.MatchString(event.Message) {
				return ErrorClassLLMUnavailable, "omp auto_retry_end failed: " + event.Message
			}
		case "agent.agent_end":
			agentEndZeroTokens = agentEndZeroTokens || strings.Contains(event.Message, "output_tokens=0")
		}
	}
	transcript := strings.Join(decodeEventTranscript(events), "\n")
	combined := errorText + "\n" + transcript
	if agentEndZeroTokens && llmConnectionPattern.MatchString(combined) {
		return ErrorClassLLMUnavailable, "connection error in the run transcript with a zero-output-token agent_end"
	}
	return "", ""
}

// SaveErrorClassMetadata merges an error class record into the job metadata
// shown on the board card, run page, failure alert and Plane comment.
func (s *Store) SaveErrorClassMetadata(ctx context.Context, jobID string, record ErrorClassRecord) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT metadata FROM job_metadata WHERE job_id=?`, jobID).Scan(&raw)
	existing := map[string]any{}
	if err == nil && strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &existing)
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	existing[errorClassMetadataKey] = json.RawMessage(encoded)
	body, err := json.Marshal(existing)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO job_metadata(job_id,metadata) VALUES(?,?)
ON CONFLICT(job_id) DO UPDATE SET metadata=excluded.metadata`, jobID, string(body)); err != nil {
		return err
	}
	return tx.Commit()
}

// jobHasSucceededRun reports whether the job has ever had a successful run —
// a success separates one outage from the next.
func (s *Store) jobHasSucceededRun(ctx context.Context, jobID string) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE job_id=? AND state='succeeded'`, jobID).Scan(&count)
	return count > 0, err
}

// jobErrorClass reads a job's stored error class; empty when unclassified.
func (s *Store) jobErrorClass(ctx context.Context, jobID string) ErrorClass {
	job, err := s.GetJob(ctx, jobID)
	if err != nil || job.Metadata == nil || job.Metadata.ErrorClass == nil {
		return ""
	}
	return job.Metadata.ErrorClass.Class
}

// ticketActiveAttemptExists reports whether another job of the same ticket is
// queued or running: the usual dedupe — no requeue when an attempt is already
// in flight. Jobs are grouped per repository, matching the attempt chain.
func (s *Store) ticketActiveAttemptExists(ctx context.Context, job Job) (bool, error) {
	key := job.IssueKey
	if key == "" {
		key = ticketKey(job)
	}
	if key == "" {
		return false, nil
	}
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs WHERE id<>? AND repository=? AND (issue_key=? OR (?='' AND issue_key='')) AND state IN ('queued','running')`,
		job.ID, job.Repository, key, key).Scan(&count)
	return count > 0, err
}

// parseRecordTime parses a record timestamp, returning the zero time on
// absence or garbage so callers treat the record as immediately due.
func parseRecordTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// llmUnavailableAutoRequeue resolves the requeue allowance: a negative config
// disables the automatic requeue entirely.
func (s *Server) llmUnavailableAutoRequeue() int {
	return s.llmUnavailable.AutoRequeueCount()
}
