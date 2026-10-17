package controlplane

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/config"
)

// Self-healing (MACH-25): when a run fails, times out or is refused,
// Machinist classifies the cause from the run's error and transcript, then
// applies the matching fix — requeue within a per-ticket retry budget,
// schedule a backoff retry for infra, or hand the ticket back to a human.
// The cause and fix are commented on Plane and stored on the job so the run
// page shows what happened.

// HealCause classifies why a run failed.
type HealCause string

const (
	// CauseInfra is a dead endpoint, offline host or refused connection: back
	// off and retry.
	CauseInfra HealCause = "infra"
	// CauseStale is a stale checkout: fetch and rebase onto the fresh base.
	CauseStale HealCause = "stale"
	// CauseTest is a code or test failure: amend on the same branch.
	CauseTest HealCause = "test"
	// CauseSpec is a spec problem a human has to resolve: no retry.
	CauseSpec HealCause = "spec"
	// CauseSecret is a secret-guard refusal: report the offending files.
	CauseSecret HealCause = "secret"
)

// PlaneNeedsAttentionState is the state Machinist moves tickets to when the
// retry budget is exhausted or a human has to intervene.
const PlaneNeedsAttentionState = "Needs attention"

// healInfraBackoff is the base delay before the first infra-cause retry; each
// further infra retry doubles it.
const healInfraBackoff = 5 * time.Minute

// healPromptSuffix asks the coder to diagnose the prior attempt before
// continuing; the transcript arrives in the prior-attempt block.
const healPromptSuffix = `## Failure diagnosis

This is a self-healing retry of a failed attempt on the same branch. Diagnose the
prior failure from its transcript above, then apply the fix for the classified
cause below before completing the task.`

// healingMetadataKey prefixes job metadata fields the healing pass owns.
const healingMetadataKey = "healing"

// healingRecord is stored on the job so the run page and Plane comments show
// the cause and the applied fix.
type healingRecord struct {
	Cause      HealCause `json:"cause"`
	Summary    string    `json:"summary,omitempty"`
	Fix        string    `json:"fix,omitempty"`
	Attempt    int       `json:"attempt"`
	Budget     int       `json:"budget"`
	Exhausted  bool      `json:"exhausted,omitempty"`
	RetryAfter string    `json:"retry_after,omitempty"`
}

var (
	// Infra: dead GLM URL, offline hosts, refused or reset connections, DNS.
	healInfraPattern = regexp.MustCompile(`(?i)(connection refused|connection reset|no such host|network is unreachable|i/o timeout|dial tcp|econnrefused|502 bad gateway|503 service unavailable|api\.openai\.com|glm|endpoints? (is|are) (dead|down|offline)|host (is )?unreachable)`)
	// Stale checkout: the base moved under the lane.
	healStalePattern = regexp.MustCompile(`(?i)(non-fast-forward|fetch first|divergent branches|behind (its )?remote|your branch is behind|stale checkout|needs rebase|failed to push some refs)`)
	// Test or code failure: the fix is an amend on the same branch.
	healTestPattern = regexp.MustCompile(`(?i)(--- FAIL:|FAIL\t|go test|pytest|failed tests?|\d+ tests? (failed|failing)|compile error|build failed|assertion error|expect\(|lint error|typeerror|tests? failed)`)
	// Secret guard: the delivery refused real env files or staged secrets.
	healSecretPattern = regexp.MustCompile(`(?i)(secret-file guard|secret guard|trufflehog|refused to (deliver|commit|push)|possible secret|verified secret)`)
	// Spec problem: the agent could not proceed because the ticket is wrong.
	healSpecPattern = regexp.MustCompile(`(?i)spec(ification)? (is )?(unclear|ambiguous|missing|conflicting|incomplete|wrong)`)
	// A dirty lane the runner could not save pauses the lane for an operator;
	// requeueing would just hammer the same broken checkout.
	healLaneFailurePattern = regexp.MustCompile(`(?i)(lane checkout is dirty|could not be saved|stash .* failed)`)

	// Offending files: quoted or listed paths in a guard refusal.
	healFilePattern = regexp.MustCompile(`[\w./-]*\.(env|pem|key|secret)[\w.-]*`)
)

