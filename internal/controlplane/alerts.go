package controlplane

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/config"
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
	// Restored from the retired fail.py payload so the Factory Alerts watcher
	// keeps its old contract.
	Repository     string        `json:"repository,omitempty"`
	Stage          string        `json:"stage,omitempty"`
	Classification string        `json:"classification,omitempty"`
	StderrTail     string        `json:"stderr_tail,omitempty"`
	Inspect        string        `json:"inspect,omitempty"`
	Healing        *HealingAlert `json:"healing,omitempty"`
}

// HealingAlert carries the self-healing diagnosis inside a fail alert and is
// the payload of the optional healing-<run>.json requeue notice. NeedsHuman
// marks the give-up cases a watcher must escalate; the remaining fields say
// what healing already tried.
type HealingAlert struct {
	Cause      HealCause `json:"cause,omitempty"`
	Summary    string    `json:"summary,omitempty"`
	Fix        string    `json:"fix,omitempty"`
	Attempts   int       `json:"attempts"`
	Budget     int       `json:"budget"`
	NeedsHuman bool      `json:"needs_human"`
	Reason     string    `json:"reason,omitempty"`
	RetryAfter string    `json:"retry_after,omitempty"`
}

// stderrTail keeps the last non-empty lines of a failure string, the shape the
// retired fail.py hook spooled.
func stderrTail(text string, lines int) string {
	fields := strings.Split(text, "\n")
	var kept []string
	for _, line := range fields {
		if strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
	}
	if len(kept) > lines {
		kept = kept[len(kept)-lines:]
	}
	return strings.Join(kept, "\n")
}

// alertingState reports whether a terminal process state emits a fail alert.
// Cancelled runs are operator actions, not failures.
func alertingState(state string) bool {
	return state == "failed" || state == "timed_out"
}

// webhookAttempts bounds the webhook delivery, with a per-request timeout and
// a short backoff between attempts. A final failure is logged and never fails
// the run completion or the file write.
const (
	webhookAttempts = 3
	webhookTimeout  = 10 * time.Second
)

// alertWebhook POSTs spooled alerts to the configured URL so the watcher can
// wake on push instead of polling.
type alertWebhook struct {
	url        string
	header     string
	secretEnv  string
	secretFile string
	client     *http.Client
	// retries is the sleep between attempts; variables so tests run fast.
	retries []time.Duration
}

func newAlertWebhook(alerts config.Alerts) *alertWebhook {
	if alerts.WebhookURL == "" {
		return nil
	}
	return &alertWebhook{
		url:        alerts.WebhookURL,
		header:     alerts.WebhookHeader,
		secretEnv:  alerts.WebhookSecretEnv,
		secretFile: alerts.WebhookSecretFile,
		client:     &http.Client{Timeout: webhookTimeout},
		retries:    []time.Duration{time.Second, 2 * time.Second},
	}
}

// secret resolves the webhook secret from the environment or a file. The
// value itself is never logged.
func (a *alertWebhook) secret() (string, error) {
	if a.secretEnv != "" {
		value := os.Getenv(a.secretEnv)
		if value == "" {
			return "", fmt.Errorf("webhook secret env %s is empty", a.secretEnv)
		}
		return value, nil
	}
	if a.secretFile != "" {
		body, err := os.ReadFile(a.secretFile)
		if err != nil {
			return "", fmt.Errorf("read webhook secret file: %w", err)
		}
		return strings.TrimSpace(string(body)), nil
	}
	return "", nil
}

// deliver POSTs the alert body to the webhook, retrying transient failures.
// Errors are logged and swallowed: the spooled file remains the source of
// truth the watcher polls.
func (w *failAlertWriter) deliver(body []byte) {
	if w == nil || w.webhook == nil {
		return
	}
	hook := w.webhook
	header := hook.header
	if header == "" {
		header = "X-Machinist-Alert-Token"
	}
	secret, err := hook.secret()
	if err != nil {
		log.Printf("alert webhook: %v (posting without the secret header)", err)
		secret = ""
	}
	client := hook.client
	if client == nil {
		client = &http.Client{Timeout: webhookTimeout}
	}
	retries := hook.retries
	for attempt := 0; ; attempt++ {
		var lastErr error
		request, err := http.NewRequest(http.MethodPost, hook.url, bytes.NewReader(body))
		if err == nil {
			request.Header.Set("Content-Type", "application/json")
			if secret != "" {
				request.Header.Set(header, secret)
			}
			response, err := client.Do(request)
			if err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				response.Body.Close()
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					return
				}
				lastErr = fmt.Errorf("alert webhook returned %s", response.Status)
			} else {
				lastErr = err
			}
		} else {
			lastErr = err
		}
		if attempt >= webhookAttempts-1 {
			log.Printf("alert webhook %s: %v", hook.url, lastErr)
			return
		}
		if attempt < len(retries) {
			time.Sleep(retries[attempt])
		}
	}
}

