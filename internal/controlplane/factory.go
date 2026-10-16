package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Factory controls let an operator pause or resume dispatch, set how many jobs
// run at once, and enable or disable individual lanes without touching
// systemd. State lives in the settings table so it survives a restart of the
// control plane; the reserved "factory." prefix keeps the generic settings
// endpoints from bypassing the validation below.

// factorySettingPrefix reserves the settings namespace used by the controls.
const factorySettingPrefix = "factory."

const (
	settingFactoryState        = "factory.state"
	settingFactoryReason       = "factory.reason"
	settingFactoryActor        = "factory.actor"
	settingFactorySince        = "factory.since"
	settingFactoryJobCap       = "factory.max_concurrent_jobs"
	settingFactoryDisabledLane = "factory.disabled_lanes"
)

// Factory dispatch states.
const (
	FactoryRunning  = "running"
	FactoryPaused   = "paused"
	FactoryDraining = "draining"
)

// FactoryLane is one worker slot in the factory view. A lane is a ready
// checkout that can take a job; the job cap is how many of them may run at
// once.
type FactoryLane struct {
	InstanceID   string   `json:"instance_id"`
	Name         string   `json:"name"`
	Connected    bool     `json:"connected"`
	Enabled      bool     `json:"enabled"`
	Busy         bool     `json:"busy"`
	State        string   `json:"state"`
	JobID        string   `json:"job_id,omitempty"`
	Repositories []string `json:"repositories"`
}

// FactoryState is the current factory control state and lane view.
type FactoryState struct {
	State             string        `json:"state"`
	Reason            string        `json:"reason,omitempty"`
	Actor             string        `json:"actor,omitempty"`
	Since             *time.Time    `json:"since,omitempty"`
	MaxConcurrentJobs int           `json:"max_concurrent_jobs"`
	ReadyLanes        int           `json:"ready_lanes"`
	EnabledLanes      int           `json:"enabled_lanes"`
	Lanes             []FactoryLane `json:"lanes"`
}

// factorySettings is the persisted control state read from the settings table.
type factorySettings struct {
	State         string
	Reason        string
	Actor         string
	Since         *time.Time
	JobCap        int
	HasJobCap     bool
	DisabledLanes map[string]bool
}

// loadFactorySettings reads the persisted control state. Missing rows mean the
// factory is running with no cap override and every lane enabled.
func (s *Store) loadFactorySettings(ctx context.Context) (factorySettings, error) {
	values, err := s.Settings(ctx)
	if err != nil {
		return factorySettings{}, err
	}
	settings := factorySettings{State: FactoryRunning, DisabledLanes: map[string]bool{}}
	if state := values[settingFactoryState]; state != "" {
		settings.State = state
	}
	settings.Reason = values[settingFactoryReason]
	settings.Actor = values[settingFactoryActor]
	if since := values[settingFactorySince]; since != "" {
		if parsed, parseErr := time.Parse(time.RFC3339Nano, since); parseErr == nil {
			settings.Since = &parsed
		}
	}
	if raw := values[settingFactoryJobCap]; raw != "" {
		var cap int
		if _, scanErr := fmt.Sscanf(raw, "%d", &cap); scanErr == nil && cap > 0 {
			settings.JobCap = cap
			settings.HasJobCap = true
		}
	}
	if raw := values[settingFactoryDisabledLane]; raw != "" {
		var names []string
		if json.Unmarshal([]byte(raw), &names) == nil {
			for _, name := range names {
				settings.DisabledLanes[name] = true
			}
		}
	}
	return settings, nil
}

// rowQueryer is satisfied by both *sql.DB and *sql.Tx so lane counting works
// inside the poll transaction (the store holds a single connection).
type rowQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// countReadyLanes counts the lanes that are both connected and enabled, the
// upper bound the job cap is clamped to. Names listed as disabled are skipped.
func countReadyLanes(ctx context.Context, q rowQueryer, seenAfter time.Time, disabled map[string]bool) (int, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM workers WHERE julianday(last_seen_at) >= julianday(?)`, seenAfter.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("count ready lanes: %w", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return 0, err
		}
		if disabled[name] {
			continue
		}
		count++
	}
	return count, rows.Err()
}

// ClampJobCap forces a requested cap into [1, lanes]. A zero lane count means
// no lane is connected yet, so only the minimum applies.
func ClampJobCap(requested, lanes int) int {
	if requested < 1 {
		requested = 1
	}
	if lanes > 0 && requested > lanes {
		requested = lanes
	}
	return requested
}

// resolveJobCap resolves the cap dispatch should honour: the stored override
// when present, otherwise the configured default, clamped to the ready enabled
// lanes. A resolved zero means unlimited.
func resolveJobCap(configured int, settings factorySettings, lanes int) int {
	limit := configured
	if settings.HasJobCap {
		limit = settings.JobCap
	}
	if limit <= 0 {
		return 0
	}
	return ClampJobCap(limit, lanes)
}

// FactoryStatus reports the control state, the effective job cap and one slot
// per registered lane. configured is the server's default cap.
func (s *Store) FactoryStatus(ctx context.Context, configured int) (FactoryState, error) {
	settings, err := s.loadFactorySettings(ctx)
	if err != nil {
		return FactoryState{}, err
	}
	now := s.now().UTC()
	seenAfter := now.Add(-workerAvailabilityWindow)
	lanes, err := countReadyLanes(ctx, s.db, seenAfter, settings.DisabledLanes)
	if err != nil {
		return FactoryState{}, err
	}
	workers, err := s.listWorkers(ctx)
	if err != nil {
		return FactoryState{}, err
	}
	running, err := s.runningJobsByWorker(ctx)
	if err != nil {
		return FactoryState{}, err
	}
	state := FactoryState{
		State:             settings.State,
		Reason:            settings.Reason,
		Actor:             settings.Actor,
		Since:             settings.Since,
		MaxConcurrentJobs: resolveJobCap(configured, settings, lanes),
		Lanes:             []FactoryLane{},
	}
	for _, worker := range workers {
		connected := !worker.LastSeenAt.Before(seenAfter)
		enabled := !settings.DisabledLanes[worker.Name]
		lane := FactoryLane{
			InstanceID:   worker.InstanceID,
			Name:         worker.Name,
			Connected:    connected,
			Enabled:      enabled,
			Repositories: worker.Repositories,
		}
		switch {
		case !connected:
			lane.State = "offline"
		case !enabled:
			lane.State = "disabled"
		case running[worker.InstanceID] != "":
			lane.State = "busy"
			lane.Busy = true
			lane.JobID = running[worker.InstanceID]
		default:
			lane.State = "idle"
		}
		if connected && enabled {
			state.ReadyLanes++
		}
		if enabled {
			state.EnabledLanes++
		}
		state.Lanes = append(state.Lanes, lane)
	}
	return state, nil
}

// runningJobsByWorker maps each worker instance to the job id of its active run.
func (s *Store) runningJobsByWorker(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT worker_instance,job_id FROM runs WHERE state='running' AND worker_instance IS NOT NULL`)
	if err != nil {
		return nil, fmt.Errorf("read running lanes: %w", err)
	}
	defer rows.Close()
	running := map[string]string{}
	for rows.Next() {
		var instance, jobID string
		if err := rows.Scan(&instance, &jobID); err != nil {
			return nil, err
		}
		running[instance] = jobID
	}
	return running, rows.Err()
}

