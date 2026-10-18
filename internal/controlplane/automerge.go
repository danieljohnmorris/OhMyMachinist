package controlplane

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"html"

	"github.com/owainlewis/machinist/internal/issues"
)

// Per-project auto-merge and deploy policy (MACH-24). A factory PR for an
// auto-merge project is queued when the run opens it; a scheduler pass then
// walks each entry through a restart-tolerant state machine:
//
//	queued     gate on checks, conflicts and review verdicts, then merge
//	deploying  run the policy's deploy command (safe to resume after the
//	           deploy restarts this very control plane)
//	verifying  run the health check, with a few passes of grace
//	done       comment on the ticket (merge SHA + deploy/health result), move
//	           the ticket to Done and resolve the job
//	failed     move the ticket to Needs attention, alert Factory Alerts and
//	           roll back the previous release
//
// Every transition is recorded on the job so the run page shows the audit
// trail, and each state is committed before the step that can restart the
// control plane, so a restart resumes instead of re-merging.

const (
	autoMergeQueued     = "queued"
	autoMergeDeploying  = "deploying"
	autoMergeVerifying  = "verifying"
	autoMergeDone       = "done"
	autoMergeFailed     = "failed"
	autoMergeMaxDeploys = 2
	autoMergeMaxVerify  = 6
)

// defaultAutoMergeEvery is the auto-merge pass cadence.
const defaultAutoMergeEvery = 45 * time.Second

// autoMergeSchema holds the per-PR auto-merge queue.
const autoMergeSchema = `
CREATE TABLE IF NOT EXISTS automerge_jobs (
 job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
 run_id TEXT NOT NULL DEFAULT '',
 issue_key TEXT NOT NULL DEFAULT '',
 project_id TEXT NOT NULL DEFAULT '',
 repo TEXT NOT NULL DEFAULT '',
 pr_url TEXT NOT NULL DEFAULT '',
 pr_number INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL DEFAULT 'queued',
 merge_sha TEXT NOT NULL DEFAULT '',
 deploy_runs INTEGER NOT NULL DEFAULT 0,
 verify_attempts INTEGER NOT NULL DEFAULT 0,
 updated_at TEXT NOT NULL);`

// AutoMergeJob is one queued merge + deploy, keyed by the factory job whose
// run opened the pull request.
type AutoMergeJob struct {
	JobID          string `json:"job_id"`
	RunID          string `json:"run_id"`
	IssueKey       string `json:"issue_key"`
	ProjectID      string `json:"project_id"`
	Repo           string `json:"repo"`
	PRURL          string `json:"pr_url"`
	PRNumber       int    `json:"pr_number"`
	State          string `json:"state"`
	MergeSHA       string `json:"merge_sha"`
	DeployRuns     int    `json:"deploy_runs"`
	VerifyAttempts int    `json:"verify_attempts"`
	UpdatedAt      string `json:"updated_at"`
}

// AutoMergeRecord is the audit trail stored on the job and shown on the run
// page: what was merged, how the deploy and the health check went.
type AutoMergeRecord struct {
	PR           string `json:"pr,omitempty"`
	PRNumber     int    `json:"pr_number,omitempty"`
	MergeSHA     string `json:"merge_sha,omitempty"`
	MergeMethod  string `json:"merge_method,omitempty"`
	State        string `json:"state,omitempty"`
	DeployResult string `json:"deploy_result,omitempty"`
	HealthResult string `json:"health_result,omitempty"`
	Reason       string `json:"reason,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
}

// AutoMergeAlert is the Factory Alerts spool entry for a merge that did not
// deploy or verify cleanly. Operators are also pointed at the ticket.
type AutoMergeAlert struct {
	Type      string `json:"type"`
	Timestamp string `json:"ts"`
	IssueKey  string `json:"issue_key"`
	IssueURL  string `json:"issue_url"`
	PRURL     string `json:"pr_url"`
	JobID     string `json:"job_id"`
	Repo      string `json:"repo,omitempty"`
	Reason    string `json:"reason"`
	Message   string `json:"message"`
}

// prNumberPattern extracts the pull request number from a GitHub PR URL.
var prNumberPattern = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/pull/(\d+)/?$`)

