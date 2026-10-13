package controlplane

import (
	"context"
	"fmt"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/issues"
)

type issuePoller interface {
	Poll(ctx context.Context, label string) ([]issues.Issue, error)
}

// issueCommenter is implemented by issue sources that can post comments back to
// the ticket (both Plane and Linear clients do).
type issueCommenter interface {
	PostComment(ctx context.Context, key, body string) error
}

type planePoller struct {
	client *issues.PlaneClient
}

func (p planePoller) Poll(ctx context.Context, label string) ([]issues.Issue, error) {
	return p.client.IssuesWithLabel(ctx, label)
}

func (p planePoller) PostComment(ctx context.Context, key, body string) error {
	return p.client.PostComment(ctx, key, body)
}

type linearPoller struct {
	client *issues.LinearClient
}

func (l linearPoller) Poll(ctx context.Context, label string) ([]issues.Issue, error) {
	return l.client.IssuesWithLabel(ctx, label, l.client.Project())
}

func (l linearPoller) PostComment(ctx context.Context, key, body string) error {
	return l.client.PostComment(ctx, key, body)
}

func loadIssueSources(path string) (map[string]issuePoller, map[string]*issues.PlaneClient, error) {
	definition, err := config.LoadDefinitions(path)
	if err != nil {
		return nil, nil, fmt.Errorf("load issue sources: %w", err)
	}
	triggers, err := config.LoadTriggers(path)
	if err != nil {
		return nil, nil, fmt.Errorf("load issue source triggers: %w", err)
	}
	planeConnections := definition.IssueSources.PlaneProjects()
	linearConnections := definition.IssueSources.LinearProjects()
	result := map[string]issuePoller{}
	planeClients := map[string]*issues.PlaneClient{}
	for _, trigger := range triggers {
		if trigger.Family != "issue" {
			continue
		}
		if trigger.SourceKind == "plane" {
			_, ok := definition.IssueSources.Plane[trigger.Repository]
			if !ok {
				return nil, nil, fmt.Errorf("issue trigger %q has no Plane project", trigger.Identity)
			}
			client, err := issues.NewPlaneConnection(planeConnections[trigger.Repository])
			if err != nil {
				return nil, nil, fmt.Errorf("issue trigger %q: %w", trigger.Identity, err)
			}
			result[trigger.Identity] = planePoller{client: client}
			planeClients[trigger.Identity] = client
			continue
		}
		if _, ok := definition.IssueSources.Linear[trigger.Repository]; trigger.SourceKind == "linear" && !ok {
			return nil, nil, fmt.Errorf("issue trigger %q has no Linear project", trigger.Identity)
		}
		client, err := issues.NewLinearConnection(linearConnections[trigger.Repository])
		if err != nil {
			return nil, nil, fmt.Errorf("issue trigger %q: %w", trigger.Identity, err)
		}
		result[trigger.Identity] = linearPoller{client: client}
	}
	return result, planeClients, nil
}
