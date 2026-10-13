package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/artifacts"
	"github.com/owainlewis/machinist/internal/backlog"
	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/protocol"
	_ "modernc.org/sqlite"
)

var (
	ErrLeaseConflict                   = errors.New("run lease does not match")
	ErrRunState                        = errors.New("run is not active")
	ErrInvalidCompletion               = errors.New("invalid run completion")
	ErrJobActive                       = errors.New("active job cannot be deleted")
	ErrTriggerMissing                  = errors.New("trigger state does not exist")
	ErrTriggerStale                    = errors.New("trigger state configuration changed")
	ErrTriggerPreviousGenerationActive = errors.New("previous trigger configuration still has active work")
)

const leaseDuration = 30 * time.Second
const maxTriggerErrorLength = 2000

// reclaimExpiredLeasesSQL returns running runs whose lease lapsed to the queue.
const reclaimExpiredLeasesSQL = `UPDATE runs SET state='queued',worker_instance=NULL,worker_name='',lease_token=NULL,lease_expires_at=NULL,started_at=NULL WHERE state='running' AND (lease_expires_at IS NULL OR lease_expires_at<=?)`

type Store struct {
	artifactStore artifacts.Store
	storageConfig config.ResolvedStorage
	db            *sql.DB
	now           func() time.Time
}

type Job struct {
	Task             *protocol.Task    `json:"task,omitempty"`
	Workflow         *WorkflowProgress `json:"workflow,omitempty"`
	Metadata         *JobMetadata      `json:"metadata,omitempty"`
	ID               string            `json:"id"`
	Title            string            `json:"title,omitempty"`
	Prompt           string            `json:"prompt"`
	Repository       string            `json:"repository"`
	GitHubIssueTitle string            `json:"github_issue_title,omitempty"`
	Command          string            `json:"command"`
	TriggerID        string            `json:"trigger_id,omitempty"`
	OccurrenceKey    string            `json:"occurrence_key,omitempty"`
	TriggerSubject   string            `json:"trigger_subject,omitempty"`
	State            string            `json:"state"`
	Attempt          int               `json:"attempt"`
	PreviousJobID    string            `json:"previous_job_id,omitempty"`
	SupersededBy     string            `json:"superseded_by,omitempty"`
	IssueKey         string            `json:"issue_key,omitempty"`
	SourceKind       string            `json:"source_kind,omitempty"`
	BacklogIssueID   string            `json:"backlog_issue_id,omitempty"`
	BacklogIssueKey  string            `json:"backlog_issue_key,omitempty"`
	ResolutionKind   string            `json:"resolution_kind,omitempty"`
	ResolutionDetail string            `json:"resolution_detail,omitempty"`
	ResolvedAt       *time.Time        `json:"resolved_at,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	UpdatedAt        time.Time         `json:"updated_at"`
	StateHistory     []StateTransition `json:"state_history,omitempty"`
	Runs             []Run             `json:"runs"`
}

// StateTransition is one recorded job state change with the time it happened.
type StateTransition struct {
	State     string    `json:"state"`
	ChangedAt time.Time `json:"changed_at"`
}

type Run struct {
	Revision       *protocol.Revision `json:"revision,omitempty"`
	ReviewedRunID  string             `json:"reviewed_run_id,omitempty"`
	Step           *int               `json:"step,omitempty"`
	Outcome        string             `json:"outcome,omitempty"`
	Summary        string             `json:"summary,omitempty"`
	ID             string             `json:"id"`
	Command        string             `json:"command"`
	Executor       string             `json:"executor"`
	Model          string             `json:"model,omitempty"`
	State          string             `json:"state"`
	WorkerName     string             `json:"worker_name,omitempty"`
	ExitCode       *int               `json:"exit_code,omitempty"`
	Error          string             `json:"error,omitempty"`
	StartedAt      time.Time          `json:"started_at,omitempty"`
	CompletedAt    time.Time          `json:"completed_at,omitempty"`
	DurationMillis *int64             `json:"duration_millis,omitempty"`
	TokenUsage     *int64             `json:"token_usage,omitempty,string"`
}

type Worker struct {
	InstanceID   string    `json:"instance_id"`
	Name         string    `json:"name"`
	LastSeenAt   time.Time `json:"last_seen_at"`
	Repositories []string  `json:"repositories"`
	Connected    bool      `json:"connected"`
}

type Snapshot struct {
	Jobs     []Job           `json:"jobs"`
	Workers  []Worker        `json:"workers"`
	Triggers []TriggerStatus `json:"triggers"`
}

// TriggerDefinition is the durable identity and schedule state for one resolved trigger.
// A changed signature resets scheduling from NextDueAt; an unchanged signature preserves
// its existing due time across restarts.
type TriggerDefinition struct {
	Identity        string
	Family          string
	ConfigSignature string
	NextDueAt       time.Time
}

// TriggerAdmission contains an already resolved job. Trigger scheduling code remains
// responsible for validating configuration and rendering the prompt before admission.
type TriggerAdmission struct {
	Identity          string
	Family            string
	ConfigSignature   string
	ConfigGeneration  string
	OccurrenceKey     string
	Subject           string
	Title             string
	ScheduledAt       time.Time
	NextDueAt         time.Time
	Prompt            string
	Repository        string
	SelectionName     string
	Command           config.ResolvedCommand
	Task              *protocol.Task
	GitHubRepository  string
	GitHubIssueNumber int
	GitHubIssueTitle  string
	RequestActor      string
	RequestLabel      string
	IssueKey          string
	// SourceKind records the provider the job came from ('plane' or
	// 'linear'); empty means the job has no tracker provider.
	SourceKind string
}

type TriggerStatus struct {
	Identity            string     `json:"identity"`
	Family              string     `json:"family"`
	ConfigSignature     string     `json:"config_signature,omitempty"`
	ConfigGeneration    string     `json:"-"`
	NextDueAt           *time.Time `json:"next_due,omitempty"`
	PendingOccurrenceAt *time.Time `json:"-"`
	LastAttemptAt       *time.Time `json:"last_attempt,omitempty"`
	LastSuccessAt       *time.Time `json:"last_success,omitempty"`
	ActiveJobID         string     `json:"active_job,omitempty"`
	CandidateCount      int64      `json:"candidate_count,omitempty"`
	AdmissionCount      int64      `json:"admission_count,omitempty"`
	CoalescedCount      int64      `json:"coalesced_count,omitempty"`
	Health              string     `json:"health"`
	LatestError         string     `json:"error,omitempty"`
}

type GitHubTriggerRequest struct {
	TriggerIdentity  string
	OccurrenceKey    string
	ConfigGeneration string
	Repository       string
	IssueNumber      int
	Subject          string
	Actor            string
	RequestLabel     string
	RequestedAt      time.Time
	State            string
	JobID            string
}

type RunOutput struct {
	Result string
	Events string
}

type CreateJobOptions struct {
	Title    string
	Project  string
	IssueKey string
}

func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, now: time.Now}
	if err := store.initialize(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	resolved, e := (config.Config{}).ResolveStorage(filepath.Join(filepath.Dir(path), "artifacts"))
	if e != nil {
		db.Close()
		return nil, e
	}
	if e = store.configureStorage(resolved); e != nil {
		db.Close()
		return nil, e
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) initialize(ctx context.Context) error {
	const schemaVersion = 9
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read database schema version: %w", err)
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, schemaVersion)
	}
	if version < 1 {
		if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys=OFF;
DROP TABLE IF EXISTS github_trigger_requests; DROP TABLE IF EXISTS trigger_state; DROP TABLE IF EXISTS schedule_state;
DROP TABLE IF EXISTS known_repositories; DROP TABLE IF EXISTS worker_repositories; DROP TABLE IF EXISTS workers;
DROP TABLE IF EXISTS runs; DROP TABLE IF EXISTS jobs; PRAGMA foreign_keys=ON;`); err != nil {
			return fmt.Errorf("replace legacy database schema: %w", err)
		}
	}
	if version == 1 {
		if err := s.upgradeToVersionTwo(ctx); err != nil {
			return fmt.Errorf("upgrade database schema to version 2: %w", err)
		}
	}
	const schema = `
PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS jobs (
 id TEXT PRIMARY KEY, prompt TEXT NOT NULL, repository TEXT NOT NULL, command TEXT NOT NULL,
 title TEXT NOT NULL DEFAULT '',
 trigger_identity TEXT NOT NULL DEFAULT '', trigger_config_signature TEXT NOT NULL DEFAULT '',
 trigger_generation_id TEXT NOT NULL DEFAULT '', occurrence_key TEXT NOT NULL DEFAULT '',
 trigger_subject TEXT NOT NULL DEFAULT '', github_issue_title TEXT NOT NULL DEFAULT '',
 issue_key TEXT NOT NULL DEFAULT '', resolution_kind TEXT NOT NULL DEFAULT '', resolution_detail TEXT NOT NULL DEFAULT '',
 resolved_at TEXT,
 fixed_trigger INTEGER NOT NULL DEFAULT 0, state TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
 previous_job_id TEXT NOT NULL DEFAULT '', attempt INTEGER NOT NULL DEFAULT 1, superseded_by TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS runs (
 id TEXT PRIMARY KEY, job_id TEXT NOT NULL UNIQUE REFERENCES jobs(id), command TEXT NOT NULL, command_hash TEXT NOT NULL,
 executor TEXT NOT NULL, model TEXT NOT NULL DEFAULT '', repository TEXT NOT NULL, rendered_prompt TEXT NOT NULL,
 timeout_ms INTEGER NOT NULL, state TEXT NOT NULL, worker_instance TEXT, worker_name TEXT NOT NULL DEFAULT '',
 lease_token TEXT, lease_expires_at INTEGER, exit_code INTEGER, error TEXT, result TEXT, events TEXT,
 started_at TEXT, completed_at TEXT, duration_millis INTEGER, token_usage INTEGER);
CREATE INDEX IF NOT EXISTS runs_dispatch ON runs(state, job_id);
CREATE TABLE IF NOT EXISTS workers (instance_id TEXT PRIMARY KEY, name TEXT NOT NULL, last_seen_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS worker_repositories (worker_instance TEXT NOT NULL REFERENCES workers(instance_id) ON DELETE CASCADE, repository TEXT NOT NULL, PRIMARY KEY(worker_instance,repository));
CREATE TABLE IF NOT EXISTS known_repositories (repository TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS trigger_state (identity TEXT PRIMARY KEY,family TEXT NOT NULL,config_signature TEXT NOT NULL,generation_id TEXT NOT NULL,next_due_at TEXT,pending_occurrence_at TEXT,last_attempt_at TEXT,last_success_at TEXT,last_job_state TEXT NOT NULL DEFAULT '',last_job_error TEXT NOT NULL DEFAULT '',health TEXT NOT NULL DEFAULT 'healthy',latest_error TEXT NOT NULL DEFAULT '',candidate_count INTEGER NOT NULL DEFAULT 0,admission_count INTEGER NOT NULL DEFAULT 0,coalesced_count INTEGER NOT NULL DEFAULT 0,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS github_trigger_requests (trigger_identity TEXT NOT NULL,occurrence_key TEXT NOT NULL,config_generation TEXT NOT NULL,repository TEXT NOT NULL,issue_number INTEGER NOT NULL,subject TEXT NOT NULL,actor TEXT NOT NULL,request_label TEXT NOT NULL,requested_at TEXT NOT NULL,state TEXT NOT NULL CHECK(state IN ('pending','admitted','rejected')),job_id TEXT,needs_reconciliation INTEGER NOT NULL DEFAULT 1,updated_at TEXT NOT NULL,PRIMARY KEY(trigger_identity,occurrence_key));
CREATE UNIQUE INDEX IF NOT EXISTS jobs_trigger_occurrence ON jobs(trigger_identity,occurrence_key) WHERE trigger_identity<>'' AND occurrence_key<>'';
CREATE UNIQUE INDEX IF NOT EXISTS jobs_active_fixed_trigger ON jobs(trigger_identity) WHERE fixed_trigger=1 AND state IN ('queued','running');
CREATE UNIQUE INDEX IF NOT EXISTS jobs_active_trigger_subject ON jobs(trigger_subject) WHERE trigger_subject<>'' AND state IN ('queued','running');
CREATE INDEX IF NOT EXISTS github_trigger_requests_reconciliation ON github_trigger_requests(trigger_identity,needs_reconciliation,requested_at);
CREATE TABLE IF NOT EXISTS backlog_issues (
 id TEXT PRIMARY KEY, project TEXT NOT NULL, key TEXT NOT NULL, title TEXT NOT NULL,
 spec TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'backlog', revision INTEGER NOT NULL DEFAULT 1,
 url TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS idx_backlog_issues_project_key ON backlog_issues(project, key);
CREATE INDEX IF NOT EXISTS idx_backlog_issues_project_state ON backlog_issues(project, state);
CREATE TABLE IF NOT EXISTS job_state_history (
 job_id TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
 state TEXT NOT NULL, changed_at TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS job_state_history_job ON job_state_history(job_id);
CREATE TABLE IF NOT EXISTS review_instructions (
 repository TEXT PRIMARY KEY,
 instructions TEXT NOT NULL,
 updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS review_profiles (
 name TEXT PRIMARY KEY,
 body TEXT NOT NULL,
 updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS project_review_settings (
 repository TEXT PRIMARY KEY,
 profile_name TEXT NOT NULL DEFAULT '',
 profile_body TEXT NOT NULL DEFAULT '',
 extra_instructions TEXT NOT NULL DEFAULT '',
 updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS settings (
 key TEXT PRIMARY KEY,
 value TEXT NOT NULL);
CREATE TRIGGER IF NOT EXISTS job_state_history_insert AFTER INSERT ON jobs
BEGIN
 INSERT INTO job_state_history(job_id,state,changed_at) VALUES (new.id,new.state,new.created_at);
END;
CREATE TRIGGER IF NOT EXISTS job_state_history_update AFTER UPDATE OF state ON jobs WHEN old.state<>new.state
BEGIN
 INSERT INTO job_state_history(job_id,state,changed_at) VALUES (new.id,new.state,new.updated_at);
END;`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}
	if err := s.migrateReviewInstructions(ctx); err != nil {
		return fmt.Errorf("migrate review instructions: %w", err)
	}
	if version < 3 {
		if err := s.upgradeWorkflows(ctx); err != nil {
			return fmt.Errorf("upgrade workflow schema: %w", err)
		}
	}
	if version < 6 {
		if err := s.addJobTitle(ctx); err != nil {
			return fmt.Errorf("upgrade database schema to version 6: %w", err)
		}
	}
	if version < 7 {
		if err := s.addJobAttemptColumns(ctx); err != nil {
			return fmt.Errorf("upgrade database schema to version 7: %w", err)
		}
	}
	if version < 8 {
		if err := s.addJobResolution(ctx); err != nil {
			return fmt.Errorf("upgrade database schema to version 8: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, workflowSchema+artifactSchema+reviewSchema+jobMetadataSchema); err != nil {
		return fmt.Errorf("initialize database: %w", err)
	}
	// job_metadata must exist (and carry its current columns on fresh
	// databases) before the idempotent ALTER runs against older databases.
	if err := s.addJobSourceColumns(ctx); err != nil {
		return fmt.Errorf("upgrade database schema to version 9: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `PRAGMA user_version=9;`)
	return err
}

// addJobSourceColumns records where a job came from: the trigger provider kind
// ('plane', 'linear', or 'backlog') and, for native issues, the issue a started
// run belongs to. Existing rows keep empty fields, so legacy jobs stay
// reconcilable through the workspace fallback.
func (s *Store) addJobSourceColumns(ctx context.Context) error {
	for _, column := range []string{
		"source_kind TEXT NOT NULL DEFAULT ''",
		"backlog_issue_id TEXT",
	} {
		name, _, _ := strings.Cut(column, " ")
		var present int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('job_metadata') WHERE name=?`, name).Scan(&present); err != nil {
			return err
		}
		if present > 0 {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE job_metadata ADD COLUMN `+column); err != nil {
			return err
		}
	}
	return nil
}

// addJobResolution adds the ticket-resolution columns to jobs. Existing rows
// keep empty resolution fields, so the reconciler backfills them.
func (s *Store) addJobResolution(ctx context.Context) error {
	for _, column := range []string{
		"issue_key TEXT NOT NULL DEFAULT ''",
		"resolution_kind TEXT NOT NULL DEFAULT ''",
		"resolution_detail TEXT NOT NULL DEFAULT ''",
		"resolved_at TEXT",
	} {
		name, _, _ := strings.Cut(column, " ")
		var present int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name=?`, name).Scan(&present); err != nil {
			return err
		}
		if present > 0 {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE jobs ADD COLUMN `+column); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) addJobTitle(ctx context.Context) error {
	var columns int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name='title'`).Scan(&columns); err != nil {
		return err
	}
	if columns > 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `ALTER TABLE jobs ADD COLUMN title TEXT NOT NULL DEFAULT ''`)
	return err
}

// addJobAttemptColumns links requeued runs to the attempt they replace: the issue a
// job came from, the prior job it follows, and the newer job that superseded it.
func (s *Store) addJobAttemptColumns(ctx context.Context) error {
	for _, column := range []struct{ name, definition string }{
		{"previous_job_id", "TEXT NOT NULL DEFAULT ''"},
		{"attempt", "INTEGER NOT NULL DEFAULT 1"},
		{"superseded_by", "TEXT NOT NULL DEFAULT ''"},
	} {
		var present int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name=?`, column.name).Scan(&present); err != nil {
			return err
		}
		if present > 0 {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `ALTER TABLE jobs ADD COLUMN `+column.name+` `+column.definition); err != nil {
			return err
		}
	}
	return nil
}

