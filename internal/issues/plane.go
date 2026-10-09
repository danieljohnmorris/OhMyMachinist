package issues

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

func (p *PlaneClient) IssuesWithLabel(ctx context.Context, label string) ([]Issue, error) {
	if err := requiresValue(label, "label"); err != nil {
		return nil, err
	}
	query := url.Values{"labels": []string{label}}
	var response struct {
		Results []struct {
			ID          string       `json:"id"`
			Name        string       `json:"name"`
			Description string       `json:"description"`
			URL         string       `json:"url"`
			Labels      []planeLabel `json:"labels"`
		} `json:"results"`
	}
	if err := p.request(ctx, http.MethodGet, p.endpoint("issues/?", query.Encode()), nil, &response); err != nil {
		return nil, err
	}
	issues := make([]Issue, 0, len(response.Results))
	for _, result := range response.Results {
		issues = append(issues, Issue{
			ID: result.ID, Key: result.ID, Title: result.Name, Description: result.Description,
			URL: firstNonEmpty(result.URL, p.IssueURL(result.ID)), Labels: planeLabelNames(result.Labels),
		})
	}
	return issues, nil
}

func (p *PlaneClient) PostComment(ctx context.Context, key, body string) error {
	if err := requiresValue(key, "issue key"); err != nil {
		return err
	}
	if err := requiresBody(body); err != nil {
		return err
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
	payload, _ := json.Marshal(map[string]string{"state": state})
	return p.request(ctx, http.MethodPatch, p.endpoint("issues/", key, "/"), payload, nil)
}

func (p *PlaneClient) IssueURL(key string) string { return p.endpoint("issues/", key, "/") }

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

func (p *PlaneClient) request(ctx context.Context, method, endpoint string, body []byte, output any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-API-Key", p.config.APIKey)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("plane request %s: HTTP %d: %s", method, response.StatusCode, strings.TrimSpace(string(payload)))
	}
	if output == nil {
		return nil
	}
	return json.Unmarshal(payload, output)
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