// classifyFailure maps failure text to a cause, in priority order: a secret
// refusal outranks infra noise in the same log, infra outranks a stale
// checkout, and everything else defaults to a test/code failure — the most
// common recoverable case. The second result lists offending files for a
// secret refusal.
func classifyFailure(text string) (HealCause, []string) {
	if healSecretPattern.MatchString(text) {
		return CauseSecret, healOffendingFiles(text)
	}
	if healInfraPattern.MatchString(text) {
		return CauseInfra, nil
	}
	if healSpecPattern.MatchString(text) {
		return CauseSpec, nil
	}
	if healStalePattern.MatchString(text) {
		return CauseStale, nil
	}
	if healTestPattern.MatchString(text) {
		return CauseTest, nil
	}
	return CauseTest, nil
}

func healOffendingFiles(text string) []string {
	seen := map[string]bool{}
	var files []string
	for _, match := range healFilePattern.FindAllString(text, -1) {
		match = strings.Trim(match, ".,;:'\"")
		if match == "" || seen[match] {
			continue
		}
		seen[match] = true
		files = append(files, match)
	}
	return files
}

// healingFixInstruction is the per-cause instruction appended to the retry
// prompt. Spec and secret causes never requeue, so they have none.
func healingFixInstruction(cause HealCause, files []string) string {
	switch cause {
	case CauseStale:
		return healPromptSuffix + "\n\nClassified cause: a stale checkout. Fetch the remote and rebase your branch onto the fresh base before making changes; replay your work on top of it."
	case CauseTest:
		return healPromptSuffix + "\n\nClassified cause: a code or test failure. Fix the code and amend the commit on the same branch; do not open a new branch."
	case CauseInfra:
		return healPromptSuffix + "\n\nClassified cause: an infrastructure outage (a dead endpoint or offline host). The underlying task was not the problem; retry the delivery as-is once the endpoint responds again."
	default:
		return healPromptSuffix
	}
}

// healingFixDescription is the human-facing fix shown in the Plane comment
// and on the run page.
func healingFixDescription(cause HealCause, files []string) string {
	switch cause {
	case CauseInfra:
		return "Backing off and retrying the same job."
	case CauseStale:
		return "Requeued with instructions to fetch and rebase onto the fresh base."
	case CauseTest:
		return "Requeued to fix the code and amend on the same branch."
	case CauseSpec:
		return "No retry: the ticket needs a human. Spec problem left for review."
	case CauseSecret:
		report := "No retry: the secret guard refused the delivery. Offending files:"
		if len(files) == 0 {
			return report + " none identified; check the run transcript."
		}
		return report + " " + strings.Join(files, ", ")
	default:
		return ""
	}
}

// RunFailureContext returns the recorded error and the raw event transcript
// for a run, the inputs to diagnosis.
func (s *Store) RunFailureContext(ctx context.Context, runID string) (string, string, error) {
	var errorText, events string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(error,''),COALESCE(events,'') FROM runs WHERE id=?`, runID).Scan(&errorText, &events)
	return errorText, events, err
}

// runAttemptCount returns how many runs (attempts) a job has already used.
func (s *Store) runAttemptCount(ctx context.Context, jobID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE job_id=?`, jobID).Scan(&count)
	return count, err
}

// healDiagnosis is the outcome of the diagnose step: a cause, a short summary
// and, for secret refusals, the offending files.
type healDiagnosis struct {
	Cause   HealCause
	Summary string
	Files   []string
}

// diagnoseFailure builds the diagnose prompt from the failure context and
// classifies the cause. The configured diagnose command (omp at low effort)
// runs first; the keyword classifier is the fallback so self-healing still
// works without an executor on the host.
func (s *Server) diagnoseFailure(ctx context.Context, failureText string) healDiagnosis {
	if len(s.healing.DiagnoseCommand) > 0 {
		if diagnosis, err := runDiagnoseCommand(ctx, s.healing, failureText); err == nil {
			return diagnosis
		} else {
			log.Printf("healing diagnose command failed, falling back to keyword classifier: %v", err)
		}
	}
	cause, files := classifyFailure(failureText)
	return healDiagnosis{Cause: cause, Summary: "classified from the run transcript", Files: files}
}

