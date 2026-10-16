package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// planeSchema holds the intake state that used to live in JSON files beside
// the retired plane-intake.py: seen specs, state-sync progress, alert dedupe,
// shadow decisions and the imported script's job records.
const planeSchema = `
CREATE TABLE IF NOT EXISTS plane_intake (
 issue_id TEXT PRIMARY KEY,
 spec_hash TEXT NOT NULL DEFAULT '',
 key TEXT NOT NULL DEFAULT '',
 project_id TEXT NOT NULL DEFAULT '',
 job_id TEXT NOT NULL DEFAULT '',
 source TEXT NOT NULL DEFAULT '',
 delivery_id TEXT NOT NULL DEFAULT '',
 updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS plane_state_sync (
 job_id TEXT PRIMARY KEY,
 started TEXT NOT NULL DEFAULT '',
 ended TEXT NOT NULL DEFAULT '',
 notified TEXT NOT NULL DEFAULT '',
 updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS plane_alert_state (
 issue_id TEXT PRIMARY KEY,
 state_id TEXT NOT NULL DEFAULT '',
 state_name TEXT NOT NULL DEFAULT '',
 state_group TEXT NOT NULL DEFAULT '',
 updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS plane_shadow (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 at TEXT NOT NULL,
 kind TEXT NOT NULL,
 key TEXT NOT NULL,
 decision TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS plane_shadow_at ON plane_shadow(at);
CREATE TABLE IF NOT EXISTS plane_script_job (
 job_dir TEXT PRIMARY KEY,
 key TEXT NOT NULL DEFAULT '',
 issue_id TEXT NOT NULL DEFAULT '',
 spec_hash TEXT NOT NULL DEFAULT '',
 job_id TEXT NOT NULL DEFAULT '',
 source TEXT NOT NULL DEFAULT '',
 imported_at TEXT NOT NULL);`

// PlaneIntakeRecord is the dedupe and provenance state for one Plane ticket.
type PlaneIntakeRecord struct {
	SpecHash   string `json:"spec_hash"`
	Key        string `json:"key"`
	ProjectID  string `json:"project_id"`
	JobID      string `json:"job_id"`
	Source     string `json:"source"`
	DeliveryID string `json:"delivery_id"`
}

// PlaneSyncRecord tracks the state-sync and PR-notify progress for one job.
type PlaneSyncRecord struct {
	JobID    string `json:"job_id"`
	Started  string `json:"started"`
	Ended    string `json:"ended"`
	Notified string `json:"notified"`
}

// PlaneAlertState is the last observed workflow state of a ticket, used to
// dedupe state-change alerts.
type PlaneAlertState struct {
	IssueID string `json:"issue_id"`
	StateID string `json:"state_id"`
	Name    string `json:"name"`
	Group   string `json:"group"`
}

// PlaneShadowDecision is one decision Machinist intake would have made while
// shadowing the legacy script.
type PlaneShadowDecision struct {
	ID       int64  `json:"id"`
	At       string `json:"at"`
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	Decision string `json:"decision"`
}

