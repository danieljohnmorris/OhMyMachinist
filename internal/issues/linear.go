package issues

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const linearEndpoint = "https://api.linear.app/graphql"

type LinearConfig struct {
	APIKey string
	Project string
}

type LinearClient struct {
	config LinearConfig
	project string
	client *http.Client
}

func NewLinearClient(config LinearConfig) (*LinearClient, error) {
	if err := requiresValue(config.APIKey, "LINEAR_API_KEY"); err != nil {
		return nil, err
	}
	return &LinearClient{config: config, project: config.Project, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (l *LinearClient) FetchIssue(ctx context.Context, key string) (Issue, error) {
	if err := requiresValue(key, "issue key"); err != nil {
		return Issue{}, err
	}
	var response struct {
		Data struct {
			Issues struct {
				Nodes []linearIssueNode `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
	}
	query := `query Issue($key: String!) { issues(filter: {identifier: {eq: $key}}, first: 1) { nodes { id identifier title description url labels { name } team { id } } } }`
	if err := l.request(ctx, map[string]any{"query": query, "variables": map[string]string{"key": key}}, &response); err != nil {
		return Issue{}, err
	}
	if len(response.Data.Issues.Nodes) == 0 {
		return Issue{}, fmt.Errorf("%w: %s", ErrIssueNotFound, key)
	}
	return response.Data.Issues.Nodes[0].issue(), nil
}

func (l *LinearClient) IssuesWithLabel(ctx context.Context, label string, project string) ([]Issue, error) {
	if err := requiresValue(label, "label"); err != nil {
		return nil, err
	}
	projectFilter := "labels: {name: {eq: $label}}"
	variables := map[string]any{"label": label}
	if project != "" {
		projectFilter = "labels: {name: {eq: $label}}, project: {name: {eq: $project}}"
		variables["project"] = project
	}
	var response struct {
		Data struct {
			Issues struct {
				Nodes []linearIssueNode `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
	}
	query := fmt.Sprintf(`query Issues($label: String!%s) { issues(filter: {%s}, first: 100) { nodes { id identifier title description url labels { name } team { id } } } }`, projectVariableType(project), projectFilter)
	if err := l.request(ctx, map[string]any{"query": query, "variables": variables}, &response); err != nil {
		return nil, err
	}
	issues := make([]Issue, 0, len(response.Data.Issues.Nodes))
	for _, node := range response.Data.Issues.Nodes {
		issues = append(issues, node.issue())
	}
	return issues, nil
}

// Project reports the configured project name.
func (l *LinearClient) Project() string {
	return l.project
}

func projectVariableType(project string) string {
	if project == "" {
		return ""
	}
	return ", $project: String!"
}

func (l *LinearClient) PostComment(ctx context.Context, key, body string) error {
	if err := requiresBody(body); err != nil {
		return err
	}
	issue, err := l.FetchIssue(ctx, key)
	if err != nil {
		return err
	}
	var response struct {
		Data struct {
			CommentCreate struct {
				Success bool `json:"success"`
			} `json:"commentCreate"`
		} `json:"data"`
	}
	query := `mutation Comment($issue: String!, $body: String!) { commentCreate(input: {issueId: $issue, body: $body}) { success } }`
		variables := map[string]string{"issue": issue.ID, "body": body}
	return l.request(ctx, map[string]any{"query": query, "variables": variables}, &response)
}

func (l *LinearClient) SetState(ctx context.Context, key, state string) error {
	if err := requiresValue(state, "state"); err != nil {
		return err
	}
	issue, err := l.FetchIssue(ctx, key)
	if err != nil {
		return err
	}
	var statesResponse struct {
		Data struct {
			WorkflowStates struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"nodes"`
			} `json:"workflowStates"`
		} `json:"data"`
	}
	statesQuery := `query States($team: String!) { workflowStates(filter: {team: {id: {eq: $team}}}) { nodes { id name } } }`
	if err = l.request(ctx, map[string]any{"query": statesQuery, "variables": map[string]string{"team": issue.TeamID}}, &statesResponse); err != nil {
		return err
	}
	stateID := ""
	for _, candidate := range statesResponse.Data.WorkflowStates.Nodes {
		if strings.EqualFold(candidate.Name, state) {
			stateID = candidate.ID
			break
		}
	}
	if stateID == "" {
		return fmt.Errorf("linear state not found: %s", state)
	}
	var response struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
	}
	query := `mutation SetState($issue: String!, $state: String!) { issueUpdate(id: $issue, input: {stateId: $state}) { success } }`
	return l.request(ctx, map[string]any{"query": query, "variables": map[string]string{"issue": issue.ID, "state": stateID}}, &response)
}

func (l *LinearClient) request(ctx context.Context, body any, output any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, linearEndpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Authorization", l.config.APIKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := l.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("linear request: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return json.Unmarshal(responseBody, output)
}

type linearIssueNode struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	TeamID      string `json:"-"`
	Labels      []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
}

func (node linearIssueNode) issue() Issue {
	labels := make([]string, 0, len(node.Labels))
	for _, label := range node.Labels {
		labels = append(labels, label.Name)
	}
	return Issue{
		ID: node.ID, Key: node.Identifier, Title: node.Title, Description: node.Description,
		URL: node.URL, Labels: labels, TeamID: node.Team.ID,
	}
}