const diagnosePromptHeader = `You are triaging a failed Machinist run. Below are the run's error and transcript.
Classify the root cause as exactly one of: infra (a dead endpoint, offline host or refused connection), stale (the checkout is behind its base), test (a code or test failure), spec (the ticket is wrong or incomplete), secret (a secret guard refused the delivery).
Reply with only a JSON object: {"cause":"...","summary":"one sentence"}. For a secret refusal also list the offending file paths as {"files":["..."]}.

`

// runDiagnoseCommand feeds the failure to the diagnose executor and parses the
// JSON classification.
func runDiagnoseCommand(ctx context.Context, healing config.Healing, failureText string) (healDiagnosis, error) {
	deadline, err := healing.DiagnoseDeadline()
	if err != nil {
		return healDiagnosis{}, err
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	command := exec.CommandContext(timeoutCtx, healing.DiagnoseCommand[0], healing.DiagnoseCommand[1:]...)
	command.Stdin = strings.NewReader(diagnosePromptHeader + failureText)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return healDiagnosis{}, fmt.Errorf("diagnose command: %w (stderr: %s)", err, bound(stderr.String(), 512))
	}
	output := strings.TrimSpace(stdout.String())
	start, end := strings.Index(output, "{"), strings.LastIndex(output, "}")
	if start < 0 || end <= start {
		return healDiagnosis{}, fmt.Errorf("diagnose output is not JSON: %s", bound(output, 512))
	}
	var parsed struct {
		Cause   string   `json:"cause"`
		Summary string   `json:"summary"`
		Files   []string `json:"files"`
	}
	if err := json.Unmarshal([]byte(output[start:end+1]), &parsed); err != nil {
		return healDiagnosis{}, fmt.Errorf("parse diagnose output: %w", err)
	}
	switch HealCause(parsed.Cause) {
	case CauseInfra, CauseStale, CauseTest, CauseSpec, CauseSecret:
	default:
		return healDiagnosis{}, fmt.Errorf("unknown diagnose cause %q", parsed.Cause)
	}
	return healDiagnosis{
		Cause:   HealCause(parsed.Cause),
		Summary: parsed.Summary,
		Files:   parsed.Files,
	}, nil
}

// healBudget resolves the retry budget for a job: the configured default, an
// optional per-project override, or a settings-table override
// (healing.max_retries / healing.max_retries.<PROJECT>) set at runtime.
func (s *Server) healBudget(ctx context.Context, job Job) (int, error) {
	if project := jobProject(job); project != "" {
		if value, ok, err := s.store.Setting(ctx, "healing.max_retries."+project); err != nil {
			return 0, err
		} else if ok {
			if parsed, err := parseInt(strings.TrimSpace(value)); err == nil && parsed >= 0 {
				return parsed, nil
			}
		}
	}
	if value, ok, err := s.store.Setting(ctx, "healing.max_retries"); err != nil {
		return 0, err
	} else if ok {
		if parsed, err := parseInt(strings.TrimSpace(value)); err == nil && parsed >= 0 {
			return parsed, nil
		}
	}
	return s.healing.DiagnoseBudget(jobProject(job)), nil
}

func jobProject(job Job) string {
	if job.Metadata != nil {
		return job.Metadata.Project
	}
	return ""
}

// parseInt parses a non-negative integer without importing strconv at every
// call site that only needs this one conversion.
func parseInt(value string) (int, error) {
	if value == "" {
		return 0, errors.New("empty integer")
	}
	result := 0
	for _, digit := range []byte(value) {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("not an integer: %q", value)
		}
		result = result*10 + int(digit-'0')
	}
	return result, nil
}