// failAlertWriter spools pending fail alert JSON files into the configured
// directory for an external alert poller, and POSTs each new alert to the
// optional webhook. Writing is idempotent per run: an existing alert file is
// left untouched and not re-posted, so a re-delivered completion never
// duplicates the alert. A zero fileMode keeps the default spool mode so
// writers built without a mode stay group-readable (MACH-72).
type failAlertWriter struct {
	directory string
	webhook   *alertWebhook
	fileMode  os.FileMode
}

// spoolMode returns the file mode spooled alerts are created with.
func (w *failAlertWriter) spoolMode() os.FileMode {
	if w.fileMode == 0 {
		return config.DefaultAlertFileMode
	}
	return w.fileMode
}

// alertDirectoryMode widens the spool file mode into a directory mode: the
// owner can always traverse, and a group that can read spooled files can also
// enter and manage them (the watcher moves files out of the spool).
func alertDirectoryMode(fileMode os.FileMode) os.FileMode {
	mode := fileMode.Perm() | 0o700
	if mode&0o070 != 0 {
		mode |= 0o070
	}
	return mode
}

// ensureAlertDirectory creates directory with the alert directory mode and
// re-applies that mode so the process umask cannot narrow it. MkdirAll leaves
// an existing directory alone; a setgid bit that is already set is kept.
func ensureAlertDirectory(directory string, fileMode os.FileMode) error {
	dirMode := alertDirectoryMode(fileMode)
	if err := os.MkdirAll(directory, dirMode); err != nil {
		return fmt.Errorf("create alert directory: %w", err)
	}
	info, err := os.Stat(directory)
	if err != nil {
		return fmt.Errorf("stat alert directory: %w", err)
	}
	mode := dirMode
	if info.Mode()&os.ModeSetgid != 0 {
		mode |= os.ModeSetgid
	}
	if info.Mode().Perm() != mode.Perm() {
		if err := os.Chmod(directory, mode); err != nil {
			return fmt.Errorf("mode alert directory: %w", err)
		}
	}
	return nil
}

// writeAlert spools one alert under name and wakes the webhook. The boolean
// reports whether a new file was written.
func (w *failAlertWriter) writeAlert(name string, alert FailAlert) (bool, error) {
	if w == nil || w.directory == "" {
		return false, nil
	}
	if err := ensureAlertDirectory(w.directory, w.spoolMode()); err != nil {
		return false, err
	}
	body, err := json.MarshalIndent(alert, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode fail alert: %w", err)
	}
	body = append(body, '\n')
	path := filepath.Join(w.directory, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, w.spoolMode())
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("create fail alert %s: %w", path, err)
	}
	// The create mode only fills in bits the umask allows; chmod explicitly so
	// the group bits survive a restrictive umask (MACH-72).
	if err = file.Chmod(w.spoolMode()); err != nil {
		file.Close()
		return false, fmt.Errorf("mode fail alert %s: %w", path, err)
	}
	if _, err = file.Write(body); err != nil {
		file.Close()
		return false, fmt.Errorf("write fail alert %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("write fail alert %s: %w", path, err)
	}
	go w.deliver(body)
	return true, nil
}

func (w *failAlertWriter) write(alert FailAlert) error {
	_, err := w.writeAlert("fail-"+alert.RunID+".json", alert)
	return err
}

// pendingFailAlert collects the failure alert payload for a terminal run. ok
// is false when the run did not end failed or timed out.
func (s *Store) pendingFailAlert(ctx context.Context, runID string) (FailAlert, bool, error) {
	var alert FailAlert
	var state, jobID, title, metadata, issueKey, errorText string
	var exitCode, durationMillis int64
	err := s.db.QueryRowContext(ctx, `SELECT r.state,r.job_id,COALESCE(r.command,''),COALESCE(r.repository,''),r.timeout_ms,COALESCE(r.exit_code,0),COALESCE(r.duration_millis,0),COALESCE(r.error,''),COALESCE(j.title,''),COALESCE(j.issue_key,''),COALESCE((SELECT metadata FROM job_metadata WHERE job_id=j.id),'') FROM runs r JOIN jobs j ON j.id=r.job_id WHERE r.id=?`,
		runID).Scan(&state, &jobID, &alert.Command, &alert.Repository, &alert.TimeoutMillis, &exitCode, &durationMillis, &errorText, &title, &issueKey, &metadata)
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
	alert.Stage = alert.Command
	alert.StderrTail = stderrTail(errorText, 40)
	alert.Inspect = fmt.Sprintf("sqlite3 -readonly %s \"select state,exit_code,error,result from runs where id='%s'\"", s.path, runID)
	if issueKey != "" {
		alert.IssueKey = issueKey
	} else if match := projectTitlePattern.FindStringSubmatch(strings.TrimSpace(title)); match != nil {
		alert.IssueKey = match[0]
	}
	if metadata != "" {
		var parsed JobMetadata
		if err := json.Unmarshal([]byte(metadata), &parsed); err == nil {
			alert.IssueURL = parsed.IssueURL
		}
	}
	return alert, true, nil
}
