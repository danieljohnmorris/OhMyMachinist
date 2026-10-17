package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/config"
)

const ticketSchema = `
CREATE TABLE IF NOT EXISTS tickets (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 key TEXT UNIQUE,
 title TEXT NOT NULL,
 description TEXT NOT NULL DEFAULT '',
 repository TEXT NOT NULL DEFAULT '',
 project TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL DEFAULT 'backlog',
 job_id TEXT NOT NULL DEFAULT '',
 pr_url TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL,
 updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS ticket_comments (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 ticket_key TEXT NOT NULL REFERENCES tickets(key) ON DELETE CASCADE,
 body TEXT NOT NULL,
 created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS ticket_jobs (
 job_id TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
 ticket_key TEXT NOT NULL REFERENCES tickets(key));
`

var errTicketInvalid = errors.New("invalid ticket")
var errTicketConflict = errors.New("ticket is already handed to the factory or finished")

type Ticket struct {
	Key         string          `json:"key"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Repository  string          `json:"repository"`
	Project     string          `json:"project,omitempty"`
	State       string          `json:"state"`
	JobID       string          `json:"job_id,omitempty"`
	PRURL       string          `json:"pr_url,omitempty"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	Comments    []TicketComment `json:"comments"`
}
type TicketComment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

const ticketColumns = `key,title,description,repository,project,state,job_id,pr_url,created_at,updated_at`

func scanTicket(row interface{ Scan(...any) error }) (Ticket, error) {
	var t Ticket
	err := row.Scan(&t.Key, &t.Title, &t.Description, &t.Repository, &t.Project, &t.State, &t.JobID, &t.PRURL, &t.CreatedAt, &t.UpdatedAt)
	t.Comments = []TicketComment{}
	return t, err
}
func (s *Store) ticket(ctx context.Context, key string) (Ticket, error) {
	t, err := scanTicket(s.db.QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE key=?`, key))
	if err != nil {
		return t, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,body,created_at FROM ticket_comments WHERE ticket_key=? ORDER BY id`, key)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var c TicketComment
		if err := rows.Scan(&c.ID, &c.Body, &c.CreatedAt); err != nil {
			return t, err
		}
		t.Comments = append(t.Comments, c)
	}
	return t, rows.Err()
}

type ticketExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// releaseBuiltinTickets returns built-in tickets whose active attempt was
// cancelled or deleted outside the completion path to the planning state, with
// one comment, so they can be approved again. jobsWhere selects the affected
// jobs. Tickets already in review, done or cancelled are left alone.
func releaseBuiltinTickets(ctx context.Context, ex ticketExecer, now, reason, jobsWhere string, args ...any) error {
	scope := `job_id IN (SELECT id FROM jobs WHERE ` + jobsWhere + `) AND state IN ('approved','in_progress')`
	commentArgs := append([]any{reason, now}, args...)
	if _, err := ex.ExecContext(ctx, `INSERT INTO ticket_comments(ticket_key,body,created_at) SELECT key,?||' Approve again to start a new run.',? FROM tickets WHERE `+scope, commentArgs...); err != nil {
		return fmt.Errorf("comment on released tickets: %w", err)
	}
	updateArgs := append([]any{now}, args...)
	if _, err := ex.ExecContext(ctx, `UPDATE tickets SET job_id='',state='todo',updated_at=? WHERE `+scope, updateArgs...); err != nil {
		return fmt.Errorf("release tickets: %w", err)
	}
	return nil
}

