package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// resolveBuiltinTicket resolves the finished jobs of a builtin ticket in one
// transaction. With a merged pull request, an open ticket moves to done and
// gets one comment, and its jobs resolve as "pr"; a cancelled ticket keeps its
// state but its jobs still resolve. Without a merged PR, jobs resolve as
// "ticket" only when the ticket is already done. Unknown tickets and open
// tickets without a merged PR are left alone. It returns the number of jobs
// resolved.
func (s *Store) resolveBuiltinTicket(ctx context.Context, key string, pullRequest *GitHubPullRequest) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRowContext(ctx, `SELECT state FROM tickets WHERE key=?`, key).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	kind, detail := "ticket", "Done"
	switch {
	case pullRequest != nil:
		kind, detail = "pr", resolutionDetail(*pullRequest)
		if state != "done" && state != "cancelled" {
			if _, err = tx.ExecContext(ctx, `UPDATE tickets SET state='done',updated_at=? WHERE key=?`, now, key); err != nil {
				return 0, err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_comments(ticket_key,body,created_at) VALUES(?,?,?)`, key, builtinResolutionComment(*pullRequest), now); err != nil {
				return 0, err
			}
		}
	case state != "done":
		return 0, nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE jobs SET state='resolved',resolution_kind=?,resolution_detail=?,resolved_at=?,updated_at=? WHERE id IN (SELECT job_id FROM ticket_jobs WHERE ticket_key=?) AND state IN `+resolutionStates, kind, detail, now, now, key)
	if err != nil {
		return 0, err
	}
	resolved, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return resolved, tx.Commit()
}