// upgradeToVersionTwo removes the Shepherd schedule bookkeeping. Finished jobs
// keep their rows. Queued schedule jobs are failed so they cannot overlap a
// replacement trigger, and a running one blocks the upgrade until it finishes.
// The steps run in one transaction and skip columns that are already gone, so
// an interrupted upgrade completes on the next start.
func (s *Store) upgradeToVersionTwo(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS jobs_active_shepherd_repository; DROP TABLE IF EXISTS schedule_state;`); err != nil {
		return err
	}
	for _, column := range []string{"schedule_name", "has_shepherd"} {
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('jobs') WHERE name=?`, column).Scan(&present); err != nil {
			return err
		}
		if present == 0 {
			continue
		}
		if column == "schedule_name" {
			if err := s.drainScheduledJobs(ctx, tx); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE jobs DROP COLUMN `+column); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version=2`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) drainScheduledJobs(ctx context.Context, tx *sql.Tx) error {
	var running int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE schedule_name<>'' AND state='running'`).Scan(&running); err != nil {
		return err
	}
	if running > 0 {
		return fmt.Errorf("%d scheduled Shepherd jobs are still running; let them finish and start again", running)
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	const reason = "cancelled by schema upgrade: shepherd schedules were removed"
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET state='failed',exit_code=1,error=?,completed_at=? WHERE state='queued' AND job_id IN (SELECT id FROM jobs WHERE schedule_name<>'' AND state='queued')`, reason, now); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET state='failed',updated_at=? WHERE schedule_name<>'' AND state='queued'`, now)
	return err
}

func (s *Store) CreateJob(ctx context.Context, prompt, repository, name string, command config.ResolvedCommand) (string, error) {
	return s.CreateJobWithOptions(ctx, prompt, repository, name, command, CreateJobOptions{})
}

func (s *Store) CreateJobWithOptions(ctx context.Context, prompt, repository, name string, command config.ResolvedCommand, options CreateJobOptions) (string, error) {
	if command.Name == "" {
		return "", errors.New("job must contain one command")
	}
	jobID, err := randomID("job", 12)
	if err != nil {
		return "", err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,title,prompt,repository,command,issue_key,state,created_at,updated_at) VALUES(?,?,?,?,?,?,'queued',?,?)`, jobID, options.Title, prompt, repository, name, options.IssueKey, now, now); err != nil {
		return "", fmt.Errorf("insert job: %w", err)
	}
	if options.Project != "" {
		if err := saveJobMetadata(ctx, tx, jobID, map[string]any{"project": options.Project}); err != nil {
			return "", fmt.Errorf("insert job metadata: %w", err)
		}
	}
	runID, err := randomID("run", 12)
	if err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id,job_id,command,command_hash,executor,model,repository,rendered_prompt,timeout_ms,state) VALUES(?,?,?,?,?,?,?,?,?,'queued')`, runID, jobID, command.Name, command.Hash, command.Executor, command.Model, repository, command.Prompt, command.Timeout.Milliseconds()); err != nil {
		return "", fmt.Errorf("insert run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit job: %w", err)
	}
	return jobID, nil
}

// linkPriorAttempt chains a newly created job to the most recent finished job for the
// same source issue: the new job records the prior job and its attempt number, and the
// prior job records the job that superseded it. Issues are matched by trigger subject
// or task source URL within one repository. Returns the prior job ID, empty when the
// job starts a new attempt chain.
func (s *Store) linkPriorAttempt(ctx context.Context, tx *sql.Tx, jobID, subject, repository string) (string, error) {
	if subject == "" {
		return "", nil
	}
	var previousID string
	var previousAttempt int
	err := tx.QueryRowContext(ctx, `SELECT id,attempt FROM jobs
