package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/issues"
)

// The bulk importer one-shot copies every issue of a configured Plane or
// Linear project into the built-in tickets table. It complements the label
// pollers, which only admit issues carrying the intake label: an import
// brings the whole project across, marking finished work so it stays out of
// planning. Repeated imports are idempotent — existing keys are skipped and
// reported instead of failing.

const (
	importSourcePlane  = "plane"
	importSourceLinear = "linear"

	importActionCreate = "create"
	importActionSkip   = "skip"

	importReasonExists    = "exists"
	importReasonCompleted = "completed"

	// validateTicket caps titles at 500 characters; remote issues can be
	// longer and one long title must not fail a whole import.
	importMaxTitleLength = 500
	// validateTicket caps descriptions at 256 KiB.
	importMaxDescriptionLength = 256 << 10
)

// Client constructors are package variables so tests can bind them to stub
// APIs without environment plumbing.
var (
	newPlaneImportClient  = issues.NewPlaneConnection
	newLinearImportClient = issues.NewLinearConnection
)

// errInvalidImport marks request-level failures that map to HTTP 400.
var errInvalidImport = errors.New("invalid import request")

// importTicket is one remote issue that would land in the tickets table.
// Description rides along for the insert but stays out of preview payloads.
type importTicket struct {
	Key         string `json:"key"`
	Title       string `json:"title"`
	State       string `json:"state"`
	Action      string `json:"action"`
	Reason      string `json:"reason,omitempty"`
	Description string `json:"-"`
}

// importProject is one remote project of a source.
type importProject struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Identifier string `json:"identifier,omitempty"`
	// Repository is the configured issue source covering this project; empty
	// when the project has no Machinist connection and cannot be imported.
	Repository string `json:"repository,omitempty"`
}

// importResult serves both the preview and the import response: the project
// roster, the planned tickets and, after a real import, the counts.
type importResult struct {
	Source   string          `json:"source"`
	Project  string          `json:"project,omitempty"`
	Projects []importProject `json:"projects,omitempty"`
	Tickets  []importTicket  `json:"tickets,omitempty"`
	Created  int             `json:"created"`
	Skipped  int             `json:"skipped"`

	// sourceRepository is the issue source key the project resolves to; it is
	// not serialized because every planned ticket shares it.
	sourceRepository string
}

type importRequest struct {
	Source  string `json:"source"`
	Project string `json:"project"`
	DryRun  bool   `json:"dry_run"`
}

// mapImportState maps a remote workflow state type (Linear) or state group
// (Plane) onto a ticket state. Completed and canceled states are skipped:
// finished work must not re-enter planning. Triage and backlog states start
// in backlog.
func mapImportState(stateType string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(stateType)) {
	case "completed", "canceled", "cancelled":
		return "", false
	case "started":
		return "in_progress", true
	default:
		return "backlog", true
	}
}

