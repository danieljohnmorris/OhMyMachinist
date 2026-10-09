package issues

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var ErrIssueNotFound = errors.New("issue not found")

// Issue is the source-neutral representation used by factory workflows.
type Issue struct {
	ID          string
	Key         string
	Title       string
	Description string
	URL         string
	Labels      []string
	TeamID      string
}

// Source is the minimum contract needed to submit and report on work.
type Source interface {
	FetchIssue(ctx context.Context, key string) (Issue, error)
	PostComment(ctx context.Context, key, body string) error
}

// StateSetter is optionally implemented by sources that allow state updates.
type StateSetter interface {
	SetState(ctx context.Context, key, state string) error
}

// LabelPoller is optionally implemented by sources that support polling.
type LabelPoller interface {
	IssuesWithLabel(ctx context.Context, label string) ([]Issue, error)
}

// Title combines the stable issue key with the human issue title.
func Title(key, title string) string {
	key = strings.TrimSpace(key)
	title = strings.TrimSpace(title)
	switch {
	case key == "":
		return title
	case title == "":
		return key
	default:
		return key + ": " + title
	}
}

func requiresValue(value, field string) error {
	if value == "" {
		return fmt.Errorf("issues: %s is required", field)
	}
	return nil
}

func requiresBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return errors.New("issues: comment body is required")
	}
	return nil
}
