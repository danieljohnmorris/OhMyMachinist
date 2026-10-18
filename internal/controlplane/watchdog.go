package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// A failure whose heal was lost — the control plane died between recording
// the terminal state and the async HealRun finishing (a reboot, a crash) —
// would otherwise sit failed forever: no requeue, no alert, nothing. The
// startup sweep re-runs healing for those.

// healSweepWindow bounds how far back the sweep reaches. Healing is about
// recent failures lost to a crash, not archaeology: older failed jobs are
// operator territory.
const healSweepWindow = 24 * time.Hour

// sweepUnhealedFailures re-runs healing for recent failed jobs that carry no
// healing record. Guards keep it boring:
//   - superseded attempts already have a later attempt (the heal would just
//     duplicate it);
//   - a newer job with the same repository and title means someone already
//     requeued it (manual requeues carry the original title);
//   - HealRun itself enforces the retry budget and workflow-job rules.
func (s *Server) sweepUnhealedFailures(ctx context.Context) error {
	cutoff := s.now().UTC().Add(-healSweepWindow).Format(time.RFC3339Nano)
	rows, err := s.store.db.QueryContext(ctx, `SELECT j.id, j.title, j.repository, COALESCE((SELECT metadata FROM job_metadata WHERE job_id=j.id),''),
		EXISTS(SELECT 1 FROM jobs newer WHERE newer.repository=j.repository AND newer.title=j.title AND newer.created_at>j.created_at),
		(SELECT r.id FROM runs r WHERE r.job_id=j.id ORDER BY r.rowid DESC LIMIT 1)
		FROM jobs j WHERE j.state='failed' AND COALESCE(j.superseded_by,'')='' AND j.updated_at>=?`, cutoff)
	if err != nil {
		return fmt.Errorf("list unhealed failures: %w", err)
	}
	defer rows.Close()
	var runIDs []string
	for rows.Next() {
		var jobID, title, repository, metadata string
		var requeued int
		var lastRun sql.NullString
		if err := rows.Scan(&jobID, &title, &repository, &metadata, &requeued, &lastRun); err != nil {
			return fmt.Errorf("scan unhealed failure: %w", err)
		}
		if requeued == 1 || lastRun.Valid == false || hasHealingMetadata(metadata) {
			continue
		}
		runIDs = append(runIDs, lastRun.String)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list unhealed failures: %w", err)
	}
	for _, runID := range runIDs {
		log.Printf("healing sweep: re-running heal for run %s lost to a restart", runID)
		s.HealRun(ctx, runID)
	}
	return nil
}

// hasHealingMetadata reports whether the job's metadata blob already carries a
// healing record, so the sweep never re-decides a healed failure.
func hasHealingMetadata(metadata string) bool {
	if metadata == "" {
		return false
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(metadata), &parsed); err != nil {
		return false
	}
	_, ok := parsed["healing"]
	return ok
}

// workerStarvationWindow is how long the queue may wait without any worker
// heartbeat before the watchdog spools an alert. Workers heartbeat every 10s,
// so this only trips when no worker is running at all.
const workerStarvationWindow = 10 * time.Minute

// workerStarvationAlert is the JSON spooled when jobs are queued but no
// worker has heartbeated recently.
type workerStarvationAlert struct {
	QueuedRuns int    `json:"queued_runs"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
	CheckedAt  string `json:"checked_at"`
	Reason     string `json:"reason"`
}

// checkWorkerLiveness alerts when runs are queued but no worker has
// heartbeated within the starvation window — the reboot-orphaned-queue case.
// The alert is a state file like the health checks: overwritten while starved,
// removed once a worker returns.
func (s *Server) checkWorkerLiveness(ctx context.Context) error {
	if s.failAlerts == nil {
		return nil
	}
	var queued int
	if err := s.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE state='queued'`).Scan(&queued); err != nil {
		return fmt.Errorf("count queued runs: %w", err)
	}
	if queued == 0 {
		return s.failAlerts.remove("worker-starvation.json")
	}
	var lastSeen sql.NullString
	if err := s.store.db.QueryRowContext(ctx, `SELECT MAX(last_seen_at) FROM workers`).Scan(&lastSeen); err != nil {
		return fmt.Errorf("read worker heartbeats: %w", err)
	}
	if lastSeen.Valid && lastSeen.String != "" {
		if seen, err := time.Parse(time.RFC3339Nano, lastSeen.String); err == nil {
			if s.now().UTC().Sub(seen) < workerStarvationWindow {
				return s.failAlerts.remove("worker-starvation.json")
			}
		}
	}
	alert := workerStarvationAlert{
		QueuedRuns: queued,
		CheckedAt:  s.now().UTC().Format(time.RFC3339),
		Reason:     "jobs are queued but no worker has heartbeated recently; start the machinist-worker units",
	}
	if lastSeen.Valid {
		alert.LastSeenAt = lastSeen.String
	}
	if err := s.failAlerts.writeNamed("worker-starvation.json", alert); err != nil {
		return fmt.Errorf("spool worker starvation alert: %w", err)
	}
	return nil
}