WHERE repository=? AND id<>? AND state IN ('succeeded','failed','cancelled')
AND (trigger_subject=? OR id IN (SELECT job_id FROM task_inputs WHERE json_valid(task) AND json_extract(task,'$.source_url')=?))
ORDER BY created_at DESC,id DESC LIMIT 1`, repository, jobID, subject, subject).Scan(&previousID, &previousAttempt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find prior job for issue %q: %w", subject, err)
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET previous_job_id=?,attempt=?,updated_at=? WHERE id=?`, previousID, previousAttempt+1, now, jobID); err != nil {
		return "", fmt.Errorf("link job to prior attempt: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET superseded_by=?,updated_at=? WHERE id=?`, jobID, now, previousID); err != nil {
		return "", fmt.Errorf("mark prior job superseded: %w", err)
	}
	return previousID, nil
}

// SyncTriggers makes the durable trigger set match the resolved configuration. Existing
// state is preserved only when both family and configuration signature are unchanged.
func (s *Store) SyncTriggers(ctx context.Context, definitions []TriggerDefinition) error {
	seen := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		if definition.Identity == "" || definition.Family == "" || definition.ConfigSignature == "" {
			return errors.New("trigger identity, family, and configuration signature are required")
		}
		if seen[definition.Identity] {
			return fmt.Errorf("duplicate trigger identity %q", definition.Identity)
		}
		seen[definition.Identity] = true
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC().Format(time.RFC3339Nano)
	placeholders := make([]string, 0, len(definitions))
	identities := make([]any, 0, len(definitions))
	for _, definition := range definitions {
		placeholders = append(placeholders, "?")
		identities = append(identities, definition.Identity)
		generationID, err := randomID("trigger", 12)
		if err != nil {
			return fmt.Errorf("generate trigger %q configuration generation: %w", definition.Identity, err)
		}
		// A changed family or signature discards every piece of durable state, so the
		// row is recreated from scratch. An unchanged trigger keeps its schedule and
		// generation, gaining a generation only if it never had one.
		if _, err := tx.ExecContext(ctx, `DELETE FROM trigger_state WHERE identity=? AND (family<>? OR config_signature<>?)`, definition.Identity, definition.Family, definition.ConfigSignature); err != nil {
			return fmt.Errorf("sync trigger %q: %w", definition.Identity, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO trigger_state(identity,family,config_signature,generation_id,next_due_at,health,updated_at)
VALUES(?,?,?,?,?,'healthy',?)
ON CONFLICT(identity) DO UPDATE SET
  generation_id=CASE WHEN trigger_state.generation_id='' THEN excluded.generation_id ELSE trigger_state.generation_id END,
  updated_at=excluded.updated_at`, definition.Identity, definition.Family, definition.ConfigSignature, generationID, nullableTimeText(definition.NextDueAt), now); err != nil {
			return fmt.Errorf("sync trigger %q: %w", definition.Identity, err)
		}
	}
	removeStale := `DELETE FROM trigger_state`
	if len(identities) > 0 {
		removeStale += ` WHERE identity NOT IN (` + strings.Join(placeholders, ",") + `)`
	}
	if _, err := tx.ExecContext(ctx, removeStale, identities...); err != nil {
		return fmt.Errorf("remove stale triggers: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit trigger sync: %w", err)
	}
	return nil
}

// CreateTriggeredJob durably admits an occurrence and its job in one transaction.
// Duplicate occurrences are idempotent. Fixed schedules coalesce a new occurrence while
// active work exists; subject-based triggers wait so a later retry can admit the event.
func (s *Store) CreateTriggeredJob(ctx context.Context, admission TriggerAdmission) (string, bool, error) {
	if admission.Identity == "" || admission.Family == "" {
		return "", false, errors.New("trigger identity and family are required")
	}
	if admission.ConfigSignature == "" {
		return "", false, errors.New("trigger config signature is required")
	}
	if admission.ConfigGeneration == "" {
		return "", false, errors.New("trigger config generation is required")
	}
	if admission.Command.Name == "" {
		return "", false, errors.New("triggered job must contain one command")
	}
	fixed := fixedTriggerFamily(admission.Family)
	if admission.OccurrenceKey == "" && fixed && !admission.ScheduledAt.IsZero() {
		admission.OccurrenceKey = admission.ScheduledAt.UTC().Format(time.RFC3339Nano)
	}
	if admission.OccurrenceKey == "" {
		return "", false, errors.New("trigger occurrence key is required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var family, configSignature, configGeneration string
	if err := tx.QueryRowContext(ctx, `SELECT family,config_signature,generation_id FROM trigger_state WHERE identity=?`, admission.Identity).Scan(&family, &configSignature, &configGeneration); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, fmt.Errorf("%w: %s", ErrTriggerMissing, admission.Identity)
		}
		return "", false, fmt.Errorf("read trigger %q: %w", admission.Identity, err)
	}
	if family != admission.Family {
		return "", false, fmt.Errorf("trigger %q family is %q, not %q", admission.Identity, family, admission.Family)
	}
	if configSignature != admission.ConfigSignature {
		return "", false, fmt.Errorf("%w: trigger %q configuration signature changed before admission", ErrTriggerStale, admission.Identity)
	}
	if configGeneration != admission.ConfigGeneration {
		return "", false, fmt.Errorf("%w: %s", ErrTriggerStale, admission.Identity)
	}
	if admission.Family == "github" {
		if admission.GitHubRepository == "" || admission.GitHubIssueNumber <= 0 || admission.Subject == "" || admission.RequestActor == "" || admission.RequestLabel == "" || admission.ScheduledAt.IsZero() {
			return "", false, errors.New("github trigger request metadata is required")
		}
		now := s.now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `INSERT INTO github_trigger_requests(trigger_identity,occurrence_key,config_generation,repository,issue_number,subject,actor,request_label,requested_at,state,needs_reconciliation,updated_at)
VALUES(?,?,?,?,?,?,?,?,?, 'pending',1,?)
ON CONFLICT(trigger_identity,occurrence_key) DO UPDATE SET
  config_generation=excluded.config_generation,
  repository=excluded.repository,
  issue_number=excluded.issue_number,
  subject=excluded.subject,
  actor=excluded.actor,
  request_label=excluded.request_label,
  requested_at=excluded.requested_at,
  needs_reconciliation=1,
  updated_at=excluded.updated_at
WHERE github_trigger_requests.state='pending'`, admission.Identity, admission.OccurrenceKey, admission.ConfigGeneration, admission.GitHubRepository, admission.GitHubIssueNumber, admission.Subject, admission.RequestActor, admission.RequestLabel, admission.ScheduledAt.UTC().Format(time.RFC3339Nano), now); err != nil {
			return "", false, fmt.Errorf("persist github trigger request: %w", err)
		}
	}

	var existingJob, existingState string
	err = tx.QueryRowContext(ctx, `SELECT id,state FROM jobs WHERE trigger_identity=? AND occurrence_key=? ORDER BY rowid DESC LIMIT 1`, admission.Identity, admission.OccurrenceKey).Scan(&existingJob, &existingState)
	// Issue-family admissions treat a terminal job as spent: a later edit or
	// deliberate re-label admits fresh work, so only active jobs dedupe here.
	// Other families keep one job per occurrence key forever.
	duplicate := err == nil && (!terminalJobState(existingState) || admission.Family != "issue")
	if duplicate {
		if admission.Family == "github" {
			if admission.GitHubIssueTitle != "" {
				if _, updateErr := tx.ExecContext(ctx, `UPDATE jobs SET github_issue_title=? WHERE id=? AND github_issue_title=''`, admission.GitHubIssueTitle, existingJob); updateErr != nil {
					return "", false, fmt.Errorf("update existing github job title: %w", updateErr)
				}
			}
			if _, updateErr := tx.ExecContext(ctx, `UPDATE github_trigger_requests SET state='admitted',job_id=?,config_generation=?,needs_reconciliation=1,updated_at=? WHERE trigger_identity=? AND occurrence_key=?`, existingJob, admission.ConfigGeneration, s.now().UTC().Format(time.RFC3339Nano), admission.Identity, admission.OccurrenceKey); updateErr != nil {
				return "", false, fmt.Errorf("repair github trigger request: %w", updateErr)
			}
		}
		if fixed {
			if _, updateErr := tx.ExecContext(ctx, `UPDATE trigger_state SET pending_occurrence_at=NULL,next_due_at=COALESCE(?,next_due_at),updated_at=? WHERE identity=?`, nullableTimeText(admission.NextDueAt), s.now().UTC().Format(time.RFC3339Nano), admission.Identity); updateErr != nil {
				return "", false, fmt.Errorf("finish duplicate trigger occurrence: %w", updateErr)
			}
		}
		return existingJob, false, tx.Commit()
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// No prior occurrence; admit below.
	case err == nil:
		// Terminal issue-family job; admit fresh work below.
	default:
		return "", false, fmt.Errorf("read trigger occurrence: %w", err)
	}
	// jobs_trigger_occurrence admits one job per (identity, occurrence key), so a
	// spent issue occurrence gets a fresh key before its successor is inserted.
	if err == nil && admission.Family == "issue" {
		admission.OccurrenceKey = fmt.Sprintf("%s#%d", admission.OccurrenceKey, admission.ScheduledAt.UnixNano())
	}

	if fixed {
		var activeGeneration string
		err = tx.QueryRowContext(ctx, `SELECT id,trigger_generation_id FROM jobs WHERE trigger_identity=? AND fixed_trigger=1 AND state IN ('queued','running')`, admission.Identity).Scan(&existingJob, &activeGeneration)
		if err == nil {
			if activeGeneration != admission.ConfigGeneration {
				return existingJob, false, fmt.Errorf("%w: %s", ErrTriggerPreviousGenerationActive, admission.Identity)
			}
			now := s.now().UTC().Format(time.RFC3339Nano)
			if _, err := tx.ExecContext(ctx, `UPDATE trigger_state SET next_due_at=COALESCE(?,next_due_at),pending_occurrence_at=NULL,last_attempt_at=?,health='coalesced',latest_error='',coalesced_count=coalesced_count+1,updated_at=? WHERE identity=?`, nullableTimeText(admission.NextDueAt), now, now, admission.Identity); err != nil {
				return "", false, fmt.Errorf("coalesce trigger %q: %w", admission.Identity, err)
			}
			return existingJob, false, tx.Commit()
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", false, fmt.Errorf("read active trigger job: %w", err)
		}
	}
	if admission.Subject != "" {
		err = tx.QueryRowContext(ctx, `SELECT id FROM jobs WHERE trigger_subject=? AND state IN ('queued','running')`, admission.Subject).Scan(&existingJob)
		if err == nil {
			if admission.Family == "github" && admission.GitHubIssueTitle != "" {
				if _, updateErr := tx.ExecContext(ctx, `UPDATE jobs SET github_issue_title=? WHERE id=? AND github_issue_title=''`, admission.GitHubIssueTitle, existingJob); updateErr != nil {
					return "", false, fmt.Errorf("update existing github job title: %w", updateErr)
				}
			}
			return existingJob, false, tx.Commit()
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", false, fmt.Errorf("read active trigger subject: %w", err)
		}
	}

	jobID, err := randomID("job", 12)
	if err != nil {
		return "", false, err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(id,prompt,repository,command,trigger_identity,trigger_config_signature,trigger_generation_id,occurrence_key,trigger_subject,github_issue_title,issue_key,fixed_trigger,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,'queued',?,?)`, jobID, admission.Prompt, admission.Repository, admission.SelectionName, admission.Identity, admission.ConfigSignature, admission.ConfigGeneration, admission.OccurrenceKey, admission.Subject, admission.GitHubIssueTitle, admission.IssueKey, fixed, now, now); err != nil {
		return "", false, fmt.Errorf("insert triggered job: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET title=? WHERE id=?`, admission.Title, jobID); err != nil {
		return "", false, fmt.Errorf("insert triggered job title: %w", err)
	}
	if admission.SourceKind != "" {
		if err := stampJobSource(ctx, tx, jobID, admission.SourceKind, ""); err != nil {
			return "", false, err
		}
	}
	previous, err := s.linkPriorAttempt(ctx, tx, jobID, admission.Subject, admission.Repository)
	if err != nil {
		return "", false, err
	}
	command := admission.Command
	// A requeue carries the prior attempt's transcript so the coder can diagnose
	// the failure from the prompt alone.
	if previous != "" {
		block, blockErr := priorAttemptContext(ctx, tx, previous)
		if blockErr != nil {
			return "", false, blockErr
		}
		admission.Prompt, _ = appendPriorContext(admission.Prompt, "", block)
		command.Prompt += "\n\n" + block
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET prompt=? WHERE id=?`, admission.Prompt, jobID); err != nil {
			return "", false, fmt.Errorf("add prior attempt context: %w", err)
		}
	}
	if admission.Task != nil {
		task := *admission.Task
		raw, err := json.Marshal(task)
		if err != nil {
			return "", false, fmt.Errorf("encode triggered task: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_inputs(job_id,task) VALUES(?,?)`, jobID, string(raw)); err != nil {
			return "", false, fmt.Errorf("insert triggered task: %w", err)
		}
	}
	if admission.Family == "github" {
		if _, err := tx.ExecContext(ctx, `UPDATE github_trigger_requests SET state='admitted',job_id=?,needs_reconciliation=1,updated_at=? WHERE trigger_identity=? AND occurrence_key=?`, jobID, now, admission.Identity, admission.OccurrenceKey); err != nil {
			return "", false, fmt.Errorf("admit github trigger request: %w", err)
		}
	}
	runID, err := randomID("run", 12)
	if err != nil {
		return "", false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id,job_id,command,command_hash,executor,model,repository,rendered_prompt,timeout_ms,state) VALUES(?,?,?,?,?,?,?,?,?,'queued')`, runID, jobID, command.Name, command.Hash, command.Executor, command.Model, admission.Repository, command.Prompt, command.Timeout.Milliseconds()); err != nil {
		return "", false, fmt.Errorf("insert triggered run: %w", err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE trigger_state SET next_due_at=COALESCE(?,next_due_at),pending_occurrence_at=NULL,last_attempt_at=?,health='active',latest_error='',admission_count=admission_count+1,updated_at=? WHERE identity=?`, nullableTimeText(admission.NextDueAt), now, now, admission.Identity)
	if err != nil {
		return "", false, fmt.Errorf("update trigger admission: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return "", false, err
	}
	if changed != 1 {
		return "", false, fmt.Errorf("%w: %s", ErrTriggerMissing, admission.Identity)
	}
	if err := tx.Commit(); err != nil {
		return "", false, fmt.Errorf("commit triggered job: %w", err)
	}
	return jobID, true, nil
}

// ActiveTriggerJob returns the newest queued or running job admitted for any of
// the given trigger subjects (an issue URL or ticket key). found is false when
// no active job exists.
func (s *Store) ActiveTriggerJob(ctx context.Context, subjects ...string) (jobID, state string, found bool, err error) {
	cleaned := make([]string, 0, len(subjects))
	for _, subject := range subjects {
		if subject = strings.TrimSpace(subject); subject != "" {
			cleaned = append(cleaned, subject)
		}
	}
	if len(cleaned) == 0 {
		return "", "", false, errors.New("trigger subject is required")
	}
	placeholders := strings.Repeat("?,", len(cleaned))
	args := make([]any, 0, len(cleaned)+2)
	for _, subject := range cleaned {
		args = append(args, subject)
	}
	query := `SELECT id,state FROM jobs WHERE trigger_subject IN (` + placeholders[:len(placeholders)-1] + `) AND state IN ('queued','running') ORDER BY rowid DESC LIMIT 1`
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&jobID, &state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("read active trigger job: %w", err)
	}
	return jobID, state, true, nil
}

// UpdateQueuedTriggerJob rewrites the spec of a queued job in place with the
// edited ticket content: prompt, title, task input, and the queued run's
// rendered command. The job must still be queued.
func (s *Store) UpdateQueuedTriggerJob(ctx context.Context, jobID string, admission TriggerAdmission) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET prompt=?,title=?,command=?,updated_at=? WHERE id=? AND state='queued'`,
		admission.Prompt, admission.Title, admission.SelectionName, now, jobID)
	if err != nil {
		return fmt.Errorf("update queued job spec: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return ErrJobActive
	}
	if admission.Task != nil {
		raw, err := json.Marshal(*admission.Task)
		if err != nil {
			return fmt.Errorf("encode queued task: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_inputs(job_id,task) VALUES(?,?) ON CONFLICT(job_id) DO UPDATE SET task=excluded.task`, jobID, string(raw)); err != nil {
			return fmt.Errorf("update queued task: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx, `DELETE FROM task_inputs WHERE job_id=?`, jobID); err != nil {
		return fmt.Errorf("clear queued task: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET command=?,command_hash=?,executor=?,model=?,rendered_prompt=? WHERE job_id=? AND state='queued'`,
		admission.Command.Name, admission.Command.Hash, admission.Command.Executor, admission.Command.Model, admission.Command.Prompt, jobID); err != nil {
		return fmt.Errorf("update queued run spec: %w", err)
	}
	if admission.SourceKind != "" {
		if err := stampJobSource(ctx, tx, jobID, admission.SourceKind, ""); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RecordTriggerAttempt records one poll or scheduling attempt. Candidate counts are
// cumulative. Failed attempts do not advance the occurrence or next due time.
func (s *Store) RecordTriggerAttempt(ctx context.Context, identity, configGeneration string, candidates int, attemptErr error) error {
	if candidates < 0 {
		return errors.New("trigger candidate count cannot be negative")
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	health := "healthy"
	latestError := ""
	if attemptErr != nil {
		health = "failed"
		latestError = boundedTriggerError(attemptErr.Error())
	}
	result, err := s.db.ExecContext(ctx, `UPDATE trigger_state SET
  last_attempt_at=?,
  candidate_count=candidate_count+?,
  health=CASE
    WHEN ?='healthy' AND health='coalesced' THEN 'coalesced'
	WHEN ?='healthy' AND EXISTS (SELECT 1 FROM jobs WHERE jobs.trigger_identity=trigger_state.identity AND jobs.trigger_generation_id=trigger_state.generation_id AND jobs.state IN ('queued','running')) THEN 'active'
	WHEN ?='healthy' AND last_job_state='failed' THEN 'failed'
    ELSE ?
  END,
  latest_error=CASE
	WHEN ?='healthy' AND last_job_state='failed' THEN last_job_error
    ELSE ?
  END,
  updated_at=?
WHERE identity=? AND generation_id=?`, now, candidates, health, health, health, health, health, latestError, now, identity, configGeneration)
	if err != nil {
		return fmt.Errorf("record trigger %q attempt: %w", identity, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: %s", ErrTriggerStale, identity)
	}
	return nil
}

func (s *Store) SetTriggerNextDue(ctx context.Context, identity, configGeneration string, nextDue time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE trigger_state SET next_due_at=?,updated_at=? WHERE identity=? AND generation_id=?`, nullableTimeText(nextDue), s.now().UTC().Format(time.RFC3339Nano), identity, configGeneration)
	if err != nil {
		return fmt.Errorf("set trigger %q next due time: %w", identity, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: %s", ErrTriggerStale, identity)
	}
	return nil
}

func (s *Store) SetTriggerPendingOccurrence(ctx context.Context, identity, configGeneration string, occurrence time.Time) error {
	if occurrence.IsZero() {
		return errors.New("trigger pending occurrence is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE trigger_state SET pending_occurrence_at=?,updated_at=? WHERE identity=? AND generation_id=?`, nullableTimeText(occurrence), s.now().UTC().Format(time.RFC3339Nano), identity, configGeneration)
	if err != nil {
		return fmt.Errorf("set trigger %q pending occurrence: %w", identity, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: %s", ErrTriggerStale, identity)
	}
	return nil
}

// TriggerOccurrenceExists distinguishes an idempotent admission from an occurrence that
// is still waiting behind active work for the same subject.
func (s *Store) TriggerOccurrenceExists(ctx context.Context, identity, occurrenceKey string) (bool, error) {
	if identity == "" || occurrenceKey == "" {
		return false, errors.New("trigger identity and occurrence key are required")
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE trigger_identity=? AND occurrence_key=?)`, identity, occurrenceKey).Scan(&exists); err != nil {
		return false, fmt.Errorf("read trigger occurrence: %w", err)
	}
	return exists == 1, nil
}

// RejectGitHubTriggerRequest durably consumes an unauthorized request before
// its label is removed. Reconciliation remains pending until GitHub confirms
// that no newer request event was hidden by the label transition.
func (s *Store) RejectGitHubTriggerRequest(ctx context.Context, request GitHubTriggerRequest) error {
	if request.TriggerIdentity == "" || request.OccurrenceKey == "" || request.ConfigGeneration == "" || request.Repository == "" || request.IssueNumber <= 0 || request.Subject == "" || request.Actor == "" || request.RequestLabel == "" || request.RequestedAt.IsZero() {
		return errors.New("complete github trigger request metadata is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var generation string
	if err := tx.QueryRowContext(ctx, `SELECT generation_id FROM trigger_state WHERE identity=?`, request.TriggerIdentity).Scan(&generation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrTriggerMissing, request.TriggerIdentity)
		}
		return err
	}
	if generation != request.ConfigGeneration {
		return fmt.Errorf("%w: %s", ErrTriggerStale, request.TriggerIdentity)
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO github_trigger_requests(trigger_identity,occurrence_key,config_generation,repository,issue_number,subject,actor,request_label,requested_at,state,needs_reconciliation,updated_at)
VALUES(?,?,?,?,?,?,?,?,?, 'rejected',1,?)
ON CONFLICT(trigger_identity,occurrence_key) DO UPDATE SET
  state=CASE WHEN github_trigger_requests.state='admitted' THEN 'admitted' ELSE 'rejected' END,
  config_generation=excluded.config_generation,
  request_label=excluded.request_label,
  needs_reconciliation=1,
  updated_at=excluded.updated_at`, request.TriggerIdentity, request.OccurrenceKey, request.ConfigGeneration, request.Repository, request.IssueNumber, request.Subject, request.Actor, request.RequestLabel, request.RequestedAt.UTC().Format(time.RFC3339Nano), now); err != nil {
		return fmt.Errorf("persist rejected github trigger request: %w", err)
	}
	return tx.Commit()
}

func (s *Store) GitHubTriggerReconciliations(ctx context.Context, identity string) ([]GitHubTriggerRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT trigger_identity,occurrence_key,config_generation,repository,issue_number,subject,actor,request_label,requested_at,state,COALESCE(job_id,'')
FROM github_trigger_requests
WHERE trigger_identity=? AND needs_reconciliation=1
ORDER BY requested_at, occurrence_key`, identity)
	if err != nil {
		return nil, fmt.Errorf("read github trigger reconciliations: %w", err)
	}
	defer rows.Close()
	var requests []GitHubTriggerRequest
	for rows.Next() {
		var request GitHubTriggerRequest
		var requestedAt string
		if err := rows.Scan(&request.TriggerIdentity, &request.OccurrenceKey, &request.ConfigGeneration, &request.Repository, &request.IssueNumber, &request.Subject, &request.Actor, &request.RequestLabel, &requestedAt, &request.State, &request.JobID); err != nil {
			return nil, fmt.Errorf("read github trigger reconciliation: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, requestedAt)
		if err != nil {
			return nil, fmt.Errorf("parse github trigger request time: %w", err)
		}
		request.RequestedAt = parsed
		requests = append(requests, request)
	}
	return requests, rows.Err()
}

func (s *Store) CompleteGitHubTriggerReconciliation(ctx context.Context, identity, occurrenceKey, generation string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE github_trigger_requests SET needs_reconciliation=0,updated_at=? WHERE trigger_identity=? AND occurrence_key=? AND config_generation=?`, s.now().UTC().Format(time.RFC3339Nano), identity, occurrenceKey, generation)
	if err != nil {
		return fmt.Errorf("complete github trigger reconciliation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: %s", ErrTriggerStale, identity)
	}
	return nil
}

func (s *Store) AddTriggerCoalesced(ctx context.Context, identity, configGeneration string, count int64) error {
	if count < 0 {
		return errors.New("trigger coalesced count cannot be negative")
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `UPDATE trigger_state SET coalesced_count=coalesced_count+?,health=CASE WHEN ?>0 THEN 'coalesced' ELSE health END,updated_at=? WHERE identity=? AND generation_id=?`, count, count, now, identity, configGeneration)
	if err != nil {
		return fmt.Errorf("record trigger %q coalesced occurrences: %w", identity, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("%w: %s", ErrTriggerStale, identity)
	}
	return nil
}

func (s *Store) Poll(ctx context.Context, request protocol.PollRequest) (*protocol.RunSpec, error) {
	return s.poll(ctx, request, 0)
}

// poll allows at most maxConcurrentJobs running jobs. Zero leaves concurrency
// unlimited. Expired leases remain eligible so interrupted work can make progress.
func (s *Store) poll(ctx context.Context, request protocol.PollRequest, maxConcurrentJobs int) (*protocol.RunSpec, error) {
	if maxConcurrentJobs < 0 {
		return nil, errors.New("max concurrent jobs cannot be negative")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	nowTime := s.now().UTC()
	now := nowTime.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO workers(instance_id,name,last_seen_at) VALUES(?,?,?) ON CONFLICT(instance_id) DO UPDATE SET name=excluded.name,last_seen_at=excluded.last_seen_at`, request.InstanceID, request.Name, now); err != nil {
		return nil, fmt.Errorf("update worker: %w", err)
	}
	if _, err := reclaimLeases(ctx, tx, nowTime); err != nil {
		return nil, fmt.Errorf("reclaim expired leases: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM worker_repositories WHERE worker_instance=?`, request.InstanceID); err != nil {
		return nil, fmt.Errorf("clear worker repositories: %w", err)
	}
	repositories := stringSet(request.Repositories)
	for repository := range repositories {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO known_repositories(repository) VALUES(?)`, repository); err != nil {
			return nil, fmt.Errorf("store known repository: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO worker_repositories(worker_instance,repository) VALUES(?,?)`, request.InstanceID, repository); err != nil {
			return nil, fmt.Errorf("store worker repository: %w", err)
		}
	}
	active, err := scanRunSpec(tx.QueryRowContext(ctx, `SELECT id,job_id,command,command_hash,executor,model,repository,rendered_prompt,timeout_ms,lease_token FROM runs WHERE worker_instance=? AND state='running' LIMIT 1`, request.InstanceID))
	if err == nil {
		var activeWorkflow bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_jobs WHERE job_id=?)`, active.JobID).Scan(&activeWorkflow); err != nil {
			return nil, err
		}
		if err := s.enrichRun(ctx, tx, &active); err != nil {
			return nil, err
		}
		if err := enrichReview(ctx, tx, &active); err != nil {
			return nil, err
		}
		if activeWorkflow && !request.SharedOutputs {
			var plan string
			var index int
			if err := tx.QueryRowContext(ctx, `SELECT w.plan,a.step FROM workflow_jobs w JOIN workflow_attempts a ON a.run_id=? WHERE w.job_id=?`, active.ID, active.JobID).Scan(&plan, &index); err != nil {
				return nil, err
			}
			var steps []config.WorkflowStep
			if err := json.Unmarshal([]byte(plan), &steps); err != nil {
				return nil, err
			}
			if index < 0 || index >= len(steps) {
				return nil, errors.New("invalid workflow step")
			}
			if steps[index].SharedOutputs {
				return nil, errors.New("worker must support shared task outputs")
			}
		}
		if active.Revision != nil && !request.Reviews {
			return nil, errors.New("worker must support review feedback")
		}
		if active.Task != nil && !request.Artifacts {
			return nil, errors.New("worker must support artifacts")
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		if activeWorkflow && !request.Workflows {
			return nil, errors.New("worker must support workflow results")
		}
		active.Workflow = activeWorkflow
		return &active, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	atCapacity := false
	if maxConcurrentJobs > 0 {
		var runningJobs int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE state='running'`).Scan(&runningJobs); err != nil {
			return nil, fmt.Errorf("count running jobs: %w", err)
		}
		atCapacity = runningJobs >= maxConcurrentJobs
	}

	executors := stringSet(request.Executors)
	rows, err := tx.QueryContext(ctx, `SELECT r.id,r.job_id,r.command,r.command_hash,r.executor,r.model,r.repository,r.rendered_prompt,r.timeout_ms,j.state,COALESCE((SELECT plan FROM workflow_jobs WHERE job_id=j.id),'') FROM runs r JOIN jobs j ON j.id=r.job_id WHERE r.state='queued' AND (NOT EXISTS(SELECT 1 FROM workflow_jobs w WHERE w.job_id=j.id) OR (? AND EXISTS(SELECT 1 FROM workflow_jobs w WHERE w.job_id=j.id AND (w.worker_name='' OR w.worker_name=?)))) AND (? OR NOT EXISTS(SELECT 1 FROM task_inputs t WHERE t.job_id=j.id)) AND (? OR NOT EXISTS(SELECT 1 FROM execution_reviews er WHERE er.run_id=r.id)) ORDER BY r.rowid`, request.Workflows, request.Name, request.Artifacts, request.Reviews)
	if err != nil {
		return nil, err
	}
	var selected protocol.RunSpec
	for rows.Next() {
		var candidate protocol.RunSpec
		var jobState, workflowPlan string
		if err := rows.Scan(&candidate.ID, &candidate.JobID, &candidate.Command, &candidate.CommandHash, &candidate.Executor, &candidate.Model, &candidate.Repository, &candidate.RenderedPrompt, &candidate.TimeoutMillis, &jobState, &workflowPlan); err != nil {
			rows.Close()
			return nil, err
		}
		if atCapacity && jobState != "running" {
			continue
		}
		if workflowPlan != "" {
			var steps []config.WorkflowStep
			if err := json.Unmarshal([]byte(workflowPlan), &steps); err != nil {
				rows.Close()
				return nil, err
			}
			capable := true
			for _, step := range steps {
				if (step.SharedOutputs && !request.SharedOutputs) || !executors[step.Command.Executor] || !supportsModel(request.Models, step.Command.Executor, step.Command.Model) {
					capable = false
					break
				}
			}
			if !capable {
				continue
			}
		}
		if executors[candidate.Executor] && repositories[candidate.Repository] && supportsModel(request.Models, candidate.Executor, candidate.Model) {
			selected = candidate
			break
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if selected.ID == "" {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_jobs WHERE job_id=?)`, selected.JobID).Scan(&selected.Workflow); err != nil {
		return nil, err
	}
	if selected.Workflow {
		if _, err := tx.ExecContext(ctx, `UPDATE workflow_jobs SET worker_name=? WHERE job_id=? AND worker_name=''`, request.Name, selected.JobID); err != nil {
			return nil, err
		}
	}
	selected.LeaseToken, err = randomID("lease", 24)
	if err != nil {
		return nil, err
	}
	expiresAt := nowTime.Add(leaseDuration).UnixNano()
	result, err := tx.ExecContext(ctx, `UPDATE runs SET state='running',worker_instance=?,worker_name=?,lease_token=?,lease_expires_at=?,started_at=? WHERE id=? AND state='queued'`, request.InstanceID, request.Name, selected.LeaseToken, expiresAt, now, selected.ID)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return nil, fmt.Errorf("lease run: concurrent state change")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state='running',updated_at=? WHERE id=? AND state='queued'`, now, selected.JobID); err != nil {
		return nil, err
	}
	if err := s.enrichRun(ctx, tx, &selected); err != nil {
		return nil, err
	}
	if err := enrichReview(ctx, tx, &selected); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &selected, nil
}

func (s *Store) Complete(ctx context.Context, runID string, completion protocol.Completion) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var jobID, state, instanceID, leaseToken, startedAt, triggerIdentity, triggerGeneration string
	var leaseExpiresAt sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT r.job_id,r.state,COALESCE(r.worker_instance,''),COALESCE(r.lease_token,''),r.lease_expires_at,COALESCE(r.started_at,''),COALESCE(j.trigger_identity,''),COALESCE(j.trigger_generation_id,'') FROM runs r JOIN jobs j ON j.id=r.job_id WHERE r.id=?`, runID).Scan(&jobID, &state, &instanceID, &leaseToken, &leaseExpiresAt, &startedAt, &triggerIdentity, &triggerGeneration); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(instanceID), []byte(completion.InstanceID)) != 1 || subtle.ConstantTimeCompare([]byte(leaseToken), []byte(completion.LeaseToken)) != 1 {
		return ErrLeaseConflict
	}
	if terminalRunState(state) {
		return tx.Commit()
	}
	if state != "running" {
		return ErrRunState
	}
	if !leaseExpiresAt.Valid || leaseExpiresAt.Int64 <= s.now().UTC().UnixNano() {
		return ErrLeaseConflict
	}
	if !terminalRunState(completion.State) {
		return fmt.Errorf("%w: invalid terminal run state %q", ErrInvalidCompletion, completion.State)
	}
	if err := validateOutcome(completion.State, completion.ExitCode); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidCompletion, err)
	}
	durationMillis, tokenUsage := resultMetrics(completion.Result)
	completedAt := s.now().UTC()
	now := completedAt.Format(time.RFC3339Nano)
	if durationMillis == nil {
		durationMillis = elapsedMillis(startedAt, now)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET state=?,exit_code=?,error=?,result=?,events=?,lease_expires_at=NULL,completed_at=?,duration_millis=?,token_usage=? WHERE id=?`, completion.State, completion.ExitCode, completion.Error, string(completion.Result), completion.Events, now, durationMillis, tokenUsage, runID); err != nil {
		return err
	}
	if handled, err := completeWorkflow(ctx, tx, jobID, runID, completion, now); handled || err != nil {
		if err != nil {
			return err
		}
		return tx.Commit()
	}
	if completion.State == "succeeded" {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state='succeeded',updated_at=? WHERE id=?`, now, jobID); err != nil {
			return err
		}
		if triggerIdentity != "" {
			if _, err := tx.ExecContext(ctx, `UPDATE trigger_state SET last_success_at=?,last_job_state='succeeded',last_job_error='',health='healthy',latest_error='',updated_at=? WHERE identity=? AND generation_id=?`, now, now, triggerIdentity, triggerGeneration); err != nil {
				return fmt.Errorf("record trigger success: %w", err)
			}
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET state='failed',updated_at=? WHERE id=?`, now, jobID); err != nil {
			return err
		}
		if triggerIdentity != "" {
			latestError := completion.Error
			if latestError == "" {
				latestError = "triggered job " + completion.State
			}
			latestError = boundedTriggerError(latestError)
			if _, err := tx.ExecContext(ctx, `UPDATE trigger_state SET last_job_state='failed',last_job_error=?,health='failed',latest_error=?,updated_at=? WHERE identity=? AND generation_id=?`, latestError, latestError, now, triggerIdentity, triggerGeneration); err != nil {
				return fmt.Errorf("record trigger failure: %w", err)
			}
		}
	}
	return tx.Commit()
}

