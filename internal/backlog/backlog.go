// Package backlog defines the native lightweight issue tracker: a persisted
// issue entity per project that work is started from without any external
// tracker. Creating or editing an issue never enqueues work; starting a run
// is the only path from an issue to a job.
package backlog

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// SpecLimit bounds an issue spec to the same size the protocol allows a task.
const SpecLimit = 256 << 10

// Issue states. Completion and cancellation are distinct: a completed issue
// finished its work, a cancelled one was abandoned.
const (
	StateBacklog   = "backlog"
	StateTodo      = "todo"
	StateStarted   = "started"
	StateCompleted = "completed"
	StateCancelled = "cancelled"
)

// States lists every valid issue state.
var States = []string{StateBacklog, StateTodo, StateStarted, StateCompleted, StateCancelled}

var projectPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

// Issue is one persisted backlog issue.
type Issue struct {
	ID        string `json:"id"`
	Project   string `json:"project"`
	Key       string `json:"key"`
	Title     string `json:"title"`
	Spec      string `json:"spec"`
	State     string `json:"state"`
	Revision  int    `json:"revision"`
	URL       string `json:"url"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Update is a partial edit; nil fields are left unchanged.
type Update struct {
	Title *string
	Spec  *string
	State *string
}

// ValidState reports whether state is one of the issue states.
func ValidState(state string) bool {
	for _, candidate := range States {
		if candidate == state {
			return true
		}
	}
	return false
}

// Validate checks an issue against the tracker invariants: a required title,
// a known state, a spec within the protocol limit, and an optional HTTP(S)
// URL.
func (i Issue) Validate() error {
	if strings.TrimSpace(i.Title) == "" {
		return errors.New("title is required")
	}
	if !ValidState(i.State) {
		return fmt.Errorf("unknown issue state %q", i.State)
	}
	if i.Project == "" || !projectPattern.MatchString(i.Project) {
		return fmt.Errorf("project must match [A-Z][A-Z0-9]{1,9}; got %q", i.Project)
	}
	if len(i.Spec) > SpecLimit {
		return fmt.Errorf("spec exceeds %d bytes", SpecLimit)
	}
	if i.URL != "" {
		parsed, err := url.Parse(i.URL)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return errors.New("url must be an HTTP(S) URL")
		}
	}
	return nil
}

// Apply returns the issue with a partial update applied.
func (i Issue) Apply(update Update) Issue {
	if update.Title != nil {
		i.Title = *update.Title
	}
	if update.Spec != nil {
		i.Spec = *update.Spec
	}
	if update.State != nil {
		i.State = *update.State
	}
	return i
}

// Startable reports whether a start may move the issue to the started state:
// only backlog and todo issues transition automatically.
func (i Issue) Startable() bool {
	return i.State == StateBacklog || i.State == StateTodo
}