// shellRunner executes a policy deploy, rollback or health-check command.
// Tests replace it; production runs bash.
type shellRunner interface {
	Run(ctx context.Context, command string, timeout time.Duration) (string, error)
}

type bashRunner struct{}

// Run executes command under bash and returns its combined output.
func (bashRunner) Run(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	command_ := exec.CommandContext(ctx, "bash", "-c", command)
	var output bytes.Buffer
	command_.Stdout = &output
	command_.Stderr = &output
	err := command_.Run()
	return strings.TrimSpace(output.String()), err
}

// QueueAutoMerge inserts or refreshes one queued auto-merge.
func (s *Store) QueueAutoMerge(ctx context.Context, entry AutoMergeJob) error {
	if entry.State == "" {
		entry.State = autoMergeQueued
	}
	entry.UpdatedAt = s.now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `INSERT INTO automerge_jobs
(job_id,run_id,issue_key,project_id,repo,pr_url,pr_number,state,merge_sha,deploy_runs,verify_attempts,updated_at)
VALUES(?,?,?,?,?,?,?,?,?,0,0,?)
ON CONFLICT(job_id) DO UPDATE SET run_id=excluded.run_id,pr_url=excluded.pr_url,pr_number=excluded.pr_number,updated_at=excluded.updated_at
WHERE automerge_jobs.state IN ('queued','deploying','verifying')`,
		entry.JobID, entry.RunID, entry.IssueKey, entry.ProjectID, entry.Repo,
		entry.PRURL, entry.PRNumber, entry.State, entry.MergeSHA, entry.UpdatedAt)
	if err != nil {
		return fmt.Errorf("queue auto-merge for job %s: %w", entry.JobID, err)
	}
	return nil
}

// PendingAutoMergeJobs lists every entry the runner still has to advance.
func (s *Store) PendingAutoMergeJobs(ctx context.Context) ([]AutoMergeJob, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id,run_id,issue_key,project_id,repo,pr_url,pr_number,state,merge_sha,deploy_runs,verify_attempts,updated_at
FROM automerge_jobs WHERE state IN ('queued','deploying','verifying') ORDER BY updated_at`)
	if err != nil {
		return nil, fmt.Errorf("read auto-merge queue: %w", err)
	}
	defer rows.Close()
	entries := []AutoMergeJob{}
	for rows.Next() {
		var entry AutoMergeJob
		if err := rows.Scan(&entry.JobID, &entry.RunID, &entry.IssueKey, &entry.ProjectID, &entry.Repo,
			&entry.PRURL, &entry.PRNumber, &entry.State, &entry.MergeSHA, &entry.DeployRuns, &entry.VerifyAttempts, &entry.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// SaveAutoMergeState persists one state-machine transition.
func (s *Store) SaveAutoMergeState(ctx context.Context, entry AutoMergeJob) error {
	entry.UpdatedAt = s.now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `UPDATE automerge_jobs
SET state=?,merge_sha=?,deploy_runs=?,verify_attempts=?,updated_at=? WHERE job_id=?`,
		entry.State, entry.MergeSHA, entry.DeployRuns, entry.VerifyAttempts, entry.UpdatedAt, entry.JobID)
	if err != nil {
		return fmt.Errorf("save auto-merge state for job %s: %w", entry.JobID, err)
	}
	return nil
}

// DeleteAutoMergeJob removes a finished queue entry; the audit record stays on
// the job metadata.
func (s *Store) DeleteAutoMergeJob(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM automerge_jobs WHERE job_id=?`, jobID)
	if err != nil {
		return fmt.Errorf("delete auto-merge entry %s: %w", jobID, err)
	}
	return nil
}