func (s *Store) Heartbeat(ctx context.Context, runID string, heartbeat protocol.Heartbeat) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, instanceID, leaseToken string
	var leaseExpiresAt sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT state,COALESCE(worker_instance,''),COALESCE(lease_token,''),lease_expires_at FROM runs WHERE id=?`, runID).Scan(&state, &instanceID, &leaseToken, &leaseExpiresAt); err != nil {
		return err
	}
	if state != "running" {
		return ErrRunState
	}
	if subtle.ConstantTimeCompare([]byte(instanceID), []byte(heartbeat.InstanceID)) != 1 || subtle.ConstantTimeCompare([]byte(leaseToken), []byte(heartbeat.LeaseToken)) != 1 {
		return ErrLeaseConflict
	}
	now := s.now().UTC()
	if !leaseExpiresAt.Valid || leaseExpiresAt.Int64 <= now.UnixNano() {
		return ErrLeaseConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE runs SET lease_expires_at=? WHERE id=? AND state='running' AND worker_instance=? AND lease_token=?`, now.Add(leaseDuration).UnixNano(), runID, heartbeat.InstanceID, heartbeat.LeaseToken)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return ErrLeaseConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workers SET last_seen_at=? WHERE instance_id=?`, now.Format(time.RFC3339Nano), heartbeat.InstanceID); err != nil {
		return fmt.Errorf("update worker heartbeat: %w", err)
	}
	return tx.Commit()
}

func (s *Store) DeleteJob(ctx context.Context, jobID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM jobs WHERE id=?`, jobID).Scan(&state); err != nil {
		return err
	}
	if !terminalJobState(state) {
		return ErrJobActive
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE github_trigger_requests SET job_id=NULL,updated_at=? WHERE job_id=?`, now, jobID); err != nil {
		return fmt.Errorf("clear deleted job trigger links: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM runs WHERE job_id=?`, jobID); err != nil {
		return fmt.Errorf("delete job runs: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE id=?`, jobID); err != nil {
		return fmt.Errorf("delete job: %w", err)
	}
	return tx.Commit()
}

