package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/issues"
)

// linearWebhookMaxAge bounds how stale a signed Linear delivery may be, so a
// captured request cannot be replayed later.
const linearWebhookMaxAge = 5 * time.Minute

// ticketCompletion is the ticket state carried by a tracker webhook.
type ticketCompletion struct {
	Key       string
	StateName string
	Completed bool
}

type completionIssue struct {
	Identifier string          `json:"identifier"`
	Key        string          `json:"key"`
	State      json.RawMessage `json:"state"`
	StateName  string          `json:"state_name"`
}

type completionEnvelope struct {
	Event            string          `json:"event"`
	Type             string          `json:"type"`
	Action           string          `json:"action"`
	Issue            completionIssue `json:"issue"`
	Data             json.RawMessage `json:"data"`
	WebhookTimestamp int64           `json:"webhookTimestamp"`
}

// parseTicketCompletion extracts the ticket key and state from a Plane or
// Linear issue webhook. State-only updates are exactly what resolution cares
// about, so unlike intake parsing they are not treated as no-ops. Payloads
// that are not issue updates, or carry no ticket key, report an empty Key.
func parseTicketCompletion(tracker string, body []byte) (ticketCompletion, int64, error) {
	var envelope completionEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ticketCompletion{}, 0, fmt.Errorf("parse issue webhook: %w", err)
	}
	if strings.EqualFold(envelope.Action, "remove") || strings.EqualFold(envelope.Action, "delete") ||
		(envelope.Type != "" && !strings.EqualFold(envelope.Type, "issue")) ||
		(envelope.Event != "" && !strings.EqualFold(envelope.Event, "issue")) {
		return ticketCompletion{}, envelope.WebhookTimestamp, nil
	}
	payload := envelope.Issue
	if payload.Identifier == "" && payload.Key == "" && len(envelope.Data) > 0 {
		var nested struct {
			Issue completionIssue `json:"issue"`
		}
		if err := json.Unmarshal(envelope.Data, &nested); err == nil && (nested.Issue.Identifier != "" || nested.Issue.Key != "") {
			payload = nested.Issue
		} else if err := json.Unmarshal(envelope.Data, &payload); err != nil {
			return ticketCompletion{}, 0, fmt.Errorf("parse issue webhook data: %w", err)
		}
	}
	key := strings.ToUpper(strings.TrimSpace(firstNonEmptyString(payload.Identifier, payload.Key)))
	if ticketKeyPattern.FindString(key) != key {
		key = ""
	}
	var state struct {
		Name  string `json:"name"`
		Group string `json:"group"`
		Type  string `json:"type"`
	}
	if len(payload.State) > 0 && json.Unmarshal(payload.State, &state) != nil {
		var name string
		if json.Unmarshal(payload.State, &name) == nil {
			state.Name = name
		}
	}
	completion := ticketCompletion{Key: key, StateName: firstNonEmptyString(state.Name, payload.StateName)}
	if tracker == trackerLinear {
		completion.Completed = issues.IsCompletedLinearState(issues.LinearIssueState{Name: completion.StateName, Type: state.Type})
	} else {
		completion.Completed = issues.IsCompletedPlaneState(issues.PlaneIssueState{Name: completion.StateName, Group: state.Group})
	}
	return completion, envelope.WebhookTimestamp, nil
}

// sourceCompletion re-reads the ticket through the group's own tracker source
// and reports whether it is completed there, with the resolution detail. A
// ticket the source does not know belongs to another workspace or project
// and counts as not completed.
func (s *Server) sourceCompletion(ctx context.Context, group ticketGroup) (string, bool, error) {
	switch {
	case group.tracker == trackerLinear && group.linear != nil:
		state, err := group.linear.IssueState(ctx, group.key)
		if errors.Is(err, issues.ErrIssueNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("read ticket %s state: %w", group.key, err)
		}
		return "Linear " + firstNonEmptyString(state.Name, issues.CompletedLinearState), issues.IsCompletedLinearState(state), nil
	case group.tracker == trackerPlane && group.plane != nil:
		state, err := group.plane.IssueState(ctx, group.key)
		if errors.Is(err, issues.ErrIssueNotFound) {
			return "", false, nil
		}
		if err != nil {
			return "", false, fmt.Errorf("read ticket %s state: %w", group.key, err)
		}
		return "Plane " + firstNonEmptyString(state.Name, issues.CompletedPlaneState), issues.IsCompletedPlaneState(state), nil
	}
	return "", false, nil
}

