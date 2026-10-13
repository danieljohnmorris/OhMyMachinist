package issues

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Plane's project-scoped issue endpoints take issue UUIDs, not human keys
// such as MACH-5. Keys are resolved through the workspace-level endpoint,
// which also yields the owning project, so one client can serve tickets from
// every project in the workspace.

var planeTicketKey = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}-[0-9]+$`)

var planeUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// NewPlaneWorkspaceClient returns a client that is not bound to one project.
// It can resolve, read, transition and comment on tickets by key across the
// workspace.
func NewPlaneWorkspaceClient(config PlaneConfig) (*PlaneClient, error) {
	if err := requiresValue(config.URL, "PLANE_URL"); err != nil {
		return nil, err
	}
	if err := requiresValue(config.Workspace, "PLANE_WORKSPACE"); err != nil {
		return nil, err
	}
	if err := requiresValue(config.APIKey, "PLANE_API_KEY"); err != nil {
		return nil, err
	}
	return &PlaneClient{config: config, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

type planeIssueRef struct {
	ID      string          `json:"id"`
	Project string          `json:"project"`
	State   json.RawMessage `json:"state"`
}

func (p *PlaneClient) workspaceEndpoint(parts ...string) string {
	base := strings.TrimRight(p.config.URL, "/") + "/api/v1/workspaces/" + url.PathEscape(p.config.Workspace) + "/"
	return base + strings.Join(parts, "")
}

func (p *PlaneClient) projectEndpoint(project string, parts ...string) string {
	return p.workspaceEndpoint("projects/", url.PathEscape(project), "/") + strings.Join(parts, "")
}

// resolveKey looks a ticket key (MACH-5) up through the workspace endpoint.
// ok is false when key is not in ticket form; callers then fall back to the
// project-scoped legacy path.
func (p *PlaneClient) resolveKey(ctx context.Context, key string) (planeIssueRef, bool, error) {
	if !planeTicketKey.MatchString(key) {
		return planeIssueRef{}, false, nil
	}
	var ref planeIssueRef
	if err := p.request(ctx, http.MethodGet, p.workspaceEndpoint("issues/", url.PathEscape(key), "/"), nil, &ref); err != nil {
		return planeIssueRef{}, true, fmt.Errorf("resolve plane issue %s: %w", key, err)
	}
	if ref.ID == "" || ref.Project == "" {
		return planeIssueRef{}, true, fmt.Errorf("resolve plane issue %s: missing id or project", key)
	}
	return ref, true, nil
}

type planeState struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Group string `json:"group"`
}

func (p *PlaneClient) projectStates(ctx context.Context, project string) ([]planeState, error) {
	var response struct {
		Results []planeState `json:"results"`
	}
	if err := p.request(ctx, http.MethodGet, p.projectEndpoint(project, "states/"), nil, &response); err != nil {
		return nil, err
	}
	return response.Results, nil
}

// stateID maps a state name to its UUID in project. The completed-state name
// also matches any state in the completed group.
func (p *PlaneClient) stateID(ctx context.Context, project, name string) (string, error) {
	if planeUUID.MatchString(name) {
		return name, nil
	}
	states, err := p.projectStates(ctx, project)
	if err != nil {
		return "", fmt.Errorf("list plane states: %w", err)
	}
	for _, state := range states {
		if strings.EqualFold(state.Name, name) {
			return state.ID, nil
		}
	}
	if strings.EqualFold(name, CompletedPlaneState) {
		for _, state := range states {
			if state.Group == "completed" {
				return state.ID, nil
			}
		}
	}
	return "", fmt.Errorf("plane project has no state %q", name)
}

func (p *PlaneClient) stateByID(ctx context.Context, project, id string) (PlaneIssueState, error) {
	var state planeState
	if err := p.request(ctx, http.MethodGet, p.projectEndpoint(project, "states/", url.PathEscape(id), "/"), nil, &state); err != nil {
		return PlaneIssueState{}, err
	}
	return PlaneIssueState{Name: state.Name, Group: state.Group}, nil
}

func (p *PlaneClient) keyedIssueState(ctx context.Context, ref planeIssueRef) (PlaneIssueState, error) {
	var expanded planeState
	if err := json.Unmarshal(ref.State, &expanded); err == nil && (expanded.Name != "" || expanded.Group != "") {
		return PlaneIssueState{Name: expanded.Name, Group: expanded.Group}, nil
	}
	var id string
	if err := json.Unmarshal(ref.State, &id); err != nil || id == "" {
		return PlaneIssueState{}, fmt.Errorf("plane issue %s has no state", ref.ID)
	}
	return p.stateByID(ctx, ref.Project, id)
}

func (p *PlaneClient) keyedSetState(ctx context.Context, ref planeIssueRef, state string) error {
	id, err := p.stateID(ctx, ref.Project, state)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]string{"state": id})
	return p.request(ctx, http.MethodPatch, p.projectEndpoint(ref.Project, "issues/", url.PathEscape(ref.ID), "/"), payload, nil)
}

func (p *PlaneClient) keyedComment(ctx context.Context, ref planeIssueRef, body string) error {
	payload, _ := json.Marshal(map[string]any{"comment_html": body})
	return p.request(ctx, http.MethodPost, p.projectEndpoint(ref.Project, "issues/", url.PathEscape(ref.ID), "/comments/"), payload, nil)
}

// BrowseURL is the human-facing ticket URL (as linked from PRs).
func (p *PlaneClient) BrowseURL(key string) string {
	return strings.TrimRight(p.config.URL, "/") + "/" + url.PathEscape(p.config.Workspace) + "/browse/" + url.PathEscape(key) + "/"
}