func importErrorStatus(err error) int {
	if errors.Is(err, errInvalidImport) {
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

func importError(w http.ResponseWriter, err error) {
	writeError(w, importErrorStatus(err), err)
}

// importPreviewHandler serves GET /api/v1/import/preview?source=plane|linear
// with an optional project: without it the response is the remote project
// roster, with it the tickets that would be created. Nothing is written.
func (s *Server) importPreviewHandler(w http.ResponseWriter, r *http.Request) {
	definition, err := config.LoadDefinitions(s.definitionPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	result, err := s.planImport(r.Context(), definition, r.URL.Query().Get("source"), strings.TrimSpace(r.URL.Query().Get("project")))
	if err != nil {
		importError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// importHandler serves POST /api/v1/import {source, project, dry_run?}. A dry
// run behaves like the project preview; a real import inserts the tickets and
// reports created/skipped counts.
func (s *Server) importHandler(w http.ResponseWriter, r *http.Request) {
	if !limitRequestBody(w, r, maxRequestBytes) {
		return
	}
	var input importRequest
	if err := decodeJSON(r, &input); err != nil {
		writeDecodeError(w, err)
		return
	}
	definition, err := config.LoadDefinitions(s.definitionPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	result, err := s.planImport(r.Context(), definition, input.Source, strings.TrimSpace(input.Project))
	if err != nil {
		importError(w, err)
		return
	}
	if !input.DryRun {
		created, _, err := s.store.importTickets(r.Context(), importRows(result))
		if err != nil {
			ticketError(w, err)
			return
		}
		result.Created = created
		// Every planned ticket that did not insert is reported skipped: keys
		// that already existed, plus completed work that never becomes a row.
		result.Skipped = len(result.Tickets) - created
		s.recordAudit(r, "import", fmt.Sprintf("%s/%s: created %d, skipped %d", result.Source, result.Project, created, result.Skipped))
	}
	writeJSON(w, http.StatusOK, result)
}

// importRows turns the planned tickets into table rows for the insert.
func importRows(result importResult) []Ticket {
	rows := make([]Ticket, 0, len(result.Tickets))
	for _, ticket := range result.Tickets {
		// Completed states have no ticket state and never become rows; every
		// other planned ticket is attempted, so a key that appears between
		// planning and insert is still counted skipped instead of failing.
		if ticket.State == "" {
			continue
		}
		rows = append(rows, Ticket{
			Key: ticket.Key, Title: ticket.Title, Description: ticket.Description,
			Repository: result.sourceRepository, Project: result.Project, State: ticket.State,
		})
	}
	return rows
}

// planImport resolves the source and project against the configured issue
// sources and returns either the remote project roster (empty project) or the
// planned tickets (project given). No ticket rows are written.
func (s *Server) planImport(ctx context.Context, definition config.Config, source, project string) (importResult, error) {
	result := importResult{Source: source, Project: project}
	switch source {
	case importSourcePlane:
		connections := definition.IssueSources.PlaneProjects()
		if project == "" {
			projects, err := planeImportProjects(ctx, connections)
			if err != nil {
				return result, err
			}
			result.Projects = projects
			return result, nil
		}
		repository, name, client, err := resolvePlaneImportProject(ctx, connections, project)
		if err != nil {
			return result, err
		}
		result.Project, result.sourceRepository = name, repository
		listed, err := client.IssuesInProject(ctx)
		if err != nil {
			return result, fmt.Errorf("list plane issues: %w", err)
		}
		tickets, err := s.planTickets(ctx, listed)
		result.Tickets = tickets
		return result, err
	case importSourceLinear:
		connections := definition.IssueSources.LinearProjects()
		if project == "" {
			projects, err := linearImportProjects(ctx, connections)
			if err != nil {
				return result, err
			}
			result.Projects = projects
			return result, nil
		}
		repository, client, err := resolveLinearImportProject(connections, project)
		if err != nil {
			return result, err
		}
		// The configured spelling of the project names the issue query, so a
		// case difference in the request cannot silently import a lookalike.
		result.Project, result.sourceRepository = client.Project(), repository
		listed, err := client.IssuesInProject(ctx, client.Project())
		if err != nil {
			return result, fmt.Errorf("list linear issues: %w", err)
		}
		tickets, err := s.planTickets(ctx, listed)
		result.Tickets = tickets
		return result, err
	default:
		return result, fmt.Errorf("%w: unknown source %q: want %q or %q", errInvalidImport, source, importSourcePlane, importSourceLinear)
	}
}

// planTickets maps remote issues onto the tickets table and marks which keys
// already exist, so the preview and the import agree row for row.
func (s *Server) planTickets(ctx context.Context, listed []issues.IssueWithState) ([]importTicket, error) {
	keys := make([]string, 0, len(listed))
	for _, issue := range listed {
		keys = append(keys, issue.Key)
	}
	existing, err := s.store.existingTicketKeys(ctx, keys)
	if err != nil {
		return nil, err
	}
	tickets := make([]importTicket, 0, len(listed))
	for _, issue := range listed {
		state, open := mapImportState(issue.StateType)
		ticket := importTicket{
			Key: issue.Key, Title: issues.Title(issue.Key, issue.Title),
			State: state, Description: issue.Description,
		}
		if len(ticket.Title) > importMaxTitleLength {
			ticket.Title = ticket.Title[:importMaxTitleLength]
		}
		if len(ticket.Description) > importMaxDescriptionLength {
			ticket.Description = ticket.Description[:importMaxDescriptionLength]
		}
		switch {
		case !open:
			ticket.Action, ticket.Reason = importActionSkip, importReasonCompleted
		case existing[issue.Key]:
			ticket.Action, ticket.Reason = importActionSkip, importReasonExists
		default:
			ticket.Action = importActionCreate
		}
		tickets = append(tickets, ticket)
	}
	return tickets, nil
}

// planeImportWorkspace builds the workspace client the project roster comes
// from. Every configured Plane connection shares the workspace credentials,
// which the config layer loads from the environment.
func planeImportWorkspace(connections map[string]issues.PlaneConnection) (*issues.PlaneClient, error) {
	if len(connections) == 0 {
		return nil, fmt.Errorf("%w: no Plane projects are configured in [issue_sources.plane]", errInvalidImport)
	}
	for _, repository := range sortedKeys(connections) {
		connection := connections[repository]
		workspace, err := issues.NewPlaneWorkspaceClient(issues.PlaneConfig{URL: connection.URL, Workspace: connection.Workspace, APIKey: connection.APIKey})
		if err == nil {
			return workspace, nil
		}
	}
	return nil, fmt.Errorf("%w: configure PLANE_URL, PLANE_WORKSPACE and PLANE_API_KEY", errInvalidImport)
}

// planeImportProjects lists the workspace's projects and marks the ones a
// configured issue source covers.
func planeImportProjects(ctx context.Context, connections map[string]issues.PlaneConnection) ([]importProject, error) {
	workspace, err := planeImportWorkspace(connections)
	if err != nil {
		return nil, err
	}
	remote, err := workspace.Projects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list plane projects: %w", err)
	}
	byID := make(map[string]string, len(connections))
	for repository, connection := range connections {
		byID[connection.ProjectID] = repository
	}
	projects := make([]importProject, 0, len(remote))
	for _, project := range remote {
		projects = append(projects, importProject{ID: project.ID, Name: project.Name, Identifier: project.Identifier, Repository: byID[project.ID]})
	}
	return projects, nil
}

// resolvePlaneImportProject matches a remote project name to its configured
// connection, so imports only ever touch projects Machinist is set up for.
func resolvePlaneImportProject(ctx context.Context, connections map[string]issues.PlaneConnection, name string) (string, string, *issues.PlaneClient, error) {
	workspace, err := planeImportWorkspace(connections)
	if err != nil {
		return "", "", nil, err
	}
	remote, err := workspace.Projects(ctx)
	if err != nil {
		return "", "", nil, fmt.Errorf("list plane projects: %w", err)
	}
	var matched issues.PlaneRemoteProject
	for _, project := range remote {
		if strings.EqualFold(project.Name, name) {
			matched = project
			break
		}
	}
	if matched.ID == "" {
		return "", "", nil, fmt.Errorf("%w: no Plane project named %q", errInvalidImport, name)
	}
	for _, repository := range sortedKeys(connections) {
		if connections[repository].ProjectID != matched.ID {
			continue
		}
		client, err := newPlaneImportClient(connections[repository])
		if err != nil {
			return "", "", nil, err
		}
		return repository, matched.Name, client, nil
	}
	return "", "", nil, fmt.Errorf("%w: Plane project %q is not configured in [issue_sources.plane]", errInvalidImport, name)
}

// linearImportClient builds the client used for the project roster; the query
// needs only the API key.
func linearImportClient(connections map[string]issues.LinearConnection) (*issues.LinearClient, error) {
	if len(connections) == 0 {
		return nil, fmt.Errorf("%w: no Linear projects are configured in [issue_sources.linear]", errInvalidImport)
	}
	for _, repository := range sortedKeys(connections) {
		client, err := newLinearImportClient(connections[repository])
		if err == nil {
			return client, nil
		}
	}
	return nil, fmt.Errorf("%w: configure LINEAR_API_KEY", errInvalidImport)
}

// linearImportProjects lists Linear's projects and marks the ones a
// configured issue source covers.
func linearImportProjects(ctx context.Context, connections map[string]issues.LinearConnection) ([]importProject, error) {
	client, err := linearImportClient(connections)
	if err != nil {
		return nil, err
	}
	remote, err := client.Projects(ctx)
	if err != nil {
		return nil, fmt.Errorf("list linear projects: %w", err)
	}
	byName := make(map[string]string, len(connections))
	for repository, connection := range connections {
		byName[strings.ToLower(connection.Project)] = repository
	}
	projects := make([]importProject, 0, len(remote))
	for _, project := range remote {
		projects = append(projects, importProject{ID: project.ID, Name: project.Name, Repository: byName[strings.ToLower(project.Name)]})
	}
	return projects, nil
}

// resolveLinearImportProject matches a remote project name to its configured
// connection. Configurations without a project name are skipped: importing
// them would sweep up the whole workspace.
func resolveLinearImportProject(connections map[string]issues.LinearConnection, name string) (string, *issues.LinearClient, error) {
	for _, repository := range sortedKeys(connections) {
		connection := connections[repository]
		if connection.Project == "" || !strings.EqualFold(connection.Project, name) {
			continue
		}
		client, err := newLinearImportClient(connection)
		if err != nil {
			return "", nil, err
		}
		return repository, client, nil
	}
	return "", nil, fmt.Errorf("%w: no configured Linear issue source covers project %q", errInvalidImport, name)
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// existingTicketKeys reports which of keys already exist in the tickets
// table. The chunked IN query keeps well under SQLite's parameter limit.
func (s *Store) existingTicketKeys(ctx context.Context, keys []string) (map[string]bool, error) {
	existing := map[string]bool{}
	const chunk = 500
	for start := 0; start < len(keys); start += chunk {
		end := start + chunk
		if end > len(keys) {
			end = len(keys)
		}
		args := make([]any, 0, end-start)
		placeholders := make([]string, 0, end-start)
		for _, key := range keys[start:end] {
			args = append(args, key)
			placeholders = append(placeholders, "?")
		}
		rows, err := s.db.QueryContext(ctx, `SELECT key FROM tickets WHERE key IN (`+strings.Join(placeholders, ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return nil, err
			}
			existing[key] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return existing, nil
}

// importTickets inserts external tickets under their external keys. Keys that
// already exist are skipped, so repeated imports of the same project are
// idempotent and report how many rows they added.
func (s *Store) importTickets(ctx context.Context, tickets []Ticket) (created, skipped int, err error) {
	if len(tickets) == 0 {
		return 0, 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	now := s.now().UTC().Format(time.RFC3339Nano)
	for _, ticket := range tickets {
		result, err := tx.ExecContext(ctx,
			`INSERT INTO tickets(key,title,description,repository,project,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(key) DO NOTHING`,
			ticket.Key, ticket.Title, ticket.Description, ticket.Repository, ticket.Project, ticket.State, now, now)
		if err != nil {
			return 0, 0, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		if affected == 0 {
			skipped++
		} else {
			created++
		}
	}
	return created, skipped, tx.Commit()
}