// HealRun runs the self-healing pass for one terminal failed or timed-out
// run: diagnose, classify, then apply the matching fix. It also owns failure
// alerting (MACH-70): exactly one pending fail alert is spooled when healing
// gives up — or straight away for workflow jobs and healing errors — plus the
// optional healing-<run>.json notice when a retry is scheduled. Errors are
// logged and never fail the completion delivery.
func (s *Server) HealRun(ctx context.Context, runID string) {
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		log.Printf("healing: read run %q: %v", runID, err)
		s.alertHealingFailure(ctx, runID, err)
		return
	}
	if !alertingState(run.State) {
		return
	}
	var jobID string
	if err := s.store.db.QueryRowContext(ctx, `SELECT job_id FROM runs WHERE id=?`, runID).Scan(&jobID); err != nil {
		log.Printf("healing: read job for run %q: %v", runID, err)
		s.alertHealingFailure(ctx, runID, err)
		return
	}
	job, err := s.store.GetJob(ctx, jobID)
	if err != nil {
		log.Printf("healing: read job %q: %v", jobID, err)
		s.alertHealingFailure(ctx, runID, err)
		return
	}
	errorText, events, err := s.store.RunFailureContext(ctx, runID)
	if err != nil {
		log.Printf("healing: read failure context for run %q: %v", runID, err)
		s.alertHealingFailure(ctx, runID, err)
		return
	}
	failureText := strings.Join([]string{errorText, run.Summary, strings.Join(decodeEventTranscript(events), "\n")}, "\n")
	// The keyword classifier is free and always available; it classifies the
	// paths below that never reach the diagnose executor.
	cause, _ := classifyFailure(failureText)
	keyword := string(cause)
	// Workflow steps retry through the operator-controlled workflow actions;
	// healing requeues plain jobs where a new run is the whole attempt.
	var workflowRow int
	if err := s.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM workflow_jobs WHERE job_id=?`, job.ID).Scan(&workflowRow); err != nil {
		log.Printf("healing: check workflow for job %q: %v", job.ID, err)
		s.alertHealingFailure(ctx, runID, err)
		return
	}
	if workflowRow > 0 {
		// Workflow jobs cannot be requeued by healing; hand them to the watcher.
		s.spoolFailAlert(ctx, runID, &HealingAlert{
			NeedsHuman: true,
			Reason:     "workflow job: healing does not requeue it, retry through the workflow actions",
		}, keyword)
		return
	}
	if healLaneFailurePattern.MatchString(failureText) {
		// The lane paused itself; a retry would meet the same broken checkout.
		s.spoolFailAlert(ctx, runID, &HealingAlert{
			NeedsHuman: true,
			Reason:     "lane checkout is dirty: the lane paused itself and an operator must clear it",
		}, keyword)
		return
	}
	diagnosis := s.diagnoseFailure(ctx, failureText)

	attempts, err := s.store.runAttemptCount(ctx, job.ID)
	if err != nil {
		log.Printf("healing: count attempts for job %q: %v", job.ID, err)
		s.alertHealingFailure(ctx, runID, err)
		return
	}
	budget, err := s.healBudget(ctx, job)
	if err != nil {
		log.Printf("healing: resolve budget for job %q: %v", job.ID, err)
		s.alertHealingFailure(ctx, runID, err)
		return
	}
	// Each queued run is one attempt; the budget counts retries after the
	// first, so the cap on total runs is budget+1.
	// exhausted is true only when the retry budget is actually spent; spec
	// and secret causes stop immediately with budget remaining.
	giveUp := func(reason string, exhausted bool) {
		s.finishHealing(ctx, job.ID, job, diagnosis, budget, attempts, exhausted, time.Time{})
		s.spoolFailAlert(ctx, runID, &HealingAlert{
			Cause: diagnosis.Cause, Summary: diagnosis.Summary,
			Fix:      healingFixDescription(diagnosis.Cause, diagnosis.Files),
			Attempts: attempts, Budget: budget, NeedsHuman: true, Reason: reason,
		}, string(diagnosis.Cause))
	}
	if attempts >= budget+1 {
		giveUp("retry budget exhausted", true)
		return
	}
	switch diagnosis.Cause {
	case CauseSpec, CauseSecret:
		giveUp("needs a human: "+healingFixDescription(diagnosis.Cause, diagnosis.Files), false)
	case CauseInfra:
		backoff := s.healingBackoff(attempts)
		retryAfter := s.now().UTC().Add(backoff)
		s.finishHealing(ctx, job.ID, job, diagnosis, budget, attempts, false, retryAfter)
		s.maybeHealingAlert(ctx, runID, &HealingAlert{
			Cause: diagnosis.Cause, Summary: diagnosis.Summary,
			Fix:      healingFixDescription(diagnosis.Cause, diagnosis.Files),
			Attempts: attempts, Budget: budget,
			Reason:     "infra failure: retry scheduled after backoff",
			RetryAfter: retryAfter.Format(time.RFC3339),
		})
	default: // CauseStale, CauseTest
		if err := s.store.HealRequeue(ctx, job.ID, healingFixInstruction(diagnosis.Cause, diagnosis.Files)); err != nil {
			log.Printf("healing: requeue job %q: %v", job.ID, err)
			s.spoolFailAlert(ctx, runID, &HealingAlert{
				Cause: diagnosis.Cause, Summary: diagnosis.Summary,
				Fix:      healingFixDescription(diagnosis.Cause, diagnosis.Files),
				Attempts: attempts, Budget: budget, NeedsHuman: true,
				Reason: "self-healing could not requeue: " + err.Error(),
			}, string(diagnosis.Cause))
			return
		}
		s.finishHealing(ctx, job.ID, job, diagnosis, budget, attempts, false, time.Time{})
		s.maybeHealingAlert(ctx, runID, &HealingAlert{
			Cause: diagnosis.Cause, Summary: diagnosis.Summary,
			Fix:      healingFixDescription(diagnosis.Cause, diagnosis.Files),
			Attempts: attempts, Budget: budget,
			Reason: "requeued for another attempt",
		})
	}
}

// healingBackoff returns the delay before the next infra retry: the base
// backoff doubling per attempt already used.
func (s *Server) healingBackoff(attempts int) time.Duration {
	base, err := s.healing.HealingBackoff()
	if err != nil {
		base = healInfraBackoff
	}
	for range attempts - 1 {
		base *= 2
		if base > 4*time.Hour {
			return 4 * time.Hour
		}
	}
	return base
}

// finishHealing records the diagnosis on the job and reports it to Plane.
// retryAfter is set only for infra causes waiting out their backoff.
func (s *Server) finishHealing(ctx context.Context, jobID string, job Job, diagnosis healDiagnosis, budget, attempts int, exhausted bool, retryAfter time.Time) {
	record := healingRecord{
		Cause:     diagnosis.Cause,
		Summary:   diagnosis.Summary,
		Fix:       healingFixDescription(diagnosis.Cause, diagnosis.Files),
		Attempt:   attempts,
		Budget:    budget,
		Exhausted: exhausted,
	}
	if !retryAfter.IsZero() {
		record.RetryAfter = retryAfter.Format(time.RFC3339)
	}
	if err := s.store.SaveHealingMetadata(ctx, jobID, record); err != nil {
		log.Printf("healing: record diagnosis on job %q: %v", jobID, err)
	}
	client := s.planeClientFor(job)
	if client == nil {
		return
	}
	key := ticketKey(job)
	if key == "" {
		return
	}
	var body strings.Builder
	fmt.Fprintf(&body, "<p><strong>Run failed — Machinist self-healing (%s)</strong></p>", jobID)
	fmt.Fprintf(&body, "<p>Cause: <code>%s</code>", diagnosis.Cause)
	if diagnosis.Summary != "" {
		fmt.Fprintf(&body, " — %s", diagnosis.Summary)
	}
	body.WriteString("</p><p>")
	if exhausted {
		fmt.Fprintf(&body, "Retry budget exhausted (%d of %d attempts). ", attempts, budget+1)
	}
	fmt.Fprintf(&body, "%s</p>", healingFixDescription(diagnosis.Cause, diagnosis.Files))
	// Exhausted budgets and human-required causes hand the ticket back.
	if exhausted || diagnosis.Cause == CauseSpec || diagnosis.Cause == CauseSecret {
		if err := client.SetState(ctx, key, PlaneNeedsAttentionState); err != nil {
			log.Printf("healing: set ticket %s to %s: %v", key, PlaneNeedsAttentionState, err)
		}
	}
	if len(diagnosis.Files) > 0 {
		fmt.Fprintf(&body, "<p>Offending files: %s</p>", strings.Join(diagnosis.Files, ", "))
	}
	if err := client.PostComment(ctx, key, body.String()); err != nil {
		log.Printf("healing: comment on ticket %s: %v", key, err)
	}
}

// healPendingRetries requeues infra-cause jobs whose backoff has elapsed.
// It runs on the scheduler loop; each requeue appends the diagnosis-aware
// prompt block and resets the job to queued.
func (s *Server) healPendingRetries(ctx context.Context) error {
	now := s.now().UTC()
	jobs, err := s.store.listJobs(ctx, "")
	if err != nil {
		return fmt.Errorf("list jobs for healing: %w", err)
	}
	for _, job := range jobs {
		if job.Metadata == nil || job.Metadata.Healing == nil || job.Metadata.Healing.RetryAfter == "" {
			continue
		}
		if job.State != "failed" {
			continue
		}
		due, err := time.Parse(time.RFC3339, job.Metadata.Healing.RetryAfter)
		if err != nil || due.After(now) {
			continue
		}
		if err := s.store.HealRequeue(ctx, job.ID, healingFixInstruction(job.Metadata.Healing.Cause, nil)); err != nil {
			log.Printf("healing: requeue job %q after backoff: %v", job.ID, err)
			continue
		}
		// The retry is now scheduled: clear the pending marker so the next
		// pass does not requeue twice.
		cleared := *job.Metadata.Healing
		cleared.RetryAfter = ""
		if err := s.store.SaveHealingMetadata(ctx, job.ID, cleared); err != nil {
			log.Printf("healing: clear retry marker on job %q: %v", job.ID, err)
		}
	}
	return nil
}

// HealRequeue appends the healing instruction to the job prompt and inserts a
// fresh queued run so a worker picks the retry up. The prior-attempt block is
// regenerated so the new run carries the failed attempt's transcript.
func (s *Store) HealRequeue(ctx context.Context, jobID, instruction string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var prompt, repository, commandName, state string
	err = tx.QueryRowContext(ctx, `SELECT prompt,repository,command,state FROM jobs WHERE id=?`, jobID).Scan(&prompt, &repository, &commandName, &state)
	if err != nil {
		return fmt.Errorf("read job %s: %w", jobID, err)
	}
	if state != "failed" {
		return fmt.Errorf("job %s is %s, not failed", jobID, state)
	}
	prompt += "\n\n" + instruction
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET prompt=?,state='queued',updated_at=? WHERE id=?`, prompt, s.now().UTC().Format(time.RFC3339Nano), jobID); err != nil {
		return fmt.Errorf("requeue job %s: %w", jobID, err)
	}
	runID, err := randomID("run", 12)
	if err != nil {
		return err
	}
	var timeoutMillis int64
	var commandHash, executor, model string
	if err := tx.QueryRowContext(ctx, `SELECT command_hash,COALESCE(executor,''),COALESCE(model,''),timeout_ms FROM runs WHERE job_id=? ORDER BY rowid DESC LIMIT 1`, jobID).Scan(&commandHash, &executor, &model, &timeoutMillis); err != nil {
		return fmt.Errorf("read prior run of job %s: %w", jobID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id,job_id,command,command_hash,executor,model,repository,rendered_prompt,timeout_ms,state) VALUES(?,?,?,?,?,?,?,?,?,'queued')`,
		runID, jobID, commandName, commandHash, executor, model, repository, prompt, timeoutMillis); err != nil {
		return fmt.Errorf("insert healing run for job %s: %w", jobID, err)
	}
	return tx.Commit()
}

// SaveHealingMetadata merges a healing record into the job metadata shown on
// the run page.
func (s *Store) SaveHealingMetadata(ctx context.Context, jobID string, record healingRecord) error {
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
	existing[healingMetadataKey] = json.RawMessage(encoded)
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