// SaveAutoMergeMetadata merges an auto-merge audit record into the job
// metadata shown on the run page.
func (s *Store) SaveAutoMergeMetadata(ctx context.Context, jobID string, record AutoMergeRecord) error {
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
	existing["automerge"] = json.RawMessage(encoded)
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

// autoMergePass advances every queued merge + deploy by one step. It runs on
// the scheduler loop; each entry commits its state before the step that can
// restart the control plane, so a restart resumes the flow.
func (s *Server) autoMergePass(ctx context.Context) error {
	if s.planeIntake == nil {
		return nil
	}
	cfg, err := s.planeIntake.config(ctx)
	if err != nil {
		return err
	}
	entries, err := s.store.PendingAutoMergeJobs(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := s.processAutoMerge(ctx, cfg, entry, 0); err != nil {
			s.logAutoMerge(entry, err)
		}
	}
	return nil
}

func (s *Server) logAutoMerge(entry AutoMergeJob, err error) {
	log.Printf("automerge: %s %s #%d: %v", entry.IssueKey, entry.State, entry.PRNumber, err)
}

// processAutoMerge advances one entry. depth bounds the in-pass chaining
// (merge → deploy → verify) so a state loop cannot recurse forever.
func (s *Server) processAutoMerge(ctx context.Context, cfg planeConfig, entry AutoMergeJob, depth int) error {
	if depth > 4 {
		return errors.New("too many auto-merge transitions in one pass")
	}
	policy, ok := cfg.PolicyFor(entry.IssueKey)
	if !ok || !policy.AutoMerge {
		// Policy turned off since the PR was queued: leave the PR alone.
		if err := s.store.DeleteAutoMergeJob(ctx, entry.JobID); err != nil {
			return err
		}
		log.Printf("automerge: %s dropped; auto-merge is off for %s", entry.IssueKey, strings.SplitN(entry.IssueKey, "-", 2)[0])
		return nil
	}
	job, err := s.store.GetJob(ctx, entry.JobID)
	if err != nil {
		return fmt.Errorf("read job: %w", err)
	}
	switch entry.State {
	case autoMergeQueued:
		return s.autoMergeGate(ctx, cfg, policy, entry, job, depth)
	case autoMergeDeploying:
		return s.autoMergeDeploy(ctx, cfg, policy, entry, job, depth)
	case autoMergeVerifying:
		return s.autoMergeVerify(ctx, cfg, policy, entry, job, depth)
	default:
		return s.store.DeleteAutoMergeJob(ctx, entry.JobID)
	}
}

// autoMergeGate applies the checks/conflict/review gates and, when they pass,
// merges the pull request.
func (s *Server) autoMergeGate(ctx context.Context, cfg planeConfig, policy config.MergePolicy, entry AutoMergeJob, job Job, depth int) error {
	status, err := s.github.PullRequestStatus(ctx, entry.Repo, entry.PRNumber)
	if err != nil {
		return fmt.Errorf("read PR status: %w", err)
	}
	if ok, pending, failedCheck := EvaluateChecks(status.Rollup); !ok {
		if pending {
			return nil // checks still running; re-run on the next pass
		}
		return s.autoMergeFail(ctx, policy, entry, fmt.Sprintf("checks failed: %s", failedCheck), false)
	}
	switch {
	case strings.EqualFold(status.Mergeable, "CONFLICTING"), strings.EqualFold(status.MergeState, "DIRTY"):
		return s.autoMergeFail(ctx, policy, entry, "branch conflicts with the base; not merging", false)
	case strings.EqualFold(status.MergeState, "BEHIND"):
		// Rebase-equivalent: bring the branch up to date with the base, then
		// the next pass re-runs the checks on the updated branch.
		if err := s.github.UpdatePullRequestBranch(ctx, entry.Repo, entry.PRNumber); err != nil {
			return err
		}
		log.Printf("automerge: %s updated PR #%d branch onto its base; re-running checks", entry.IssueKey, entry.PRNumber)
		return nil
	case strings.EqualFold(status.Mergeable, "UNKNOWN"):
		return nil // GitHub still computing mergeability
	}
	if verdict := reviewVerdict(job); verdict != "" {
		return s.autoMergeFail(ctx, policy, entry, "post-PR review blocked the merge: "+verdict, false)
	}
	sha, err := s.github.MergePullRequest(ctx, entry.Repo, entry.PRNumber, policy.MergeMethod)
	if err != nil {
		return s.autoMergeFail(ctx, policy, entry, fmt.Sprintf("merge failed: %v", err), false)
	}
	entry.State = autoMergeDeploying
	entry.MergeSHA = sha
	if err := s.store.SaveAutoMergeState(ctx, entry); err != nil {
		return err
	}
	s.recordAutoMerge(ctx, job.ID, AutoMergeRecord{
		PR: entry.PRURL, PRNumber: entry.PRNumber, MergeSHA: sha, MergeMethod: policy.MergeMethod,
		State: autoMergeDeploying, UpdatedAt: s.now().UTC().Format(time.RFC3339),
	})
	log.Printf("automerge: %s merged PR #%d as %s (%s); deploying", entry.IssueKey, entry.PRNumber, policy.MergeMethod, shortSHA(sha))
	return s.processAutoMerge(ctx, cfg, entry, depth+1)
}

// reviewVerdict returns the post-PR review verdict blocking a merge, if any.
// The built-in review runs before delivery and already keeps BLOCKED PRs from
// opening; this guards verdicts recorded on the job afterwards.
func reviewVerdict(job Job) string {
	if job.Metadata == nil || job.Metadata.ReviewVerdict == "" {
		return ""
	}
	var parsed struct {
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal([]byte(job.Metadata.ReviewVerdict), &parsed); err != nil {
		// A plain-string verdict ("PASS"/"BLOCKED" from the delivery script).
		verdict := strings.ToUpper(strings.TrimSpace(job.Metadata.ReviewVerdict))
		if verdict == "BLOCKED" {
			return job.Metadata.ReviewVerdict
		}
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(parsed.Verdict)) {
	case "blocked", "request_changes", "changes_requested", "rejected", "block":
		return parsed.Verdict
	}
	return ""
}

// autoMergeDeploy runs the policy deploy. After a mid-deploy restart the
// entry resumes here: a health check that already passes proves the deploy
// completed, otherwise the idempotent deploy is re-run (bounded).
func (s *Server) autoMergeDeploy(ctx context.Context, cfg planeConfig, policy config.MergePolicy, entry AutoMergeJob, job Job, depth int) error {
	if entry.DeployRuns > 0 {
		if _, err := s.runAutoMergeHealthCheck(ctx, policy, entry.MergeSHA); err == nil {
			entry.State = autoMergeVerifying
			if err := s.store.SaveAutoMergeState(ctx, entry); err != nil {
				return err
			}
			return s.processAutoMerge(ctx, cfg, entry, depth+1)
		}
	}
	if entry.DeployRuns >= autoMergeMaxDeploys {
		return s.autoMergeFail(ctx, policy, entry, "deploy was interrupted and never verified", true)
	}
	entry.DeployRuns++
	if err := s.store.SaveAutoMergeState(ctx, entry); err != nil {
		return err
	}
	if strings.TrimSpace(policy.DeployCommand) != "" {
		output, err := s.deployRunner.Run(ctx, policy.DeployCommand, policy.DeployDeadline())
		if err != nil {
			return s.autoMergeFail(ctx, policy, entry, fmt.Sprintf("deploy failed: %v", err), true)
		}
		s.recordAutoMerge(ctx, job.ID, AutoMergeRecord{
			PR: entry.PRURL, PRNumber: entry.PRNumber, MergeSHA: entry.MergeSHA, MergeMethod: policy.MergeMethod,
			State: autoMergeVerifying, DeployResult: bound(output, 2000), UpdatedAt: s.now().UTC().Format(time.RFC3339),
		})
	}
	entry.State = autoMergeVerifying
	if err := s.store.SaveAutoMergeState(ctx, entry); err != nil {
		return err
	}
	return s.processAutoMerge(ctx, cfg, entry, depth+1)
}

// autoMergeVerify runs the policy health check. A few failed passes are
// grace for a service that is still coming up; after that the deploy is
// rolled back and the ticket handed back.
func (s *Server) autoMergeVerify(ctx context.Context, cfg planeConfig, policy config.MergePolicy, entry AutoMergeJob, job Job, depth int) error {
	result, err := s.runAutoMergeHealthCheck(ctx, policy, entry.MergeSHA)
	if err == nil {
		return s.autoMergeDone(ctx, policy, entry, job, result)
	}
	entry.VerifyAttempts++
	if entry.VerifyAttempts < autoMergeMaxVerify {
		if err := s.store.SaveAutoMergeState(ctx, entry); err != nil {
			return err
		}
		log.Printf("automerge: %s health check attempt %d/%d failed: %v", entry.IssueKey, entry.VerifyAttempts, autoMergeMaxVerify, err)
		return nil
	}
	return s.autoMergeFail(ctx, policy, entry, fmt.Sprintf("health check failed: %v", err), true)
}

// autoMergeDone finishes a verified merge: ticket to Done with the merge SHA
// and deploy/health result, job resolved, queue entry removed.
func (s *Server) autoMergeDone(ctx context.Context, policy config.MergePolicy, entry AutoMergeJob, job Job, health string) error {
	entry.State = autoMergeDone
	if err := s.store.SaveAutoMergeState(ctx, entry); err != nil {
		return err
	}
	if err := s.store.SaveAutoMergeMetadata(ctx, job.ID, AutoMergeRecord{
		PR: entry.PRURL, PRNumber: entry.PRNumber, MergeSHA: entry.MergeSHA, MergeMethod: policy.MergeMethod,
		State: autoMergeDone, HealthResult: health, UpdatedAt: s.now().UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	if err := s.store.DeleteAutoMergeJob(ctx, entry.JobID); err != nil {
		return err
	}
	if client := s.planeClientFor(job); client != nil && entry.IssueKey != "" {
		var body strings.Builder
		fmt.Fprintf(&body, "<p><strong>Auto-merged and deployed (MACH-24)</strong></p>")
		fmt.Fprintf(&body, "<p>PR <a href=\"%s\">#%d</a> merged as <code>%s</code> (merge SHA <code>%s</code>).</p>",
			entry.PRURL, entry.PRNumber, policy.MergeMethod, entry.MergeSHA)
		if health != "" {
			fmt.Fprintf(&body, "<p>Health check: %s</p>", html.EscapeString(health))
		}
		if err := client.PostComment(ctx, entry.IssueKey, body.String()); err != nil {
			log.Printf("automerge: comment on %s: %v", entry.IssueKey, err)
		}
		if err := client.SetState(ctx, entry.IssueKey, issues.CompletedPlaneState); err != nil {
			log.Printf("automerge: set %s to %s: %v", entry.IssueKey, issues.CompletedPlaneState, err)
		}
	}
	if err := s.store.MarkJobResolved(ctx, job.ID, "automerge", fmt.Sprintf("PR #%d merged (%s); deploy verified", entry.PRNumber, shortSHA(entry.MergeSHA)), s.now().UTC()); err != nil {
		return err
	}
	log.Printf("automerge: %s done: PR #%d merged (%s), deploy verified (%s)", entry.IssueKey, entry.PRNumber, shortSHA(entry.MergeSHA), health)
	return nil
}

// autoMergeFail hands a blocked merge or failed deploy back to humans: Plane
// to Needs attention with a comment, a Factory Alerts spool entry, and — when
// the merge already landed and a rollback exists — the previous release back.
func (s *Server) autoMergeFail(ctx context.Context, policy config.MergePolicy, entry AutoMergeJob, reason string, rollback bool) error {
	entry.State = autoMergeFailed
	if err := s.store.SaveAutoMergeState(ctx, entry); err != nil {
		return err
	}
	job, err := s.store.GetJob(ctx, entry.JobID)
	if err != nil {
		return fmt.Errorf("read job for failed auto-merge: %w", err)
	}
	record := AutoMergeRecord{
		PR: entry.PRURL, PRNumber: entry.PRNumber, MergeSHA: entry.MergeSHA, MergeMethod: policy.MergeMethod,
		State: autoMergeFailed, Reason: reason, UpdatedAt: s.now().UTC().Format(time.RFC3339),
	}
	if rollback && entry.MergeSHA != "" && strings.TrimSpace(policy.RollbackCommand) != "" {
		output, rollErr := s.deployRunner.Run(ctx, policy.RollbackCommand, policy.DeployDeadline())
		if rollErr != nil {
			record.Reason += fmt.Sprintf("; rollback failed: %v", rollErr)
			log.Printf("automerge: %s rollback failed: %v", entry.IssueKey, rollErr)
		} else {
			record.Reason += "; rolled back to the previous release"
			record.DeployResult = bound(output, 2000)
		}
	}
	if err := s.store.SaveAutoMergeMetadata(ctx, job.ID, record); err != nil {
		return err
	}
	if err := s.store.DeleteAutoMergeJob(ctx, entry.JobID); err != nil {
		return err
	}
	alert := AutoMergeAlert{
		Type: "machinist.automerge_failed", Timestamp: s.now().UTC().Format(time.RFC3339),
		IssueKey: entry.IssueKey, PRURL: entry.PRURL, JobID: entry.JobID, Repo: entry.Repo, Reason: record.Reason,
		Message: fmt.Sprintf("%s: auto-merge of PR #%d did not complete: %s (job %s).", entry.IssueKey, entry.PRNumber, record.Reason, entry.JobID),
	}
	if client := s.planeClientFor(job); client != nil {
		alert.IssueURL = client.BrowseURL(entry.IssueKey)
	}
	if err := s.writeAutoMergeAlert(ctx, entry, alert); err != nil {
		log.Printf("automerge: write alert: %v", err)
	}
	if client := s.planeClientFor(job); client != nil && entry.IssueKey != "" {
		var body strings.Builder
		fmt.Fprintf(&body, "<p><strong>Auto-merge did not complete (MACH-24)</strong></p>")
		fmt.Fprintf(&body, "<p>%s</p>", html.EscapeString(record.Reason))
		if entry.MergeSHA != "" {
			fmt.Fprintf(&body, "<p>Merge SHA <code>%s</code>; PR <a href=\"%s\">#%d</a>.</p>", entry.MergeSHA, entry.PRURL, entry.PRNumber)
		}
		if err := client.PostComment(ctx, entry.IssueKey, body.String()); err != nil {
			log.Printf("automerge: comment on %s: %v", entry.IssueKey, err)
		}
		if err := client.SetState(ctx, entry.IssueKey, PlaneNeedsAttentionState); err != nil {
			log.Printf("automerge: set %s to %s: %v", entry.IssueKey, PlaneNeedsAttentionState, err)
		}
	}
	log.Printf("automerge: %s failed: %s", entry.IssueKey, record.Reason)
	return nil
}

// writeAutoMergeAlert spools the failure alert for the external poller.
func (s *Server) writeAutoMergeAlert(ctx context.Context, entry AutoMergeJob, alert AutoMergeAlert) error {
	if s.planeIntake == nil {
		return nil
	}
	cfg, err := s.planeIntake.config(ctx)
	if err != nil {
		return err
	}
	if cfg.AlertsDirectory == "" {
		return nil
	}
	return writeAlertFile(cfg.AlertsDirectory, fmt.Sprintf("automerge-%s-%s.json", entry.IssueKey, entry.JobID), alert, cfg.AlertsFileMode)
}

// runAutoMergeHealthCheck executes the policy health check with {sha} bound
// to the merge SHA. A command check must exit 0 and (when expect is set) its
// output must contain the expected text; a URL check must return the expected
// status (default 200) and (when expect is set) a body containing it.
func (s *Server) runAutoMergeHealthCheck(ctx context.Context, policy config.MergePolicy, sha string) (string, error) {
	check := policy.HealthCheck
	if check == nil {
		return "no health check configured", nil
	}
	if strings.TrimSpace(check.Command) != "" {
		output, err := s.deployRunner.Run(ctx, bindMergeSHA(check.Command, sha), 2*time.Minute)
		if err != nil {
			return "", err
		}
		if expect := bindMergeSHA(check.Expect, sha); expect != "" && !strings.Contains(output, expect) {
			return "", fmt.Errorf("output does not contain %q: %s", expect, bound(output, 512))
		}
		return bound(output, 2000), nil
	}
	if strings.TrimSpace(check.URL) != "" {
		status := check.Status
		if status == 0 {
			status = http.StatusOK
		}
		requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, check.URL, nil)
		if err != nil {
			return "", err
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		if response.StatusCode != status {
			return "", fmt.Errorf("status %d, want %d", response.StatusCode, status)
		}
		if expect := bindMergeSHA(check.Expect, sha); expect != "" && !strings.Contains(string(body), expect) {
			return "", fmt.Errorf("body does not contain %q", expect)
		}
		return fmt.Sprintf("HTTP %d from %s", response.StatusCode, check.URL), nil
	}
	return "", errors.New("health check has neither command nor url")
}

func bindMergeSHA(value, sha string) string {
	return strings.ReplaceAll(value, "{sha}", sha)
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// recordAutoMerge merges an audit record into the job metadata; failures are
// logged, never fatal to the flow.
func (s *Server) recordAutoMerge(ctx context.Context, jobID string, record AutoMergeRecord) {
	if err := s.store.SaveAutoMergeMetadata(ctx, jobID, record); err != nil {
		log.Printf("automerge: record audit on job %s: %v", jobID, err)
	}
}

// queueAutoMerge enqueues a factory PR for an auto-merge project; it is
// installed on the Plane intake when the server starts.
func (s *Server) queueAutoMerge(ctx context.Context, candidate PlaneSyncCandidate, prURL string) error {
	if s.planeIntake == nil {
		return nil
	}
	cfg, err := s.planeIntake.config(ctx)
	if err != nil {
		return err
	}
	policy, ok := cfg.PolicyFor(candidate.IssueKey)
	if !ok || !policy.AutoMerge {
		return nil
	}
	entry := AutoMergeJob{
		JobID: candidate.JobID, RunID: candidate.RunID, IssueKey: candidate.IssueKey,
		ProjectID: candidate.ProjectID, Repo: candidate.Repo, PRURL: prURL,
	}
	if match := prNumberPattern.FindStringSubmatch(prURL); match != nil {
		fmt.Sscanf(match[3], "%d", &entry.PRNumber)
		if entry.Repo == "" {
			entry.Repo = match[1] + "/" + match[2]
		}
	}
	if entry.Repo == "" {
		if slug, ok := s.githubRepositories[candidate.Repo]; ok {
			entry.Repo = slug
		}
	}
	if entry.Repo == "" || entry.PRNumber == 0 {
		return fmt.Errorf("cannot resolve repository and PR number from %q", prURL)
	}
	if err := s.store.QueueAutoMerge(ctx, entry); err != nil {
		return err
	}
	log.Printf("automerge: %s queued PR #%d (%s) for merge + deploy", entry.IssueKey, entry.PRNumber, entry.Repo)
	return nil
}
