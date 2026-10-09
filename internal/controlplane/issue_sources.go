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

type planePoller struct {
	client *issues.PlaneClient
}

func (p planePoller) Poll(ctx context.Context, label string) ([]issues.Issue, error) {
	return p.client.IssuesWithLabel(ctx, label)
}

type linearPoller struct {
	client *issues.LinearClient
}

func (l linearPoller) Poll(ctx context.Context, label string) ([]issues.Issue, error) {
	return l.client.IssuesWithLabel(ctx, label, l.client.Project())
}

func loadIssueSources(path string) (map[string]issuePoller, error) {
	definition, err := config.LoadConfig(path)
	if err != nil {
		return nil, fmt.Errorf("load issue sources: %w", err)
	}
	triggers, err := config.LoadTriggers(path)
	if err != nil {
		return nil, fmt.Errorf("load issue source triggers: %w", err)
	}
	planeConnections := definition.IssueSources.PlaneProjects()
	linearConnections := definition.IssueSources.LinearProjects()
	result := map[string]issuePoller{}
	for _, trigger := range triggers {
		if trigger.Family != "issue" {
			continue
		}
		if trigger.SourceKind == "plane" {
			_, ok := definition.IssueSources.Plane[trigger.Repository]
			if !ok {
				return nil, fmt.Errorf("issue trigger %q has no Plane project", trigger.Identity)
			}
			client, err := issues.NewPlaneConnection(planeConnections[trigger.Repository])
			if err != nil {
				return nil, fmt.Errorf("issue trigger %q: %w", trigger.Identity, err)
			}
			result[trigger.Identity] = planePoller{client: client}
			continue
		}
		if _, ok := definition.IssueSources.Linear[trigger.Repository]; trigger.SourceKind == "linear" && !ok {
			return nil, fmt.Errorf("issue trigger %q has no Linear project", trigger.Identity)
		}
		client, err := issues.NewLinearConnection(linearConnections[trigger.Repository])
		if err != nil {
			return nil, fmt.Errorf("issue trigger %q: %w", trigger.Identity, err)
		}
		result[trigger.Identity] = linearPoller{client: client}
	}
	return result, nil
}