// Dispatched tickets retain their delivery history; only drafts can be deleted.
func (s *Store) deleteTicket(ctx context.Context, key string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var job string
	if err := tx.QueryRowContext(ctx, `SELECT job_id FROM tickets WHERE key=?`, key).Scan(&job); err != nil {
		return err
	}
	var linked int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ticket_jobs WHERE ticket_key=?`, key).Scan(&linked); err != nil {
		return err
	}
	if job != "" || linked > 0 {
		return errTicketConflict
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM tickets WHERE key=?`, key); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) tickets(ctx context.Context) ([]Ticket, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+ticketColumns+` FROM tickets ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Ticket{}
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}
func validateTicket(t Ticket) error {
	if strings.TrimSpace(t.Title) == "" || len(t.Title) > 500 {
		return fmt.Errorf("%w: title must contain 1–500 characters", errTicketInvalid)
	}
	if len(t.Description) > 256<<10 || len(t.Repository) > 256 {
		return fmt.Errorf("%w: description or repository is too long", errTicketInvalid)
	}
	switch t.State {
	case "backlog", "todo", "approved", "in_progress", "in_review", "done", "cancelled":
		return nil
	}
	return fmt.Errorf("%w: unknown state", errTicketInvalid)
}
func (s *Store) createTicket(ctx context.Context, t Ticket, prefix string) (Ticket, error) {
	t.Title = strings.TrimSpace(t.Title)
	t.State = "backlog"
	if err := validateTicket(t); err != nil {
		return t, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return t, err
	}
	defer tx.Rollback()
	now := s.now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `INSERT INTO tickets(title,description,repository,project,created_at,updated_at) VALUES(?,?,?,?,?,?)`, t.Title, t.Description, t.Repository, t.Project, now, now)
	if err != nil {
		return t, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return t, err
	}
	t.Key = fmt.Sprintf("%s-%d", prefix, id)
	if _, err = tx.ExecContext(ctx, `UPDATE tickets SET key=? WHERE id=?`, t.Key, id); err != nil {
		return t, err
	}
	if err = tx.Commit(); err != nil {
		return t, err
	}
	return s.ticket(ctx, t.Key)
}
func (s *Store) updateTicket(ctx context.Context, key string, fields map[string]string) (Ticket, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Ticket{}, err
	}
	defer tx.Rollback()
	t, err := scanTicket(tx.QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE key=?`, key))
	if err != nil {
		return t, err
	}
	for field, value := range fields {
		switch field {
		case "title":
			t.Title = strings.TrimSpace(value)
		case "description":
			t.Description = value
		case "repository":
			if t.JobID != "" && value != t.Repository {
				return t, errTicketConflict
			}
			t.Repository = value
		case "state":
			if value == "approved" || value == "in_progress" {
				return t, fmt.Errorf("%w: use Approve for factory to start work", errTicketInvalid)
			}
			if t.JobID != "" && (value == "backlog" || value == "todo") {
				return t, errTicketConflict
			}
			t.State = value
		default:
			return t, fmt.Errorf("%w: unknown field %s", errTicketInvalid, field)
		}
	}
	if err = validateTicket(t); err != nil {
		return t, err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `UPDATE tickets SET title=?,description=?,repository=?,state=?,updated_at=? WHERE key=?`, t.Title, t.Description, t.Repository, t.State, now, key); err != nil {
		return t, err
	}
	if t.State == "done" {
		if _, err = tx.ExecContext(ctx, `UPDATE jobs SET state='resolved',resolution_kind='ticket',resolution_detail='Done',resolved_at=?,updated_at=? WHERE id IN (SELECT job_id FROM ticket_jobs WHERE ticket_key=?) AND state IN ('succeeded','failed','cancelled','timed_out')`, now, now, key); err != nil {
			return t, err
		}
	}
	if err = tx.Commit(); err != nil {
		return t, err
	}
	return s.ticket(ctx, key)
}
func (s *Store) approveTicket(ctx context.Context, key string, command config.ResolvedCommand) (string, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	t, err := scanTicket(tx.QueryRowContext(ctx, `SELECT `+ticketColumns+` FROM tickets WHERE key=?`, key))
	if err != nil {
		return "", false, err
	}
	if t.JobID != "" {
		return t.JobID, false, nil
	}
	if t.State != "backlog" && t.State != "todo" {
		return "", false, errTicketConflict
	}
	if t.Repository == "" {
		return "", false, fmt.Errorf("%w: choose a repository before approval", errTicketInvalid)
	}
	prompt := t.Key + ": " + t.Title + "\n\n" + t.Description
	command, err = config.RenderPrompt(command, prompt)
	if err != nil {
		return "", false, err
	}
	jobID, err := randomID("job", 12)
	if err != nil {
		return "", false, err
	}
	runID, err := randomID("run", 12)
	if err != nil {
		return "", false, err
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT INTO jobs(id,title,prompt,repository,command,issue_key,state,created_at,updated_at) VALUES(?,?,?,?,?,?,'queued',?,?)`, jobID, t.Key+": "+t.Title, prompt, t.Repository, command.Name, key, now, now); err != nil {
		return "", false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO runs(id,job_id,command,command_hash,executor,model,repository,rendered_prompt,timeout_ms,state) VALUES(?,?,?,?,?,?,?,?,?,'queued')`, runID, jobID, command.Name, command.Hash, command.Executor, command.Model, t.Repository, command.Prompt, command.Timeout.Milliseconds()); err != nil {
		return "", false, err
	}
	identity := map[string]any{"tracker": "builtin", "ticket_key": key, "issue_url": "#/tickets/" + key}
	if t.Project != "" {
		identity["project"] = t.Project
	}
	if err = saveJobMetadata(ctx, tx, jobID, identity); err != nil {
		return "", false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_jobs(job_id,ticket_key) VALUES(?,?)`, jobID, key); err != nil {
		return "", false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE tickets SET state='approved',job_id=?,updated_at=? WHERE key=?`, jobID, now, key); err != nil {
		return "", false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO ticket_comments(ticket_key,body,created_at) VALUES(?,?,?)`, key, "Approved for factory. Run: #/runs/"+jobID, now); err != nil {
		return "", false, err
	}
	return jobID, true, tx.Commit()
}

// Called inside completion's transaction, so feedback cannot be lost on restart.
func completeBuiltinTicket(ctx context.Context, tx *sql.Tx, jobID, now string) error {
	var key, ticketState, jobState string
	err := tx.QueryRowContext(ctx, `SELECT t.key,t.state,j.state FROM ticket_jobs tj JOIN tickets t ON t.key=tj.ticket_key JOIN jobs j ON j.id=tj.job_id WHERE j.id=?`, jobID).Scan(&key, &ticketState, &jobState)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !terminalJobState(jobState) {
		return nil
	}
	if ticketState == "done" {
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET state='resolved',resolution_kind='ticket',resolution_detail='Done',resolved_at=?,updated_at=? WHERE id=?`, now, now, jobID)
	} else if ticketState != "cancelled" {
		state := "in_progress"
		if jobState == "succeeded" {
			state = "in_review"
		}
		_, err = tx.ExecContext(ctx, `UPDATE tickets SET state=?,updated_at=? WHERE key=?`, state, now, key)
	}
	if err != nil {
		return err
	}
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT metadata FROM job_metadata WHERE job_id=?`, jobID).Scan(&raw); err != nil {
		return err
	}
	var metadata JobMetadata
	if err = json.Unmarshal([]byte(raw), &metadata); err != nil {
		return err
	}
	body := "Factory run " + jobState + ": #/runs/" + jobID
	if metadata.PRURL != "" {
		body += "\nPull request: " + metadata.PRURL
		// The PR link is durable ticket state, not just a comment: it survives
		// state changes and shows on the ticket without reading the run.
		if _, err = tx.ExecContext(ctx, `UPDATE tickets SET pr_url=?,updated_at=? WHERE key=? AND pr_url=''`, metadata.PRURL, now, key); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO ticket_comments(ticket_key,body,created_at) VALUES(?,?,?)`, key, body, now)
	return err
}