func (s *Store) ReclaimExpiredLeases(ctx context.Context) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	n, err := reclaimLeases(ctx, tx, s.now().UTC())
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

func (s *Store) PruneSupersededWorkers(ctx context.Context, seenAfter time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM workers AS old
WHERE julianday(old.last_seen_at) < julianday(?)
  AND NOT EXISTS (SELECT 1 FROM runs r WHERE r.worker_instance=old.instance_id AND r.state='running')
  AND EXISTS (
    SELECT 1 FROM workers newer
    WHERE newer.name=old.name
      AND (julianday(newer.last_seen_at) > julianday(old.last_seen_at)
        OR (newer.last_seen_at=old.last_seen_at AND newer.instance_id>old.instance_id))
  )`, seenAfter.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("prune superseded workers: %w", err)
	}
	return result.RowsAffected()
}

func (s *Store) Snapshot(ctx context.Context) (Snapshot, error) {
	jobs, err := s.listJobs(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	workers, err := s.listWorkers(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	triggers, err := s.TriggerSnapshot(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Jobs: jobs, Workers: workers, Triggers: triggers}, nil
}

func (s *Store) TriggerSnapshot(ctx context.Context) ([]TriggerStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.identity,t.family,t.config_signature,t.generation_id,COALESCE(t.next_due_at,''),COALESCE(t.pending_occurrence_at,''),COALESCE(t.last_attempt_at,''),COALESCE(t.last_success_at,''),COALESCE((SELECT j.id FROM jobs j WHERE j.trigger_identity=t.identity AND j.trigger_generation_id=t.generation_id AND j.state IN ('queued','running') ORDER BY j.created_at LIMIT 1),''),t.candidate_count,t.admission_count,t.coalesced_count,t.health,t.latest_error FROM trigger_state t ORDER BY t.identity`)
	if err != nil {
		return nil, fmt.Errorf("read trigger snapshot: %w", err)
	}
	defer rows.Close()
	statuses := []TriggerStatus{}
	for rows.Next() {
		var status TriggerStatus
		var nextDue, pendingOccurrence, lastAttempt, lastSuccess string
		if err := rows.Scan(&status.Identity, &status.Family, &status.ConfigSignature, &status.ConfigGeneration, &nextDue, &pendingOccurrence, &lastAttempt, &lastSuccess, &status.ActiveJobID, &status.CandidateCount, &status.AdmissionCount, &status.CoalescedCount, &status.Health, &status.LatestError); err != nil {
			return nil, fmt.Errorf("read trigger snapshot: %w", err)
		}
		status.NextDueAt = parseOptionalTime(nextDue)
		status.PendingOccurrenceAt = parseOptionalTime(pendingOccurrence)
		status.LastAttemptAt = parseOptionalTime(lastAttempt)
		status.LastSuccessAt = parseOptionalTime(lastSuccess)
		if status.Health == "healthy" && status.ActiveJobID == "" && status.PendingOccurrenceAt == nil && status.NextDueAt != nil && status.NextDueAt.Before(s.now().UTC()) {
			status.Health = "stale"
		}
		statuses = append(statuses, status)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read trigger snapshot: %w", err)
	}
	return statuses, nil
}