// resolveCompletedTicket resolves the finished jobs of one ticket after a
// tracker reported it completed. The webhook secret is shared by every
// source and payloads carry no reliable source identity, so the delivery is
// only a hint: candidate jobs must carry exactly this key and tracker, are
// grouped by the source that owns them, and each group resolves only when
// its own source confirms the ticket is completed. A Plane or Linear MACH-5
// therefore never resolves a builtin MACH-5, MACH-51, or the MACH-5 of
// another workspace or project. Jobs whose source has no client cannot be
// confirmed and are left to reconciliation.
func (s *Server) resolveCompletedTicket(ctx context.Context, tracker, key string) (int, error) {
	jobs, err := s.store.listJobs(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("list jobs for resolution: %w", err)
	}
	var order []string
	groups := map[string]*ticketGroup{}
	for _, job := range jobs {
		if !terminalJobState(job.State) || job.State == "resolved" || job.ResolutionKind != "" {
			continue
		}
		group, ok := s.ticketGroupFor(job)
		if !ok || group.tracker != tracker || !strings.EqualFold(group.key, key) {
			continue
		}
		existing, seen := groups[group.source]
		if !seen {
			order = append(order, group.source)
			existing = &group
			groups[group.source] = existing
		}
		existing.jobs = append(existing.jobs, job)
	}
	resolved := 0
	var failures []error
	for _, source := range order {
		group := groups[source]
		detail, completed, err := s.sourceCompletion(ctx, *group)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !completed {
			continue
		}
		for _, job := range group.jobs {
			if err := s.store.MarkJobResolved(ctx, job.ID, tracker, detail, s.now().UTC()); err != nil {
				failures = append(failures, err)
				continue
			}
			resolved++
		}
	}
	return resolved, errors.Join(failures...)
}

// issueWebhook accepts Plane and Linear issue events. A ticket moving into a
// completed state is a hint: resolveCompletedTicket confirms it through each
// owning source before resolving that source's finished jobs. Plane intake
// deliveries (X-Plane-Signature) go to the intake. Linear deliveries carry
// Linear-Signature, other Plane deliveries X-Signature; both are HMAC-SHA256
// with the issue webhook secret, and Linear's webhookTimestamp must be within
// five minutes.
func (s *Server) issueWebhook(response http.ResponseWriter, request *http.Request) {
	if s.planeIntake != nil && request.Header.Get("X-Plane-Signature") != "" {
		s.planeWebhook(response, request)
		return
	}
	if s.webhookSecret == "" {
		writeError(response, http.StatusServiceUnavailable, errors.New("issue webhook secret is not configured"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(response, request.Body, maxRequestBytes))
	if err != nil {
		writeError(response, http.StatusBadRequest, errors.New("read issue webhook body"))
		return
	}
	tracker, signature := trackerPlane, request.Header.Get("X-Signature")
	if linearSignature := request.Header.Get("Linear-Signature"); linearSignature != "" {
		tracker, signature = trackerLinear, linearSignature
	}
	if !issues.VerifyHMAC(s.webhookSecret, body, signature) {
		writeUnauthorized(response)
		return
	}
	completion, timestamp, err := parseTicketCompletion(tracker, body)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if tracker == trackerLinear {
		age := s.now().Sub(time.UnixMilli(timestamp))
		if timestamp <= 0 || age > linearWebhookMaxAge || age < -linearWebhookMaxAge {
			writeUnauthorized(response)
			return
		}
	}
	if completion.Key == "" || !completion.Completed {
		response.WriteHeader(http.StatusAccepted)
		return
	}
	resolved, err := s.resolveCompletedTicket(request.Context(), tracker, completion.Key)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if resolved > 0 {
		log.Printf("issue webhook: %s ticket %s completed, resolved %d job(s)", tracker, completion.Key, resolved)
	}
	response.WriteHeader(http.StatusAccepted)
}
