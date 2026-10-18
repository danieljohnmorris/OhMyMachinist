package issues

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type PlaneConfig struct {
	URL       string
	Workspace string
	ProjectID string
	APIKey    string
}

type PlaneClient struct {
	config PlaneConfig
	client *http.Client
}

func NewPlaneClient(config PlaneConfig) (*PlaneClient, error) {
	if err := requiresValue(config.URL, "PLANE_URL"); err != nil {
		return nil, err
	}
	if err := requiresValue(config.Workspace, "PLANE_WORKSPACE"); err != nil {
		return nil, err
	}
	if err := requiresValue(config.ProjectID, "project ID"); err != nil {
		return nil, err
	}
	if err := requiresValue(config.APIKey, "PLANE_API_KEY"); err != nil {
		return nil, err
	}
	return &PlaneClient{config: config, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (p *PlaneClient) FetchIssue(ctx context.Context, key string) (Issue, error) {
	if err := requiresValue(key, "issue key"); err != nil {
		return Issue{}, err
	}
	var response struct {
		ID          string       `json:"id"`
		Name        string       `json:"name"`
		Description string       `json:"description"`
		URL         string       `json:"url"`
		Labels      []planeLabel `json:"labels"`
	}
	if err := p.request(ctx, http.MethodGet, p.endpoint("issues/", key, "/"), nil, &response); err != nil {
		return Issue{}, err
	}
	return Issue{
		ID:          firstNonEmpty(response.ID, key),
		Key:         firstNonEmpty(response.ID, key),
		Title:       response.Name,
		Description: response.Description,
		URL:         firstNonEmpty(response.URL, p.IssueURL(firstNonEmpty(response.ID, key))),
		Labels:      planeLabelNames(response.Labels),
	}, nil
}

// IssuesWithLabel lists the project's issues carrying label that are still
// open. Issues in Plane's completed or cancelled state groups are skipped, so
// a delivered ticket that keeps its label never admits new work. Keys are the
// human ticket form (MACH-5) whenever Plane reports the sequence number.
func (p *PlaneClient) IssuesWithLabel(ctx context.Context, label string) ([]Issue, error) {
	if err := requiresValue(label, "label"); err != nil {
		return nil, err
	}
	query := url.Values{"labels": []string{label}, "expand": []string{"labels,state"}}
	var response struct {
		Results []struct {
			ID          string          `json:"id"`
			SequenceID  int64           `json:"sequence_id"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			URL         string          `json:"url"`
			Labels      []planeLabel    `json:"labels"`
			State       json.RawMessage `json:"state"`
		} `json:"results"`
	}
	if err := p.request(ctx, http.MethodGet, p.endpoint("issues/?", query.Encode()), nil, &response); err != nil {
		return nil, err
	}
	var identifier string
	var states map[string]planeState
	issues := make([]Issue, 0, len(response.Results))
	for _, result := range response.Results {
		closed, err := p.closedPlaneState(ctx, result.State, &states)
		if err != nil {
			return nil, fmt.Errorf("plane issue %s state: %w", result.ID, err)
		}
		if closed {
			continue
		}
		key, issueURL := result.ID, firstNonEmpty(result.URL, p.IssueURL(result.ID))
		if result.SequenceID > 0 {
			if identifier == "" {
				if identifier, err = p.projectIdentifier(ctx); err != nil {
					return nil, err
				}
			}
			key = identifier + "-" + strconv.FormatInt(result.SequenceID, 10)
			issueURL = firstNonEmpty(result.URL, p.BrowseURL(key))
		}
		issues = append(issues, Issue{
			ID: result.ID, Key: key, Title: result.Name, Description: result.Description,
			URL: issueURL, Labels: planeLabelNames(result.Labels),
		})
	}
	return issues, nil
}

// closedPlaneState reports whether a listed issue's state sits in the
// completed or cancelled group. An unexpanded state is a UUID, resolved
// through the project's state list, fetched once per poll into states.
func (p *PlaneClient) closedPlaneState(ctx context.Context, raw json.RawMessage, states *map[string]planeState) (bool, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return false, nil
	}
	var state planeState
	if err := json.Unmarshal(raw, &state); err != nil {
		var id string
		if err := json.Unmarshal(raw, &id); err != nil || id == "" {
			return false, nil
		}
		if *states == nil {
			list, err := p.projectStates(ctx, p.config.ProjectID)
			if err != nil {
				return false, err
			}
			*states = make(map[string]planeState, len(list))
			for _, listed := range list {
				(*states)[listed.ID] = listed
			}
		}
		state = (*states)[id]
	}
	if state.Group != "" {
		return strings.EqualFold(state.Group, "completed") || strings.EqualFold(state.Group, "cancelled"), nil
	}
	return strings.EqualFold(state.Name, CompletedPlaneState), nil
}

// projectIdentifier is the project's ticket prefix (MACH in MACH-5).
func (p *PlaneClient) projectIdentifier(ctx context.Context) (string, error) {
	var project struct {
		Identifier string `json:"identifier"`
	}
	if err := p.request(ctx, http.MethodGet, p.endpoint(), nil, &project); err != nil {
		return "", fmt.Errorf("plane project identifier: %w", err)
	}
	if project.Identifier == "" {
		return "", fmt.Errorf("plane project %s has no identifier", p.config.ProjectID)
	}
	return project.Identifier, nil
}

func (p *PlaneClient) PostComment(ctx context.Context, key, body string) error {
	if err := requiresValue(key, "issue key"); err != nil {
		return err
	}
	if err := requiresBody(body); err != nil {
		return err
	}
	if ref, ok, err := p.resolveKey(ctx, key); ok {
		if err != nil {
			return err
		}
		return p.keyedComment(ctx, ref, body)
	}
	payload, _ := json.Marshal(map[string]any{"issue": key, "comment_html": body})
	return p.request(ctx, http.MethodPost, p.endpoint("issues/", key, "/comments/"), payload, nil)
}

func (p *PlaneClient) SetState(ctx context.Context, key, state string) error {
	if err := requiresValue(key, "issue key"); err != nil {
		return err
	}
	if err := requiresValue(state, "state"); err != nil {
		return err
	}
	if ref, ok, err := p.resolveKey(ctx, key); ok {
		if err != nil {
			return err
		}
		return p.keyedSetState(ctx, ref, state)
	}
	payload, _ := json.Marshal(map[string]string{"state": state})
	return p.request(ctx, http.MethodPatch, p.endpoint("issues/", key, "/"), payload, nil)
}

func (p *PlaneClient) IssueURL(key string) string { return p.endpoint("issues/", key, "/") }

// PlaneIssueState is the state an issue currently sits in. Plane groups states
// into triage, backlog, started, completed and cancelled.
type PlaneIssueState struct {
	Name  string
	Group string
}

// IssueState fetches the current state of an issue by key.
func (p *PlaneClient) IssueState(ctx context.Context, key string) (PlaneIssueState, error) {
	if err := requiresValue(key, "issue key"); err != nil {
		return PlaneIssueState{}, err
	}
	if ref, ok, err := p.resolveKey(ctx, key); ok {
		if err != nil {
			return PlaneIssueState{}, err
		}
		return p.keyedIssueState(ctx, ref)
	}
	var response struct {
		State     json.RawMessage `json:"state"`
		StateName string          `json:"state_name"`
	}
	if err := p.request(ctx, http.MethodGet, p.endpoint("issues/", key, "/"), nil, &response); err != nil {
		return PlaneIssueState{}, err
	}
	state := PlaneIssueState{Name: response.StateName}
	if len(response.State) > 0 {
		var named struct {
			Name  string `json:"name"`
			Group string `json:"group"`
		}
		if err := json.Unmarshal(response.State, &named); err == nil {
			state.Name = firstNonEmpty(named.Name, state.Name)
			state.Group = named.Group
		} else {
			var name string
			if err := json.Unmarshal(response.State, &name); err == nil {
				state.Name = firstNonEmpty(name, state.Name)
			}
		}
	}
	if state.Name == "" && state.Group == "" {
		return PlaneIssueState{}, fmt.Errorf("plane issue %s has no state", key)
	}
	return state, nil
}

// IsCompletedPlaneState reports whether an issue state sits in Plane's
// completed group (Done) or is literally named Done.
func IsCompletedPlaneState(state PlaneIssueState) bool {
	if strings.EqualFold(state.Group, "completed") {
		return true
	}
	return strings.EqualFold(state.Name, "done")
}

// CompletedPlaneState is the state name Machinist moves delivered tickets to.
const CompletedPlaneState = "Done"

func (p *PlaneClient) endpoint(parts ...string) string {
	base := strings.TrimRight(p.config.URL, "/")
	base += "/api/v1/workspaces/" + url.PathEscape(p.config.Workspace) + "/projects/" + url.PathEscape(p.config.ProjectID) + "/"
	for _, part := range parts {
		if part != "" {
			base += part
		}
	}
	return base
}

// request performs one Plane API call. HTTP 429 responses are retried after
// the X-RateLimit-Reset time (capped) so bulk reconciles ride out Plane's
// per-key rate limit instead of failing.
func (p *PlaneClient) request(ctx context.Context, method, endpoint string, body []byte, output any) error {
	for attempt := 0; ; attempt++ {
		wait, err := p.requestOnce(ctx, method, endpoint, body, output)
		if wait <= 0 || attempt >= planeRateLimitRetries {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

const planeRateLimitRetries = 3

// planeRetryAfter is how long to wait before retrying a rate-limited call.
var planeRetryAfter = func(response *http.Response) time.Duration {
	const fallback, ceiling = 15 * time.Second, 65 * time.Second
	reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return fallback
	}
	wait := time.Until(time.Unix(reset, 0))
	if wait > ceiling {
		return ceiling
	}
	if wait <= 0 {
		return time.Second
	}
	return wait + time.Second
}

func (p *PlaneClient) requestOnce(ctx context.Context, method, endpoint string, body []byte, output any) (time.Duration, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-API-Key", p.config.APIKey)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := p.client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return 0, err
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return planeRetryAfter(response), fmt.Errorf("plane request %s: HTTP %d: %s", method, response.StatusCode, strings.TrimSpace(string(payload)))
	}
	if response.StatusCode == http.StatusNotFound && method == http.MethodGet {
		return 0, fmt.Errorf("plane request %s: %w: HTTP 404: %s", method, ErrIssueNotFound, strings.TrimSpace(string(payload)))
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return 0, fmt.Errorf("plane request %s: HTTP %d: %s", method, response.StatusCode, strings.TrimSpace(string(payload)))
	}
	if output == nil {
		return 0, nil
	}
	return 0, json.Unmarshal(payload, output)
}

// PlaneRemoteProject is a remote Plane project.
type PlaneRemoteProject struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Identifier string `json:"identifier"`
}

// Projects lists the workspace's projects. The client should be a workspace
// client (NewPlaneWorkspaceClient); a project-bound client still reaches the
// workspace roster but cannot import unbound projects.
func (p *PlaneClient) Projects(ctx context.Context) ([]PlaneRemoteProject, error) {
	var response struct {
		Results []PlaneRemoteProject `json:"results"`
	}
	if err := p.request(ctx, http.MethodGet, p.workspaceEndpoint("projects/"), nil, &response); err != nil {
		return nil, err
	}
	return response.Results, nil
}

// planeIssueRow is one listed project issue with its state payload.
type planeIssueRow struct {
	ID          string          `json:"id"`
	SequenceID  int64           `json:"sequence_id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	URL         string          `json:"url"`
	State       json.RawMessage `json:"state"`
}

// IssuesInProject lists every issue of the client's project together with its
// state, including completed and cancelled issues so a bulk import can skip
// them explicitly. Keys are the human ticket form (MACH-5) whenever Plane
// reports the sequence number. Pages are followed until a page yields no new
// issues, so imports survive servers that ignore the page parameter.
func (p *PlaneClient) IssuesInProject(ctx context.Context) ([]IssueWithState, error) {
	var identifier string
	var result []IssueWithState
	seen := map[string]bool{}
	for page := 1; ; page++ {
		query := url.Values{"expand": []string{"state"}, "page": []string{strconv.Itoa(page)}}
		var response struct {
			Results []planeIssueRow `json:"results"`
		}
		if err := p.request(ctx, http.MethodGet, p.endpoint("issues/?", query.Encode()), nil, &response); err != nil {
			return nil, err
		}
		var fresh int
		for _, row := range response.Results {
			if seen[row.ID] {
				continue
			}
			seen[row.ID] = true
			fresh++
			key := row.ID
			if row.SequenceID > 0 {
				if identifier == "" {
					var err error
					if identifier, err = p.projectIdentifier(ctx); err != nil {
						return nil, err
					}
				}
				key = identifier + "-" + strconv.FormatInt(row.SequenceID, 10)
			}
			name, group, err := p.stateNameAndGroup(ctx, row.State)
			if err != nil {
				return nil, fmt.Errorf("plane issue %s state: %w", row.ID, err)
			}
			result = append(result, IssueWithState{
				Issue:     Issue{ID: row.ID, Key: key, Title: row.Name, Description: row.Description, URL: firstNonEmpty(row.URL, p.IssueURL(row.ID))},
				StateName: name, StateType: group,
			})
		}
		if fresh == 0 {
			return result, nil
		}
	}
}

// stateNameAndGroup decodes an issue's state payload. Expanded states are
// objects; unexpanded ones are UUIDs resolved through the project state list.
func (p *PlaneClient) stateNameAndGroup(ctx context.Context, raw json.RawMessage) (string, string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", "", nil
	}
	var state planeState
	if err := json.Unmarshal(raw, &state); err == nil && (state.Name != "" || state.Group != "") {
		return state.Name, state.Group, nil
	}
	var id string
	if err := json.Unmarshal(raw, &id); err != nil || id == "" {
		return "", "", nil
	}
	resolved, err := p.stateByID(ctx, p.config.ProjectID, id)
	if err != nil {
		return "", "", err
	}
	return resolved.Name, resolved.Group, nil
}

type planeLabel struct {
	Name string `json:"name"`
}

func planeLabelNames(labels []planeLabel) []string {
	names := make([]string, 0, len(labels))
	for _, label := range labels {
		if label.Name != "" {
			names = append(names, label.Name)
		}
	}
	return names
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