func (s *Store) AvailableRepositories(ctx context.Context, seenAfter time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT wr.repository
FROM worker_repositories wr
JOIN workers w ON w.instance_id=wr.worker_instance
WHERE julianday(w.last_seen_at) >= julianday(?)
   OR EXISTS (SELECT 1 FROM runs r WHERE r.worker_instance=w.instance_id AND r.state='running')
ORDER BY wr.repository`, seenAfter.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repositories := []string{}
	for rows.Next() {
		var repository string
		if err := rows.Scan(&repository); err != nil {
			return nil, err
		}
		repositories = append(repositories, repository)
	}
	return repositories, rows.Err()
}

func (s *Store) KnownRepositories(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT repository FROM known_repositories ORDER BY repository`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	repositories := []string{}
	for rows.Next() {
		var repository string
		if err := rows.Scan(&repository); err != nil {
			return nil, err
		}
		repositories = append(repositories, repository)
	}
	return repositories, rows.Err()
}

func (s *Store) RunOutput(ctx context.Context, runID string) (RunOutput, error) {
	var output RunOutput
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(result,''),COALESCE(events,'') FROM runs WHERE id=?`, runID).Scan(&output.Result, &output.Events)
	return output, err
}

func (s *Store) listJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT j.id,j.title,j.prompt,j.repository,j.github_issue_title,j.command,j.trigger_identity,j.occurrence_key,j.trigger_subject,j.state,j.previous_job_id,j.attempt,COALESCE(j.superseded_by,''),j.issue_key,j.resolution_kind,j.resolution_detail,COALESCE(j.resolved_at,''),j.created_at,j.updated_at,
COALESCE(r.id,''),COALESCE(r.command,''),COALESCE(r.executor,''),COALESCE(r.model,''),COALESCE(r.state,''),COALESCE(NULLIF(r.worker_name,''),w.name,''),r.exit_code,COALESCE(r.error,''),COALESCE(r.started_at,''),COALESCE(r.completed_at,''),r.duration_millis,r.token_usage,a.step,COALESCE(a.outcome,''),COALESCE(NULLIF(a.summary,''),CASE WHEN json_valid(r.result) THEN json_extract(r.result,'$.final_message') END,'')
FROM jobs j LEFT JOIN runs r ON r.job_id=j.id LEFT JOIN workers w ON w.instance_id=r.worker_instance LEFT JOIN workflow_attempts a ON a.run_id=r.id ORDER BY j.created_at DESC,j.id,r.rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []Job{}
	for rows.Next() {
		job := Job{Runs: []Run{}}
		var run Run
		var created, updated, started, completed string
		var resolutionKind, resolutionDetail, resolvedAt string
		if err := rows.Scan(&job.ID, &job.Title, &job.Prompt, &job.Repository, &job.GitHubIssueTitle, &job.Command, &job.TriggerID, &job.OccurrenceKey, &job.TriggerSubject, &job.State, &job.PreviousJobID, &job.Attempt, &job.SupersededBy, &job.IssueKey, &resolutionKind, &resolutionDetail, &resolvedAt, &created, &updated,
			&run.ID, &run.Command, &run.Executor, &run.Model, &run.State, &run.WorkerName, &run.ExitCode, &run.Error, &started, &completed, &run.DurationMillis, &run.TokenUsage, &run.Step, &run.Outcome, &run.Summary); err != nil {
			return nil, err
		}
		job.ResolutionKind = resolutionKind
		job.ResolutionDetail = resolutionDetail
		if resolvedAt != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, resolvedAt); err == nil {
				job.ResolvedAt = &parsed
			}
		}
		job.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		job.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
		if run.ID != "" {
			run.StartedAt, _ = time.Parse(time.RFC3339Nano, started)
			run.CompletedAt, _ = time.Parse(time.RFC3339Nano, completed)
			job.Runs = append(job.Runs, run)
		}
		if len(jobs) > 0 && jobs[len(jobs)-1].ID == job.ID {
			jobs[len(jobs)-1].Runs = append(jobs[len(jobs)-1].Runs, job.Runs...)
		} else {
			jobs = append(jobs, job)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if err := s.loadReviews(ctx, jobs); err != nil {
		return nil, err
	}
	if err := s.loadTasks(ctx, jobs); err != nil {
		return nil, err
	}
	if err := s.loadWorkflowProgress(ctx, jobs); err != nil {
		return nil, err
	}
	if err := loadJobMetadata(ctx, s.db, jobs); err != nil {
		return nil, fmt.Errorf("read job metadata: %w", err)
	}
	if err := loadJobStateHistory(ctx, s.db, jobs); err != nil {
		return nil, fmt.Errorf("read job state history: %w", err)
	}
	return jobs, nil
}

// loadJobStateHistory attaches each job's recorded state transitions, oldest
// first. Jobs from before the history existed have no entries.
func loadJobStateHistory(ctx context.Context, db *sql.DB, jobs []Job) error {
	byID := make(map[string]*Job, len(jobs))
	for i := range jobs {
		byID[jobs[i].ID] = &jobs[i]
	}
	rows, err := db.QueryContext(ctx, `SELECT job_id,state,changed_at FROM job_state_history ORDER BY rowid`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var jobID, state, changedAt string
		if err := rows.Scan(&jobID, &state, &changedAt); err != nil {
			return err
		}
		job := byID[jobID]
		if job == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, changedAt)
		if err != nil {
			continue
		}
		job.StateHistory = append(job.StateHistory, StateTransition{State: state, ChangedAt: at})
	}
	return rows.Err()
}

func resultMetrics(result []byte) (*int64, *int64) {
	var fields map[string]json.RawMessage
	if len(result) == 0 || json.Unmarshal(result, &fields) != nil {
		return nil, nil
	}
	return nonNegativeInteger(fields["duration_millis"]), nonNegativeInteger(fields["token_usage"])
}

func nonNegativeInteger(raw json.RawMessage) *int64 {
	var value int64
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil || value < 0 {
		return nil
	}
	return &value
}

func elapsedMillis(startedAt, completedAt string) *int64 {
	started, startErr := time.Parse(time.RFC3339Nano, startedAt)
	completed, completionErr := time.Parse(time.RFC3339Nano, completedAt)
	if startErr != nil || completionErr != nil {
		return nil
	}
	duration := completed.Sub(started).Milliseconds()
	if duration < 0 {
		duration = 0
	}
	return &duration
}

func (s *Store) listWorkers(ctx context.Context) ([]Worker, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT instance_id,name,last_seen_at FROM workers ORDER BY name,instance_id`)
	if err != nil {
		return nil, err
	}
	workers := []Worker{}
	for rows.Next() {
		worker := Worker{Repositories: []string{}}
		var lastSeen string
		if err := rows.Scan(&worker.InstanceID, &worker.Name, &lastSeen); err != nil {
			rows.Close()
			return nil, err
		}
		worker.LastSeenAt, _ = time.Parse(time.RFC3339Nano, lastSeen)
		workers = append(workers, worker)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	repositories, err := s.db.QueryContext(ctx, `SELECT worker_instance,repository FROM worker_repositories ORDER BY worker_instance,repository`)
	if err != nil {
		return nil, err
	}
	defer repositories.Close()
	byInstance := make(map[string]int, len(workers))
	for index, worker := range workers {
		byInstance[worker.InstanceID] = index
	}
	for repositories.Next() {
		var instanceID, repository string
		if err := repositories.Scan(&instanceID, &repository); err != nil {
			return nil, err
		}
		if index, ok := byInstance[instanceID]; ok {
			workers[index].Repositories = append(workers[index].Repositories, repository)
		}
	}
	return workers, repositories.Err()
}

