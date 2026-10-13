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

// DefaultLinearResolvedState is used when a Linear issue source does not
// configure resolved_state explicitly.
const DefaultLinearResolvedState = "Done"

type LinearConfig struct {
	APIKey        string
	Project       string
	ResolvedState string
}

type LinearClient struct {
	config        LinearConfig
	project       string
	resolvedState string
	endpoint      string
	client        *http.Client
}

func NewLinearClient(config LinearConfig) (*LinearClient, error) {
	if err := requiresValue(config.APIKey, "LINEAR_API_KEY"); err != nil {
		return nil, err
	}
	resolvedState := strings.TrimSpace(config.ResolvedState)
	if resolvedState == "" {
		resolvedState = DefaultLinearResolvedState
	}
	return &LinearClient{config: config, project: config.Project, resolvedState: resolvedState, endpoint: linearEndpoint, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

// ResolvedState reports the state name used to resolve finished work.
func (l *LinearClient) ResolvedState() string {
	return l.resolvedState
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

// maximumLinearPages bounds cursor pagination so a misbehaving endpoint cannot
// loop forever; 20 pages at first:100 covers any realistic label backlog.
const maximumLinearPages = 20

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
	query := fmt.Sprintf(`query Issues($label: String!%s, $after: String) { issues(filter: {%s}, first: 100, after: $after) { nodes { id identifier title description url labels { name } team { id } } pageInfo { hasNextPage endCursor } } }`, projectVariableType(project), projectFilter)
	issues := []Issue{}
	after := ""
	for page := 0; ; page++ {
		var response struct {
			Data struct {
				Issues struct {
					Nodes    []linearIssueNode `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"issues"`
			} `json:"data"`
		}
		variables["after"] = after
		if err := l.request(ctx, map[string]any{"query": query, "variables": variables}, &response); err != nil {
			return nil, err
		}
		for _, node := range response.Data.Issues.Nodes {
			issues = append(issues, node.issue())
		}
		if !response.Data.Issues.PageInfo.HasNextPage || response.Data.Issues.PageInfo.EndCursor == "" {
			return issues, nil
		}
		if page+1 >= maximumLinearPages {
			return issues, nil
		}
		after = response.Data.Issues.PageInfo.EndCursor
	}
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
	if err := l.request(ctx, map[string]any{"query": query, "variables": variables}, &response); err != nil {
		return err
	}
	if !response.Data.CommentCreate.Success {
		return fmt.Errorf("linear commentCreate failed: %s", key)
	}
	return nil
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
	if err := l.request(ctx, map[string]any{"query": query, "variables": map[string]string{"issue": issue.ID, "state": stateID}}, &response); err != nil {
		return err
	}
	if !response.Data.IssueUpdate.Success {
		return fmt.Errorf("linear issueUpdate failed: %s", key)
	}
	return nil
}

// IssueState reports the current workflow state name of the issue.
func (l *LinearClient) IssueState(ctx context.Context, key string) (string, error) {
	if err := requiresValue(key, "issue key"); err != nil {
		return "", err
	}
	var response struct {
		Data struct {
			Issues struct {
				Nodes []struct {
					State struct {
						Name string `json:"name"`
					} `json:"state"`
				} `json:"nodes"`
			} `json:"issues"`
		} `json:"data"`
	}
	query := `query IssueState($key: String!) { issues(filter: {identifier: {eq: $key}}, first: 1) { nodes { state { name } } } }`
	if err := l.request(ctx, map[string]any{"query": query, "variables": map[string]string{"key": key}}, &response); err != nil {
		return "", err
	}
	if len(response.Data.Issues.Nodes) == 0 {
		return "", fmt.Errorf("%w: %s", ErrIssueNotFound, key)
	}
	return response.Data.Issues.Nodes[0].State.Name, nil
}

type graphQLError struct {
	Message string `json:"message"`
}

// request performs a GraphQL call and fails on HTTP errors, GraphQL errors,
// and empty data before decoding the data payload into output.
func (l *LinearClient) request(ctx context.Context, body any, output any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, l.endpoint, bytes.NewReader(payload))
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
	var envelope struct {
		Errors []graphQLError `json:"errors"`
	}
	if err := json.Unmarshal(responseBody, &envelope); err != nil {
		return fmt.Errorf("linear request: decode: %w", err)
	}
	if len(envelope.Errors) > 0 {
		messages := make([]string, 0, len(envelope.Errors))
		for _, graphqlErr := range envelope.Errors {
			messages = append(messages, graphqlErr.Message)
		}
		return fmt.Errorf("linear request: GraphQL error(s): %s", strings.Join(messages, "; "))
	}
	if err := json.Unmarshal(responseBody, output); err != nil {
		return fmt.Errorf("linear request: decode data: %w", err)
	}
	return nil
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
