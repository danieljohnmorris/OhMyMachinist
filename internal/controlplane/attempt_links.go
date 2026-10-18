package controlplane

import (
	"context"
	"fmt"
	"sort"
)

// linkTicketAttempts stores the Plane ticket key on every job that names one
// and chains each ticket's jobs (per repository) in creation order through
// previous_job_id/attempt/superseded_by. Requeues arrive through several
// paths (manual submit, OMM retry, Plane intake) that do not link attempts
// themselves, so this runs after every submit and on each reconcile pass,
// and doubles as the backfill for existing jobs. updated_at is left alone so
// linking does not reorder the board.
func (s *Store) linkTicketAttempts(ctx context.Context) error {
	jobs, err := s.listJobs(ctx, "")
	if err != nil {
		return fmt.Errorf("list jobs for attempt links: %w", err)
	}
	groups := map[string][]Job{}
	for _, job := range jobs {
		key := ticketKey(job)
		if key == "" {
			continue
		}
		if job.IssueKey == "" {
			if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET issue_key=? WHERE id=? AND issue_key=''`, key, job.ID); err != nil {
				return fmt.Errorf("store issue key: %w", err)
			}
		}
		group := job.Repository + "\n" + key
		groups[group] = append(groups[group], job)
	}
	for _, members := range groups {
		sort.SliceStable(members, func(i, j int) bool {
			if members[i].CreatedAt.Equal(members[j].CreatedAt) {
				return members[i].ID < members[j].ID
			}
			return members[i].CreatedAt.Before(members[j].CreatedAt)
		})
		for index, job := range members {
			previous, next := "", ""
			if index > 0 {
				previous = members[index-1].ID
			}
			if index+1 < len(members) {
				next = members[index+1].ID
			}
			if job.PreviousJobID == previous && job.Attempt == index+1 && job.SupersededBy == next {
				continue
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET previous_job_id=?,attempt=?,superseded_by=? WHERE id=?`, previous, index+1, next, job.ID); err != nil {
				return fmt.Errorf("link attempt %s: %w", job.ID, err)
			}
		}
		// A failed attempt superseded by a later one is bookkeeping, not an
		// outstanding failure: record why it ended so raw counts can tell the
		// difference. Guarded so a ticket-level resolution is never clobbered.
		if _, err := s.db.ExecContext(ctx, `UPDATE jobs SET resolution_kind='superseded', resolution_detail=? WHERE state='failed' AND resolution_kind='' AND superseded_by<>''`, "retried by a later attempt"); err != nil {
			return fmt.Errorf("record superseded attempts: %w", err)
		}
	}
	return nil
}