func scanRunSpec(row *sql.Row) (protocol.RunSpec, error) {
	var run protocol.RunSpec
	err := row.Scan(&run.ID, &run.JobID, &run.Command, &run.CommandHash, &run.Executor, &run.Model, &run.Repository, &run.RenderedPrompt, &run.TimeoutMillis, &run.LeaseToken)
	return run, err
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func supportsModel(capabilities map[string][]string, executor, model string) bool {
	if model == "" {
		return true
	}
	models, ok := capabilities[executor]
	if !ok {
		return false
	}
	if len(models) == 0 {
		return true
	}
	return stringSet(models)[model]
}

func fixedTriggerFamily(family string) bool {
	return family == "interval" || family == "cron"
}

func nullableTimeText(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseOptionalTime(value string) *time.Time {
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	return &parsed
}

func boundedTriggerError(message string) string {
	runes := []rune(message)
	if len(runes) > maxTriggerErrorLength {
		runes = runes[:maxTriggerErrorLength]
	}
	return string(runes)
}

func terminalRunState(state string) bool {
	return state == "succeeded" || state == "failed" || state == "timed_out" || state == "cancelled"
}

func terminalJobState(state string) bool {
	return state == "succeeded" || state == "failed" || state == "cancelled" || state == "timed_out" || state == "resolved"
}

// resolutionStates are the job states a resolution may be applied to. Queued
// and running jobs wait until they finish; already resolved jobs are skipped.
const resolutionStates = `('succeeded','failed','timed_out','cancelled')`

// validResolutionKind reports whether kind is an accepted resolution origin:
// a merged pull request, a provider state change (Plane or Linear), or a
// native backlog issue.
func validResolutionKind(kind string) bool {
	switch kind {
	case "pr", "plane", "linear", "backlog":
		return true
	}
	return false
}

// MarkJobResolved moves one terminal job into the resolved state and records
// how it was resolved.
func (s *Store) MarkJobResolved(ctx context.Context, jobID, kind, detail string, resolvedAt time.Time) error {
	if !validResolutionKind(kind) {
		return fmt.Errorf("unknown resolution kind %q", kind)
	}
	if strings.TrimSpace(detail) == "" {
		return errors.New("resolution detail is required")
	}
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET state='resolved',resolution_kind=?,resolution_detail=?,resolved_at=?,updated_at=? WHERE id=? AND state IN `+resolutionStates, kind, detail, resolvedAt.UTC().Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), jobID)
	if err != nil {
		return fmt.Errorf("resolve job %s: %w", jobID, err)
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return fmt.Errorf("job %s is not in a resolvable state", jobID)
	}
	return nil
}

// ResolveTicketsForIssue resolves every terminal job tied to an issue key,
// matched against the stored key, the title or the trigger subject. It returns
// the number of jobs resolved.
func (s *Store) ResolveTicketsForIssue(ctx context.Context, key, kind, detail string, resolvedAt time.Time) (int64, error) {
	if strings.TrimSpace(key) == "" {
		return 0, errors.New("issue key is required")
	}
	if !validResolutionKind(kind) {
		return 0, fmt.Errorf("unknown resolution kind %q", kind)
	}
	now := s.now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE jobs SET state='resolved',resolution_kind=?,resolution_detail=?,resolved_at=?,updated_at=? WHERE state IN `+resolutionStates+` AND resolution_kind='' AND (issue_key=? OR title LIKE ? OR trigger_subject LIKE ?)`,
		kind, detail, resolvedAt.UTC().Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), key, "%"+key+"%", "%"+key+"%")
	if err != nil {
		return 0, fmt.Errorf("resolve tickets for %s: %w", key, err)
	}
	return result.RowsAffected()
}

