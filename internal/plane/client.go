package plane

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ClientConfig carries the Plane workspace connection settings.
type ClientConfig struct {
	URL       string
	Workspace string
	APIKey    string
}

// Client talks to the Plane REST API for the operations intake needs:
// project/label/state discovery, issue paging, and issue patches and comments.
type Client struct {
	config ClientConfig
	http   *http.Client
}

// NewClient validates the connection settings and returns a Plane client.
func NewClient(config ClientConfig) (*Client, error) {
	if strings.TrimSpace(config.URL) == "" {
		return nil, fmt.Errorf("plane: PLANE_URL is required")
	}
	if strings.TrimSpace(config.Workspace) == "" {
		return nil, fmt.Errorf("plane: PLANE_WORKSPACE is required")
	}
	if strings.TrimSpace(config.APIKey) == "" {
		return nil, fmt.Errorf("plane: PLANE_API_KEY is required")
	}
	return &Client{config: config, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

// Project is a Plane project.
type Project struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Name       string `json:"name"`
}

// State is a Plane workflow state.
type State struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Group string `json:"group"`
}

// IssuePage is one page of project issues.
type IssuePage struct {
	Results    []Issue
	NextCursor string
	Next       bool
}

func (c *Client) endpoint(parts ...string) string {
	base := strings.TrimRight(c.config.URL, "/") + "/api/v1/workspaces/" + url.PathEscape(c.config.Workspace) + "/"
	return base + strings.Join(parts, "")
}

func (c *Client) projectEndpoint(projectID string, parts ...string) string {
	return c.endpoint("projects/", url.PathEscape(projectID), "/") + strings.Join(parts, "")
}

// Projects lists every project in the workspace.
func (c *Client) Projects(ctx context.Context) ([]Project, error) {
	var response struct {
		Results []Project `json:"results"`
	}
	if err := c.request(ctx, http.MethodGet, c.endpoint("projects/"), nil, &response); err != nil {
		return nil, err
	}
	return response.Results, nil
}

// Labels lists a project's labels.
func (c *Client) Labels(ctx context.Context, projectID string) ([]Label, error) {
	var response struct {
		Results []Label `json:"results"`
	}
	if err := c.request(ctx, http.MethodGet, c.projectEndpoint(projectID, "labels/"), nil, &response); err != nil {
		return nil, err
	}
	return response.Results, nil
}

// States lists a project's workflow states.
func (c *Client) States(ctx context.Context, projectID string) ([]State, error) {
	var response struct {
		Results []State `json:"results"`
	}
	if err := c.request(ctx, http.MethodGet, c.projectEndpoint(projectID, "states/"), nil, &response); err != nil {
		return nil, err
	}
	return response.Results, nil
}

// Issues pages a project's issues with labels expanded. cursor is empty for
// the first page.
func (c *Client) Issues(ctx context.Context, projectID, cursor string) (IssuePage, error) {
	query := url.Values{"expand": []string{"labels"}, "per_page": []string{"100"}}
	if cursor != "" {
		query.Set("cursor", cursor)
	}
	var response struct {
		Results         []json.RawMessage `json:"results"`
		NextCursor      string            `json:"next_cursor"`
		NextPageResults bool              `json:"next_page_results"`
	}
	if err := c.request(ctx, http.MethodGet, c.projectEndpoint(projectID, "issues/?")+query.Encode(), nil, &response); err != nil {
		return IssuePage{}, err
	}
	page := IssuePage{NextCursor: response.NextCursor, Next: response.NextPageResults}
	for _, raw := range response.Results {
		issue, err := ParseIssue(raw)
		if err != nil {
			return IssuePage{}, err
		}
		page.Results = append(page.Results, issue)
	}
	return page, nil
}

// Issue reads one issue with labels expanded.
func (c *Client) Issue(ctx context.Context, projectID, issueID string) (Issue, error) {
	var raw json.RawMessage
	if err := c.request(ctx, http.MethodGet, c.projectEndpoint(projectID, "issues/", url.PathEscape(issueID), "/?expand=labels"), nil, &raw); err != nil {
		return Issue{}, err
	}
	return ParseIssue(raw)
}

// PatchIssue applies a partial update (state and/or labels).
func (c *Client) PatchIssue(ctx context.Context, projectID, issueID string, body map[string]any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.request(ctx, http.MethodPatch, c.projectEndpoint(projectID, "issues/", url.PathEscape(issueID), "/"), payload, nil)
}

// PostComment posts an HTML comment on an issue.
func (c *Client) PostComment(ctx context.Context, projectID, issueID, commentHTML string) error {
	payload, err := json.Marshal(map[string]any{"comment_html": commentHTML})
	if err != nil {
		return err
	}
	return c.request(ctx, http.MethodPost, c.projectEndpoint(projectID, "issues/", url.PathEscape(issueID), "/comments/"), payload, nil)
}

// CreateState creates a project state (used for the review state when missing).
func (c *Client) CreateState(ctx context.Context, projectID, name, group, color string) (State, error) {
	payload, err := json.Marshal(map[string]string{"name": name, "group": group, "color": color})
	if err != nil {
		return State{}, err
	}
	var state State
	if err := c.request(ctx, http.MethodPost, c.projectEndpoint(projectID, "states/"), payload, &state); err != nil {
		return State{}, err
	}
	return state, nil
}

// CreateLabel creates a project label (used for the failed label when missing).
func (c *Client) CreateLabel(ctx context.Context, projectID, name, color string) (Label, error) {
	payload, err := json.Marshal(map[string]string{"name": name, "color": color})
	if err != nil {
		return Label{}, err
	}
	var label Label
	if err := c.request(ctx, http.MethodPost, c.projectEndpoint(projectID, "labels/"), payload, &label); err != nil {
		return Label{}, err
	}
	return label, nil
}

// request performs one Plane API call and decodes the JSON response.
func (c *Client) request(ctx context.Context, method, endpoint string, body []byte, output any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-API-Key", c.config.APIKey)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("plane request %s: %w", method, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("plane request %s %s: HTTP %d: %s", method, endpoint, response.StatusCode, strings.TrimSpace(string(payload)))
	}
	if output == nil {
		return nil
	}
	if err := json.Unmarshal(payload, output); err != nil {
		return fmt.Errorf("decode plane response: %w", err)
	}
	return nil
}
