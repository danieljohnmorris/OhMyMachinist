package issues

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const linearEndpoint = "https://api.linear.app/graphql"

// linearPageSize bounds one page of a label poll; polls follow the cursor
// until Linear reports no further pages.
const linearPageSize = 100

// CompletedLinearState is the state name Machinist moves delivered tickets
// to. Teams that renamed it still match through their completed-type state.
const CompletedLinearState = "Done"

type LinearConfig struct {
	APIKey  string
	Project string
	// Endpoint overrides the GraphQL endpoint; empty uses Linear's API.
	Endpoint string
}

type LinearClient struct {
	config   LinearConfig
	project  string
	endpoint string
	client   *http.Client
}

func NewLinearClient(config LinearConfig) (*LinearClient, error) {
	if err := requiresValue(config.APIKey, "LINEAR_API_KEY"); err != nil {
		return nil, err
	}
	endpoint := strings.TrimSpace(config.Endpoint)
	if endpoint == "" {
		endpoint = linearEndpoint
	}
	return &LinearClient{config: config, project: config.Project, endpoint: endpoint, client: &http.Client{Timeout: 30 * time.Second}}, nil
}

// linearIssueFields is the selection shared by every issue query. Labels are
// a connection in Linear's schema, so names sit under nodes.
const linearIssueFields = `id identifier title description url labels { nodes { name } } team { id } state { name type }`

// lookupIssue finds one issue by UUID or identifier (ENG-123) through the
// singleton issue(id:) query, which Linear documents as accepting both forms.
// IssueFilter.id compares UUIDs only, so it cannot resolve identifiers.
// Linear reports a missing issue as an "Entity not found" GraphQL error with
// null data; that maps to found=false.
func (l *LinearClient) lookupIssue(ctx context.Context, key, fields string, node any) (bool, error) {
	if err := requiresValue(key, "issue key"); err != nil {
		return false, err
	}
	var data struct {
		Issue json.RawMessage `json:"issue"`
	}
	query := `query Issue($key: String!) { issue(id: $key) { ` + fields + ` } }`
	if err := l.request(ctx, query, map[string]any{"key": key}, &data); err != nil {
		var responseErr *linearResponseError
		if errors.As(err, &responseErr) && responseErr.notFound() {
			return false, nil
		}
		return false, err
	}
	if len(data.Issue) == 0 || string(data.Issue) == "null" {
		return false, nil
	}
	return true, json.Unmarshal(data.Issue, node)
}

func (l *LinearClient) fetchNode(ctx context.Context, key string) (linearIssueNode, error) {
	var node linearIssueNode
	found, err := l.lookupIssue(ctx, key, linearIssueFields, &node)
	if err != nil {
		return linearIssueNode{}, err
	}
	if !found {
		return linearIssueNode{}, fmt.Errorf("%w: %s", ErrIssueNotFound, key)
	}
	return node, nil
}

func (l *LinearClient) FetchIssue(ctx context.Context, key string) (Issue, error) {
	node, err := l.fetchNode(ctx, key)
	if err != nil {
		return Issue{}, err
	}
	return node.issue(), nil
}

// LinearIssueState is the workflow state an issue sits in. Type is one of
// triage, backlog, unstarted, started, completed or canceled.
type LinearIssueState struct {
	Name string
	Type string
	// URL is the issue's browser URL, returned with the state so callers can
	// match PR links without a second request.
	URL string
}

// IsCompletedLinearState reports whether the state is in Linear's completed
// type.
func IsCompletedLinearState(state LinearIssueState) bool {
	return strings.EqualFold(state.Type, "completed")
}

// IssueState fetches the current workflow state of an issue by key.
func (l *LinearClient) IssueState(ctx context.Context, key string) (LinearIssueState, error) {
	node, err := l.fetchNode(ctx, key)
	if err != nil {
		return LinearIssueState{}, err
	}
	if node.State.Name == "" && node.State.Type == "" {
		return LinearIssueState{}, fmt.Errorf("linear issue %s has no state", key)
	}
	return LinearIssueState{Name: node.State.Name, Type: node.State.Type, URL: node.URL}, nil
}

