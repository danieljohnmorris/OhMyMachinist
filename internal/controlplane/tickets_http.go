package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/owainlewis/machinist/internal/config"
)

func ticketError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, sql.ErrNoRows):
		status = http.StatusNotFound
	case errors.Is(err, errTicketInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, errTicketConflict):
		status = http.StatusConflict
	}
	writeError(w, status, err)
}
func (s *Server) trackerConfig(ctx context.Context) (config.ResolvedTracker, error) {
	c, err := config.LoadDefinitions(s.definitionPath)
	if err != nil {
		return config.ResolvedTracker{}, err
	}
	// Settings-defined projects (the Projects view) overlay the config file.
	if err := s.applyProjectOverrides(ctx, &c); err != nil {
		return config.ResolvedTracker{}, err
	}
	return c.ResolveTracker()
}
func (s *Server) listTicketsHandler(w http.ResponseWriter, r *http.Request) {
	tickets, err := s.store.tickets(r.Context())
	if err != nil {
		ticketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tickets": tickets})
}
func (s *Server) getTicketHandler(w http.ResponseWriter, r *http.Request) {
	ticket, err := s.store.ticket(r.Context(), r.PathValue("key"))
	if err != nil {
		ticketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ticket)
}
func (s *Server) createTicketHandler(w http.ResponseWriter, r *http.Request) {
	if !limitRequestBody(w, r, maxRequestBytes) {
		return
	}
	var t Ticket
	if err := decodeJSON(r, &t); err != nil {
		writeDecodeError(w, err)
		return
	}
	c, err := s.trackerConfig(r.Context())
	if err != nil {
		ticketError(w, err)
		return
	}
	// A named project scopes the ticket: it contributes its own key prefix and
	// defaults for repository, command, and model. Unknown projects are refused
	// rather than silently filed under the default prefix.
	if t.Project != "" {
		project, ok := c.Projects[strings.ToUpper(strings.TrimSpace(t.Project))]
		if !ok {
			ticketError(w, fmt.Errorf("%w: unknown project %q; add [tracker.projects.%s] or omit it", errTicketInvalid, t.Project, t.Project))
			return
		}
		t.Project = project.Name
		if t.Repository == "" {
			t.Repository = project.Repository
		}
		if t.Repository == "" {
			t.Repository = c.Repository
		}
	} else {
		if t.Repository == "" {
			t.Repository = c.Repository
		}
	}
	var prefix string
	if t.Project != "" {
		prefix = t.Project
	} else {
		prefix = c.KeyPrefix
	}
	t, err = s.store.createTicket(r.Context(), t, prefix)
	if err != nil {
		ticketError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, t)
}
func (s *Server) updateTicketHandler(w http.ResponseWriter, r *http.Request) {
	if !limitRequestBody(w, r, maxRequestBytes) {
		return
	}
	var fields map[string]string
	if err := decodeJSON(r, &fields); err != nil {
		writeDecodeError(w, err)
		return
	}
	t, err := s.store.updateTicket(r.Context(), r.PathValue("key"), fields)
	if err != nil {
		ticketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}
func (s *Server) deleteTicketHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.store.deleteTicket(r.Context(), r.PathValue("key")); err != nil {
		ticketError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) approveTicketHandler(w http.ResponseWriter, r *http.Request) {
	c, err := s.trackerConfig(r.Context())
	if err != nil {
		ticketError(w, err)
		return
	}
	ticket, err := s.store.ticket(r.Context(), r.PathValue("key"))
	if err != nil {
		ticketError(w, err)
		return
	}
	// A project-scoped ticket runs its project's command and model; the global
	// tracker command is the fallback.
	commandName, modelOverride := c.Command, c.Model
	if ticket.Project != "" {
		project, ok := c.Projects[ticket.Project]
		if !ok {
			ticketError(w, fmt.Errorf("%w: ticket project %q is no longer configured", errTicketInvalid, ticket.Project))
			return
		}
		if project.Command != "" {
			commandName = project.Command
		}
		if project.Model != "" {
			modelOverride = project.Model
		}
	}
	if commandName == "" {
		ticketError(w, fmt.Errorf("%w: configure tracker.command before approving tickets", errTicketInvalid))
		return
	}
	command, err := config.LoadCommand(s.definitionPath, commandName)
	if err != nil {
		ticketError(w, err)
		return
	}
	if modelOverride != "" {
		command.Model = modelOverride
	}
	id, created, err := s.store.approveTicket(r.Context(), r.PathValue("key"), command)
	if err != nil {
		ticketError(w, err)
		return
	}
	ticket, err = s.store.ticket(r.Context(), r.PathValue("key"))
	if err != nil {
		ticketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ticket": ticket, "job_id": id, "created": created})
}
func (s *Server) commentTicketHandler(w http.ResponseWriter, r *http.Request) {
	if !limitRequestBody(w, r, maxRequestBytes) {
		return
	}
	var input struct {
		Body string `json:"body"`
	}
	if err := decodeJSON(r, &input); err != nil {
		writeDecodeError(w, err)
		return
	}
	input.Body = strings.TrimSpace(input.Body)
	if input.Body == "" || len(input.Body) > 64<<10 {
		ticketError(w, fmt.Errorf("%w: comment must contain 1–65536 bytes", errTicketInvalid))
		return
	}
	key := r.PathValue("key")
	if _, err := s.store.ticket(r.Context(), key); err != nil {
		ticketError(w, err)
		return
	}
	_, err := s.store.db.ExecContext(r.Context(), `INSERT INTO ticket_comments(ticket_key,body,created_at) VALUES(?,?,?)`, key, input.Body, s.now().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"))
	if err != nil {
		ticketError(w, err)
		return
	}
	s.getTicketHandler(w, r)
}
func (s *Server) trackersHandler(w http.ResponseWriter, r *http.Request) {
	c, err := s.trackerConfig(r.Context())
	if err != nil {
		ticketError(w, err)
		return
	}
	definition, err := config.LoadDefinitions(s.definitionPath)
	if err != nil {
		ticketError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"default": c.Default, "projects": c.Projects, "trackers": []map[string]any{
		{"id": "builtin", "name": "Built-in", "configured": true},
		{"id": "plane", "name": "Plane", "configured": len(definition.IssueSources.Plane) > 0 || len(s.planeClients) > 0},
		{"id": "linear", "name": "Linear", "configured": len(definition.IssueSources.Linear) > 0},
	}})
}
