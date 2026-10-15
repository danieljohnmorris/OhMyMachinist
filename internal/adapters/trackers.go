package adapters

import (
	"context"
	"strings"

	"github.com/owainlewis/machinist/internal/issues"
)

// CompletedLinearState is the Linear workflow state Machinist moves delivered
// tickets to, mirroring Plane's Done state.
const CompletedLinearState = "Done"

// PlaneTracker adapts the existing Plane client to the Tracker interface. It
// is the first tracker adapter and wraps today's intake unchanged.
type PlaneTracker struct {
	Client *issues.PlaneClient
}

func (t PlaneTracker) Kind() string { return TrackerPlane }

func (t PlaneTracker) Intake(ctx context.Context, label string) ([]issues.Issue, error) {
	return t.Client.IssuesWithLabel(ctx, label)
}

func (t PlaneTracker) Comment(ctx context.Context, key, body string) error {
	return t.Client.PostComment(ctx, key, body)
}

func (t PlaneTracker) Resolve(ctx context.Context, key string) error {
	return t.Client.SetState(ctx, key, issues.CompletedPlaneState)
}

func (t PlaneTracker) Link(ctx context.Context, key string) (string, error) {
	return t.Client.BrowseURL(key), nil
}

// LinearTracker adapts the Linear client to the Tracker interface. It is the
// second tracker adapter; intake scopes to the configured project.
type LinearTracker struct {
	Client *issues.LinearClient
}

func (t LinearTracker) Kind() string { return TrackerLinear }

func (t LinearTracker) Intake(ctx context.Context, label string) ([]issues.Issue, error) {
	return t.Client.IssuesWithLabel(ctx, label, t.Client.Project())
}

func (t LinearTracker) Comment(ctx context.Context, key, body string) error {
	return t.Client.PostComment(ctx, key, body)
}

func (t LinearTracker) Resolve(ctx context.Context, key string) error {
	return t.Client.SetState(ctx, key, CompletedLinearState)
}

// Link fetches the issue because Linear ticket URLs are per-issue and cannot
// be derived from the key alone.
func (t LinearTracker) Link(ctx context.Context, key string) (string, error) {
	issue, err := t.Client.FetchIssue(ctx, key)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(issue.URL), nil
}