// CreateBacklogIssue persists a new native issue with the next per-project
// sequence key. The key is allocated inside the create transaction from the
// highest numeric suffix already used by the project.
func (s *Store) CreateBacklogIssue(ctx context.Context, project, title, spec, issueURL string) (backlog.Issue, error) {
	issue := backlog.Issue{Project: project, Title: title, Spec: spec, URL: issueURL, State: backlog.StateBacklog, Revision: 1}
	if err := issue.Validate(); err != nil {
		return backlog.Issue{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return backlog.Issue{}, err
	}
	defer tx.Rollback()
	var highest int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(CAST(SUBSTR(key,?) AS INTEGER)),0) FROM backlog_issues WHERE project=?`, len(project)+2, project).Scan(&highest); err != nil {
		return backlog.Issue{}, fmt.Errorf("read backlog key sequence for %s: %w", project, err)
	}
	issue.Key = fmt.Sprintf("%s-%d", project, highest+1)
	id, err := randomID("issue", 12)
	if err != nil {
		return backlog.Issue{}, err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO backlog_issues(id,project,key,title,spec,state,revision,url,created_at,updated_at) VALUES(?,?,?,?,?,?,1,?,?,?)`,
		id, issue.Project, issue.Key, issue.Title, issue.Spec, issue.State, issue.URL, now, now); err != nil {
		return backlog.Issue{}, fmt.Errorf("insert backlog issue: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return backlog.Issue{}, fmt.Errorf("commit backlog issue: %w", err)
	}
	issue.ID, issue.CreatedAt, issue.UpdatedAt = id, now, now
	return issue, nil
}

// GetBacklogIssue returns one native issue; sql.ErrNoRows means it does not exist.
func (s *Store) GetBacklogIssue(ctx context.Context, id string) (backlog.Issue, error) {
	return scanBacklogIssue(s.db.QueryRowContext(ctx, `SELECT id,project,key,title,spec,state,revision,url,created_at,updated_at FROM backlog_issues WHERE id=?`, id))
}

// ListBacklogIssues returns issues filtered by project, state, and a substring
// match on title or key, ordered by project and key.
func (s *Store) ListBacklogIssues(ctx context.Context, project, state, q string) ([]backlog.Issue, error) {
	query := `SELECT id,project,key,title,spec,state,revision,url,created_at,updated_at FROM backlog_issues`
	var conditions []string
	var args []any
	if project != "" {
		conditions = append(conditions, "project=?")
		args = append(args, project)
	}
	if state != "" {
		if !backlog.ValidState(state) {
			return nil, fmt.Errorf("unknown issue state %q", state)
		}
		conditions = append(conditions, "state=?")
		args = append(args, state)
	}
	if q != "" {
		conditions = append(conditions, "(title LIKE ? OR key LIKE ?)")
		args = append(args, "%"+q+"%", "%"+q+"%")
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += ` ORDER BY project,key`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list backlog issues: %w", err)
	}
	defer rows.Close()
	var issues []backlog.Issue
	for rows.Next() {
		issue, err := scanBacklogIssue(rows)
		if err != nil {
			return nil, err
		}
		issues = append(issues, issue)
	}
	return issues, rows.Err()
}

// UpdateBacklogIssue applies a partial edit and bumps the revision so later
// readers can detect that the issue changed after a run was started from it.
func (s *Store) UpdateBacklogIssue(ctx context.Context, id string, update backlog.Update) (backlog.Issue, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return backlog.Issue{}, err
	}
	defer tx.Rollback()
	issue, err := scanBacklogIssue(tx.QueryRowContext(ctx, `SELECT id,project,key,title,spec,state,revision,url,created_at,updated_at FROM backlog_issues WHERE id=?`, id))
	if err != nil {
		return backlog.Issue{}, err
	}
	issue = issue.Apply(update)
	if err := issue.Validate(); err != nil {
		return backlog.Issue{}, err
	}
	issue.Revision++
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `UPDATE backlog_issues SET title=?,spec=?,state=?,revision=?,updated_at=? WHERE id=?`,
		issue.Title, issue.Spec, issue.State, issue.Revision, now, id); err != nil {
		return backlog.Issue{}, fmt.Errorf("update backlog issue %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return backlog.Issue{}, fmt.Errorf("commit backlog issue update: %w", err)
	}
	issue.UpdatedAt = now
	return issue, nil
}

// MarkBacklogStartedOnce moves a backlog or todo issue to started exactly once.
// Later starts leave the state alone so humans keep control of it. The state
// flip is a mutation, so it bumps the revision like any other edit.
func (s *Store) MarkBacklogStartedOnce(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE backlog_issues SET state=?,revision=revision+1,updated_at=? WHERE id=? AND state IN (?,?)`,
		backlog.StateStarted, s.now().UTC().Format(time.RFC3339Nano), id, backlog.StateBacklog, backlog.StateTodo)
	if err != nil {
		return fmt.Errorf("mark backlog issue %s started: %w", id, err)
	}
	return nil
}

func scanBacklogIssue(row interface{ Scan(dest ...any) error }) (backlog.Issue, error) {
	var issue backlog.Issue
	err := row.Scan(&issue.ID, &issue.Project, &issue.Key, &issue.Title, &issue.Spec, &issue.State, &issue.Revision, &issue.URL, &issue.CreatedAt, &issue.UpdatedAt)
	if err != nil {
		return backlog.Issue{}, err
	}
	return issue, nil
}

func validateOutcome(state string, exitCode int) error {
	switch state {
	case "succeeded":
		if exitCode != 0 {
			return errors.New("succeeded run must have exit code 0")
		}
	case "failed":
		if exitCode == 0 {
			return errors.New("failed run must have a non-zero exit code")
		}
	case "timed_out":
		if exitCode != 124 {
			return errors.New("timed out run must have exit code 124")
		}
	case "cancelled":
		if exitCode != 130 {
			return errors.New("cancelled run must have exit code 130")
		}
	}
	return nil
}

func randomID(prefix string, byteCount int) (string, error) {
	body := make([]byte, byteCount)
	if _, err := rand.Read(body); err != nil {
		return "", fmt.Errorf("generate %s ID: %w", prefix, err)
	}
	return prefix + "_" + hex.EncodeToString(body), nil
}