// PutPlaneIntake upserts a ticket's intake record.
func (s *Store) PutPlaneIntake(ctx context.Context, issueID string, record PlaneIntakeRecord) error {
	if issueID == "" {
		return errors.New("plane intake issue id is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO plane_intake(issue_id,spec_hash,key,project_id,job_id,source,delivery_id,updated_at)
VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(issue_id) DO UPDATE SET spec_hash=excluded.spec_hash,key=excluded.key,project_id=excluded.project_id,job_id=excluded.job_id,source=excluded.source,delivery_id=excluded.delivery_id,updated_at=excluded.updated_at`,
		issueID, record.SpecHash, record.Key, record.ProjectID, record.JobID, record.Source, record.DeliveryID, s.now().UTC().Format(time.RFC3339Nano))
	return err
}

// PlaneIntake reads one ticket's intake record.
func (s *Store) PlaneIntake(ctx context.Context, issueID string) (PlaneIntakeRecord, bool, error) {
	records, err := s.planeIntakeQuery(ctx, `WHERE issue_id=?`, issueID)
	if err != nil {
		return PlaneIntakeRecord{}, false, err
	}
	record, ok := records[issueID]
	return record, ok, nil
}

// PlaneIntakeAll reads every ticket intake record.
func (s *Store) PlaneIntakeAll(ctx context.Context) (map[string]PlaneIntakeRecord, error) {
	return s.planeIntakeQuery(ctx, "", nil)
}

func (s *Store) planeIntakeQuery(ctx context.Context, where string, args ...any) (map[string]PlaneIntakeRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT issue_id,spec_hash,key,project_id,job_id,source,delivery_id FROM plane_intake `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("read plane intake: %w", err)
	}
	defer rows.Close()
	records := map[string]PlaneIntakeRecord{}
	for rows.Next() {
		var issueID string
		var record PlaneIntakeRecord
		if err := rows.Scan(&issueID, &record.SpecHash, &record.Key, &record.ProjectID, &record.JobID, &record.Source, &record.DeliveryID); err != nil {
			return nil, err
		}
		records[issueID] = record
	}
	return records, rows.Err()
}

// PutPlaneSync upserts a job's state-sync record.
func (s *Store) PutPlaneSync(ctx context.Context, record PlaneSyncRecord) error {
	if record.JobID == "" {
		return errors.New("plane sync job id is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO plane_state_sync(job_id,started,ended,notified,updated_at)
VALUES(?,?,?,?,?)
ON CONFLICT(job_id) DO UPDATE SET started=excluded.started,ended=excluded.ended,notified=excluded.notified,updated_at=excluded.updated_at`,
		record.JobID, record.Started, record.Ended, record.Notified, s.now().UTC().Format(time.RFC3339Nano))
	return err
}

// PlaneSyncAll reads every state-sync record.
func (s *Store) PlaneSyncAll(ctx context.Context) (map[string]PlaneSyncRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT job_id,started,ended,notified FROM plane_state_sync`)
	if err != nil {
		return nil, fmt.Errorf("read plane state sync: %w", err)
	}
	defer rows.Close()
	records := map[string]PlaneSyncRecord{}
	for rows.Next() {
		var record PlaneSyncRecord
		if err := rows.Scan(&record.JobID, &record.Started, &record.Ended, &record.Notified); err != nil {
			return nil, err
		}
		records[record.JobID] = record
	}
	return records, rows.Err()
}

// PutPlaneAlertState records the last observed state of a ticket.
func (s *Store) PutPlaneAlertState(ctx context.Context, state PlaneAlertState) error {
	if state.IssueID == "" {
		return errors.New("plane alert state issue id is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO plane_alert_state(issue_id,state_id,state_name,state_group,updated_at)
VALUES(?,?,?,?,?)
ON CONFLICT(issue_id) DO UPDATE SET state_id=excluded.state_id,state_name=excluded.state_name,state_group=excluded.state_group,updated_at=excluded.updated_at`,
		state.IssueID, state.StateID, state.Name, state.Group, s.now().UTC().Format(time.RFC3339Nano))
	return err
}

// PlaneAlertStates reads every ticket's last observed state.
func (s *Store) PlaneAlertStates(ctx context.Context) (map[string]PlaneAlertState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT issue_id,state_id,state_name,state_group FROM plane_alert_state`)
	if err != nil {
		return nil, fmt.Errorf("read plane alert state: %w", err)
	}
	defer rows.Close()
	states := map[string]PlaneAlertState{}
	for rows.Next() {
		var state PlaneAlertState
		if err := rows.Scan(&state.IssueID, &state.StateID, &state.Name, &state.Group); err != nil {
			return nil, err
		}
		states[state.IssueID] = state
	}
	return states, rows.Err()
}

// RecordPlaneShadow appends one shadow-mode decision.
func (s *Store) RecordPlaneShadow(ctx context.Context, at, kind, key, decision string) error {
	if decision == "" {
		return errors.New("plane shadow decision is required")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO plane_shadow(at,kind,key,decision) VALUES(?,?,?,?)`, at, kind, key, decision)
	return err
}

// PlaneShadowDecisions lists the most recent shadow decisions, newest first.
func (s *Store) PlaneShadowDecisions(ctx context.Context, limit int) ([]PlaneShadowDecision, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,at,kind,key,decision FROM plane_shadow ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read plane shadow decisions: %w", err)
	}
	defer rows.Close()
	decisions := []PlaneShadowDecision{}
	for rows.Next() {
		var decision PlaneShadowDecision
		if err := rows.Scan(&decision.ID, &decision.At, &decision.Kind, &decision.Key, &decision.Decision); err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, rows.Err()
}

// PlaneSettings returns the runtime-editable "plane.*" settings.
func (s *Store) PlaneSettings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key,value FROM settings WHERE key LIKE 'plane.%' ORDER BY key`)
	if err != nil {
		return nil, fmt.Errorf("read plane settings: %w", err)
	}
	defer rows.Close()
	values := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		values[key] = value
	}
	return values, rows.Err()
}

// ImportPlaneState imports the retired plane-intake.py state files on first
// start: the repository and supervisor maps, the seen-spec hashes, the
// state-sync progress, the alert dedupe, and the script's job records used for
// the shadow-mode comparison. Each import is skipped when its target already
// holds rows, so a restart never clobbers newer state. It returns whether any
// file was imported.
func (s *Store) ImportPlaneState(ctx context.Context, machinistHome string) (bool, error) {
	if strings.TrimSpace(machinistHome) == "" {
		return false, nil
	}
	jobsDir := filepath.Join(machinistHome, "plane-jobs")
	alertsDir := filepath.Join(machinistHome, "alerts")
	imported := false

	if err := s.importPlaneMapSetting(ctx, filepath.Join(machinistHome, "plane-repos.json"), "plane.repositories", "plane.identifiers@_identifiers"); err != nil {
		return imported, err
	} else if fileExists(filepath.Join(machinistHome, "plane-repos.json")) {
		imported = true
	}
	if err := s.importPlanePolicies(ctx, filepath.Join(machinistHome, "plane-repos.json")); err != nil {
		return imported, err
	}
	if err := s.importPlaneMapSetting(ctx, filepath.Join(machinistHome, "plane-supervisors.json"), "plane.supervisors"); err != nil {
		return imported, err
	} else if fileExists(filepath.Join(machinistHome, "plane-supervisors.json")) {
		imported = true
	}

	seen, err := readJSONObject(filepath.Join(jobsDir, ".seen.json"))
	if err != nil {
		return imported, err
	}
	existingSeen, err := s.PlaneIntakeAll(ctx)
	if err != nil {
		return imported, err
	}
	for issueID, raw := range seen {
		if _, ok := existingSeen[issueID]; ok {
			continue
		}
		var hash string
		if err := json.Unmarshal(raw, &hash); err != nil {
			continue
		}
		if err := s.PutPlaneIntake(ctx, issueID, PlaneIntakeRecord{SpecHash: hash, Source: "import"}); err != nil {
			return imported, err
		}
		imported = true
	}

	sync, err := readJSONObject(filepath.Join(jobsDir, ".state-sync.json"))
	if err != nil {
		return imported, err
	}
	existingSync, err := s.PlaneSyncAll(ctx)
	if err != nil {
		return imported, err
	}
	for jobID, raw := range sync {
		if _, ok := existingSync[jobID]; ok {
			continue
		}
		var legacy struct {
			Start  string `json:"start"`
			End    string `json:"end"`
			Notify string `json:"notify"`
		}
		body, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(body, &legacy); err != nil {
			continue
		}
		if err := s.PutPlaneSync(ctx, PlaneSyncRecord{JobID: jobID, Started: legacy.Start, Ended: legacy.End, Notified: legacy.Notify}); err != nil {
			return imported, err
		}
		imported = true
	}

	states, err := readJSONObject(filepath.Join(alertsDir, ".plane-states.json"))
	if err != nil {
		return imported, err
	}
	existingStates, err := s.PlaneAlertStates(ctx)
	if err != nil {
		return imported, err
	}
	for issueID, raw := range states {
		if _, ok := existingStates[issueID]; ok {
			continue
		}
		body, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		var record struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Group string `json:"group"`
		}
		if err := json.Unmarshal(body, &record); err != nil {
			continue
		}
		if err := s.PutPlaneAlertState(ctx, PlaneAlertState{IssueID: issueID, StateID: record.ID, Name: record.Name, Group: record.Group}); err != nil {
			return imported, err
		}
		imported = true
	}

	jobsImported, err := s.importPlaneScriptJobs(ctx, jobsDir)
	if err != nil {
		return imported, err
	}
	return imported || jobsImported, nil
}

// importPlaneMapSetting stores a legacy JSON map under one or two setting keys.
// keys may be given as "setting" (whole object) or "setting@field" for one
// sub-object such as _identifiers.
// importPlanePolicies imports the optional "_merge_policies" object of
// plane-repos.json (MACH-24) into the "plane.merge_policies" setting, once:
// the per-project keys map Plane project identifiers to MergePolicy objects.
func (s *Store) importPlanePolicies(ctx context.Context, path string) error {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var object struct {
		MergePolicies map[string]any `json:"_merge_policies"`
	}
	if err := json.Unmarshal(body, &object); err != nil || len(object.MergePolicies) == 0 {
		return nil
	}
	if _, empty := s.settingIfEmpty(ctx, "plane.merge_policies"); !empty {
		return nil
	}
	encoded, err := json.Marshal(object.MergePolicies)
	if err != nil {
		return err
	}
	return s.PutSetting(ctx, "plane.merge_policies", string(encoded))
}

func (s *Store) importPlaneMapSetting(ctx context.Context, path string, settings ...string) error {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	for _, setting := range settings {
		key, field, has := strings.Cut(setting, "@")
		payload := object
		if has {
			nested, ok := object[field].(map[string]any)
			if !ok {
				continue
			}
			payload = nested
		} else if _, ok := object["_identifiers"]; ok {
			// The repositories setting excludes the _identifiers metadata,
			// which is imported into plane.identifiers separately.
			filtered := map[string]any{}
			for name, value := range object {
				if name == "_identifiers" {
					continue
				}
				filtered[name] = value
			}
			payload = filtered
		}
		if _, empty := s.settingIfEmpty(ctx, key); !empty {
			continue
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if err := s.PutSetting(ctx, key, string(encoded)); err != nil {
			return err
		}
	}
	return nil
}

// settingIfEmpty reports whether a setting has no value yet.
func (s *Store) settingIfEmpty(ctx context.Context, key string) (string, bool) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", true
	}
	if err != nil {
		return "", false
	}
	return value, strings.TrimSpace(value) == ""
}