// IssuesWithLabel returns open issues carrying label, optionally limited to
// one project. Completed and canceled issues are excluded so a finished
// ticket that keeps its label never admits new work.
func (l *LinearClient) IssuesWithLabel(ctx context.Context, label string, project string) ([]Issue, error) {
	if err := requiresValue(label, "label"); err != nil {
		return nil, err
	}
	filter := `labels: {name: {eq: $label}}, state: {type: {nin: ["completed", "canceled"]}}`
	declarations := "$label: String!, $after: String"
	variables := map[string]any{"label": label}
	if project != "" {
		filter += ", project: {name: {eq: $project}}"
		declarations += ", $project: String!"
		variables["project"] = project
	}
	query := fmt.Sprintf(`query Issues(%s) { issues(filter: {%s}, first: %d, after: $after) { nodes { %s } pageInfo { hasNextPage endCursor } } }`,
		declarations, filter, linearPageSize, linearIssueFields)
	var result []Issue
	for {
		var data struct {
			Issues struct {
				Nodes    []linearIssueNode `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issues"`
		}
		if err := l.request(ctx, query, variables, &data); err != nil {
			return nil, err
		}
		for _, node := range data.Issues.Nodes {
			result = append(result, node.issue())
		}
		if !data.Issues.PageInfo.HasNextPage || data.Issues.PageInfo.EndCursor == "" {
			return result, nil
		}
		variables["after"] = data.Issues.PageInfo.EndCursor
	}
}

// Project reports the configured project name.
func (l *LinearClient) Project() string {
	return l.project
}

func (l *LinearClient) PostComment(ctx context.Context, key, body string) error {
	if err := requiresBody(body); err != nil {
		return err
	}
	node, err := l.fetchNode(ctx, key)
	if err != nil {
		return err
	}
	var data struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	query := `mutation Comment($issue: String!, $body: String!) { commentCreate(input: {issueId: $issue, body: $body}) { success } }`
	if err := l.request(ctx, query, map[string]any{"issue": node.ID, "body": body}, &data); err != nil {
		return err
	}
	if !data.CommentCreate.Success {
		return fmt.Errorf("linear comment on %s was not created", key)
	}
	return nil
}

// SetState moves an issue to the named workflow state of its team. The
// completed-state name also matches the team's completed-type state when no
// state carries that exact name.
func (l *LinearClient) SetState(ctx context.Context, key, state string) error {
	if err := requiresValue(state, "state"); err != nil {
		return err
	}
	var node struct {
		ID   string `json:"id"`
		Team struct {
			States struct {
				Nodes []linearState `json:"nodes"`
			} `json:"states"`
		} `json:"team"`
	}
	found, err := l.lookupIssue(ctx, key, `id team { states(first: 250) { nodes { id name type } } }`, &node)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: %s", ErrIssueNotFound, key)
	}
	stateID := linearStateID(node.Team.States.Nodes, state)
	if stateID == "" {
		return fmt.Errorf("linear team of %s has no state %q", key, state)
	}
	var data struct {
		IssueUpdate struct {
			Success bool `json:"success"`
		} `json:"issueUpdate"`
	}
	query := `mutation SetState($issue: String!, $state: String!) { issueUpdate(id: $issue, input: {stateId: $state}) { success } }`
	if err := l.request(ctx, query, map[string]any{"issue": node.ID, "state": stateID}, &data); err != nil {
		return err
	}
	if !data.IssueUpdate.Success {
		return fmt.Errorf("linear state update of %s was not applied", key)
	}
	return nil
}