// ResumeFactory clears a pause or drain and lets dispatch continue.
func (s *Store) ResumeFactory(ctx context.Context) error {
	if err := s.putSetting(ctx, settingFactoryState, FactoryRunning); err != nil {
		return err
	}
	for _, key := range []string{settingFactoryReason, settingFactoryActor, settingFactorySince} {
		if err := s.putSetting(ctx, key, ""); err != nil {
			return err
		}
	}
	return nil
}

// PauseFactory stops dispatching new jobs. mode is "drain" (let the running job
// finish) or "cancel" (cancel it now). reason is recorded with the actor.
func (s *Store) PauseFactory(ctx context.Context, actor, reason, mode string) error {
	state := FactoryDraining
	if mode == "cancel" {
		state = FactoryPaused
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if err := s.putSetting(ctx, settingFactoryState, state); err != nil {
		return err
	}
	if err := s.putSetting(ctx, settingFactoryReason, reason); err != nil {
		return err
	}
	if err := s.putSetting(ctx, settingFactoryActor, actor); err != nil {
		return err
	}
	if err := s.putSetting(ctx, settingFactorySince, now); err != nil {
		return err
	}
	if mode == "cancel" {
		return s.CancelRunningJobs(ctx)
	}
	return nil
}

// SetFactoryJobCap persists a job cap clamped to the ready enabled lanes and
// returns the value that took effect.
func (s *Store) SetFactoryJobCap(ctx context.Context, requested int) (int, error) {
	if requested < 1 {
		return 0, fmt.Errorf("max concurrent jobs must be at least 1")
	}
	settings, err := s.loadFactorySettings(ctx)
	if err != nil {
		return 0, err
	}
	lanes, err := countReadyLanes(ctx, s.db, s.now().UTC().Add(-workerAvailabilityWindow), settings.DisabledLanes)
	if err != nil {
		return 0, err
	}
	clamped := ClampJobCap(requested, lanes)
	if err := s.putSetting(ctx, settingFactoryJobCap, fmt.Sprintf("%d", clamped)); err != nil {
		return 0, err
	}
	return clamped, nil
}

// SetLaneEnabled enables or disables one lane by worker name. Disabling a lane
// re-clamps the job cap so it never exceeds the enabled lanes.
func (s *Store) SetLaneEnabled(ctx context.Context, name string, enabled bool) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("lane name is required")
	}
	settings, err := s.loadFactorySettings(ctx)
	if err != nil {
		return err
	}
	if enabled {
		delete(settings.DisabledLanes, name)
	} else {
		settings.DisabledLanes[name] = true
	}
	names := make([]string, 0, len(settings.DisabledLanes))
	for lane := range settings.DisabledLanes {
		names = append(names, lane)
	}
	slices.Sort(names)
	encoded, err := json.Marshal(names)
	if err != nil {
		return err
	}
	if err := s.putSetting(ctx, settingFactoryDisabledLane, string(encoded)); err != nil {
		return err
	}
	if !enabled && settings.HasJobCap {
		if _, err := s.SetFactoryJobCap(ctx, settings.JobCap); err != nil {
			return err
		}
	}
	return nil
}

// CancelRunningJobs cancels every run that is running now and its job. Queued
// jobs stay queued: they are not dispatched while the factory is stopped and
// resume when it starts again.
func (s *Store) CancelRunningJobs(ctx context.Context) error {
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET state='cancelled',updated_at=? WHERE state='running'`, now); err != nil {
		return fmt.Errorf("cancel running jobs: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE runs SET state='cancelled',exit_code=130,error='Cancelled by factory stop',completed_at=? WHERE state='running'`, now); err != nil {
		return fmt.Errorf("cancel running runs: %w", err)
	}
	return nil
}