// importPlaneScriptJobs imports the legacy job directories' meta.json records.
func (s *Store) importPlaneScriptJobs(ctx context.Context, jobsDir string) (bool, error) {
	entries, err := os.ReadDir(jobsDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read plane jobs directory: %w", err)
	}
	imported := false
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		directory := filepath.Join(jobsDir, entry.Name())
		body, err := os.ReadFile(filepath.Join(directory, "meta.json"))
		if err != nil {
			continue
		}
		var meta struct {
			Key     string `json:"key"`
			IssueID string `json:"issue_id"`
			Hash    string `json:"hash"`
			JobID   string `json:"job_id"`
			Source  string `json:"source"`
		}
		if err := json.Unmarshal(body, &meta); err != nil {
			continue
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO plane_script_job(job_dir,key,issue_id,spec_hash,job_id,source,imported_at)
VALUES(?,?,?,?,?,?,?) ON CONFLICT(job_dir) DO UPDATE SET key=excluded.key,issue_id=excluded.issue_id,spec_hash=excluded.spec_hash,job_id=excluded.job_id,source=excluded.source`,
			entry.Name(), meta.Key, meta.IssueID, meta.Hash, meta.JobID, meta.Source, s.now().UTC().Format(time.RFC3339Nano)); err != nil {
			return imported, fmt.Errorf("import plane script job %s: %w", entry.Name(), err)
		}
		imported = true
	}
	return imported, nil
}

// PlaneScriptJobs lists the imported legacy job records, ordered by key.
func (s *Store) PlaneScriptJobs(ctx context.Context) ([]PlaneShadowDecision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT 0,imported_at,source,key,spec_hash||' '||job_id FROM plane_script_job ORDER BY key,spec_hash`)
	if err != nil {
		return nil, fmt.Errorf("read plane script jobs: %w", err)
	}
	defer rows.Close()
	jobs := []PlaneShadowDecision{}
	for rows.Next() {
		var job PlaneShadowDecision
		if err := rows.Scan(&job.ID, &job.At, &job.Kind, &job.Key, &job.Decision); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// readJSONObject reads a flat JSON object of raw values, tolerating a missing
// or malformed file by returning an empty object.
func readJSONObject(path string) (map[string]json.RawMessage, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil {
		return map[string]json.RawMessage{}, nil
	}
	return object, nil
}