// LinearRemoteProject is a remote Linear project.
type LinearRemoteProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Projects lists the workspace's projects, following the cursor until Linear
// reports no further pages.
func (l *LinearClient) Projects(ctx context.Context) ([]LinearRemoteProject, error) {
	query := `query Projects($after: String) { projects(first: 100, after: $after) { nodes { id name } pageInfo { hasNextPage endCursor } } }`
	var projects []LinearRemoteProject
	variables := map[string]any{}
	for {
		var data struct {
			Projects struct {
				Nodes    []LinearRemoteProject `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"projects"`
		}
		if err := l.request(ctx, query, variables, &data); err != nil {
			return nil, err
		}
		projects = append(projects, data.Projects.Nodes...)
		if !data.Projects.PageInfo.HasNextPage || data.Projects.PageInfo.EndCursor == "" {
			return projects, nil
		}
		variables["after"] = data.Projects.PageInfo.EndCursor
	}
}

// IssueWithState pairs an issue with the workflow state it sits in. StateType
// is Linear's state type (triage, backlog, unstarted, started, completed,
// canceled) or Plane's state group (triage, backlog, started, completed,
// cancelled).
type IssueWithState struct {
	Issue
	StateName string
	StateType string
}

// IssuesInProject lists every issue of the named project together with its
// state, including completed and canceled issues so a bulk import can skip
// them explicitly. Unlike the label poll this applies no state filter.
func (l *LinearClient) IssuesInProject(ctx context.Context, project string) ([]IssueWithState, error) {
	if err := requiresValue(project, "project"); err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`query Issues($project: String!, $after: String) { issues(filter: {project: {name: {eq: $project}}}, first: %d, after: $after) { nodes { %s } pageInfo { hasNextPage endCursor } } }`,
		linearPageSize, linearIssueFields)
	var result []IssueWithState
	variables := map[string]any{"project": project}
	for {
		var data struct {
			Issues struct {
				Nodes    []linearIssueNode `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issues"`
		}
		if err := l.request(ctx, query, variables, &data); err != nil {
			return nil, err
		}
		for _, node := range data.Issues.Nodes {
			result = append(result, IssueWithState{Issue: node.issue(), StateName: node.State.Name, StateType: node.State.Type})
		}
		if !data.Issues.PageInfo.HasNextPage || data.Issues.PageInfo.EndCursor == "" {
			return result, nil
		}
		variables["after"] = data.Issues.PageInfo.EndCursor
	}
}

type linearState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

func linearStateID(states []linearState, name string) string {
	for _, state := range states {
		if strings.EqualFold(state.Name, name) {
			return state.ID
		}
	}
	if strings.EqualFold(name, CompletedLinearState) {
		for _, state := range states {
			if strings.EqualFold(state.Type, "completed") {
				return state.ID
			}
		}
	}
	return ""
}

type linearGraphQLError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

// linearResponseError carries the GraphQL errors of one failed request.
type linearResponseError struct {
	status int
	errors []linearGraphQLError
}

func (e *linearResponseError) Error() string {
	messages := make([]string, 0, len(e.errors))
	for _, graphQLError := range e.errors {
		message := graphQLError.Message
		if graphQLError.Extensions.Code != "" {
			message += " (" + graphQLError.Extensions.Code + ")"
		}
		messages = append(messages, message)
	}
	return fmt.Sprintf("linear request: HTTP %d: %s", e.status, strings.Join(messages, "; "))
}

// notFound reports whether every error is Linear's missing-entity error.
func (e *linearResponseError) notFound() bool {
	for _, graphQLError := range e.errors {
		if !strings.Contains(strings.ToLower(graphQLError.Message), "not found") {
			return false
		}
	}
	return len(e.errors) > 0
}

// request runs one GraphQL operation and decodes its data into output.
// GraphQL errors fail the call even when HTTP succeeds, because Linear
// reports validation, permission and rate-limit failures that way.
func (l *LinearClient) request(ctx context.Context, query string, variables map[string]any, output any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": variables})
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
	var envelope struct {
		Data   json.RawMessage      `json:"data"`
		Errors []linearGraphQLError `json:"errors"`
	}
	decodeErr := json.Unmarshal(responseBody, &envelope)
	if len(envelope.Errors) > 0 {
		return &linearResponseError{status: response.StatusCode, errors: envelope.Errors}
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return fmt.Errorf("linear request: HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	if decodeErr != nil {
		return fmt.Errorf("linear request: decode response: %w", decodeErr)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return errors.New("linear request: response has no data")
	}
	return json.Unmarshal(envelope.Data, output)
}

type linearIssueNode struct {
	ID          string `json:"id"`
	Identifier  string `json:"identifier"`
	Title       string `json:"title"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Labels      struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Team struct {
		ID string `json:"id"`
	} `json:"team"`
	State linearState `json:"state"`
}

func (node linearIssueNode) issue() Issue {
	labels := make([]string, 0, len(node.Labels.Nodes))
	for _, label := range node.Labels.Nodes {
		labels = append(labels, label.Name)
	}
	return Issue{
		ID: node.ID, Key: node.Identifier, Title: node.Title, Description: node.Description,
		URL: node.URL, Labels: labels, TeamID: node.Team.ID,
	}
}
