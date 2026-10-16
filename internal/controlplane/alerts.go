package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FailAlert is the pending failure alert JSON spooled for a failed run. An
// external alert poller picks these files up and notifies; reason lets the
// notifier distinguish a timeout ("timed out after 2h") from a plain failure.
type FailAlert struct {
	JobID         string `json:"job_id"`
	RunID         string `json:"run_id"`
	IssueKey      string `json:"issue_key,omitempty"`
	IssueURL      string `json:"issue_url,omitempty"`
	Command       string `json:"command"`
	TimeoutMillis int64  `json:"timeout_millis"`
	ElapsedMillis int64  `json:"elapsed_millis"`
	ExitCode      int    `json:"exit_code"`
	Reason        string `json:"reason"`
	// ErrorClass carries the failure classification (MACH-69): a hook can
	// route an llm_unavailable outage differently from a code failure.
	ErrorClass string `json:"error_class,omitempty"`
}

// alertingState reports whether a terminal process state emits a fail alert.
// Cancelled runs are operator actions, not failures.
func alertingState(state string) bool {
	return state == "failed" || state == "timed_out"
}

// failAlertWriter spools pending fail alert JSON files into the configured
// directory for an external alert poller. Writing is idempotent per run: an
// existing alert file is left untouched.
type failAlertWriter struct {
	directory string
}

func (w *failAlertWriter) write(alert FailAlert) error {
	if w == nil || w.directory == "" {
		return nil
	}
	if err := os.MkdirAll(w.directory, 0o755); err != nil {
		return fmt.Errorf("create alert directory: %w", err)
	}
	body, err := json.MarshalIndent(alert, "", "  ")
	if err != nil {
		return fmt.Errorf("encode fail alert: %w", err)
	}
	path := filepath.Join(w.directory, "fail-"+alert.RunID+".json")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("create fail alert %s: %w", path, err)
	}
	defer file.Close()
	if _, err = file.Write(append(body, '\n')); err != nil {
		return fmt.Errorf("write fail alert %s: %w", path, err)
	}
	return nil
}

// pendingFailAlert collects the failure alert payload for a terminal run. ok
// is false when the run did not end failed or timed out.
func (s *Store) pendingFailAlert(ctx context.Context, runID string) (FailAlert, bool, error) {
	var alert FailAlert
	var state, jobID, title, metadata string
	var exitCode, durationMillis int64
	err := s.db.QueryRowContext(ctx, `SELECT r.state,r.job_id,COALESCE(r.command,''),r.timeout_ms,COALESCE(r.exit_code,0),COALESCE(r.duration_millis,0),COALESCE(j.title,''),COALESCE((SELECT metadata FROM job_metadata WHERE job_id=j.id),'') FROM runs r JOIN jobs j ON j.id=r.job_id WHERE r.id=?`,
		runID).Scan(&state, &jobID, &alert.Command, &alert.TimeoutMillis, &exitCode, &durationMillis, &title, &metadata)
	if errors.Is(err, sql.ErrNoRows) {
		return FailAlert{}, false, nil
	}
	if err != nil {
		return FailAlert{}, false, err
	}
	if !alertingState(state) {
		return FailAlert{}, false, nil
	}
	alert.JobID = jobID
	alert.RunID = runID
	alert.ExitCode = int(exitCode)
	alert.ElapsedMillis = durationMillis
	alert.Reason = state
	if match := projectTitlePattern.FindStringSubmatch(strings.TrimSpace(title)); match != nil {
		alert.IssueKey = match[0]
	}
	if metadata != "" {
		var parsed JobMetadata
		if err := json.Unmarshal([]byte(metadata), &parsed); err == nil {
			alert.IssueURL = parsed.IssueURL
			if parsed.ErrorClass != nil {
				alert.ErrorClass = string(parsed.ErrorClass.Class)
			}
		}
	}
	return alert, true, nil
}
