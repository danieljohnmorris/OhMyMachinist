package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Tracker configures project-management integration. The built-in tracker
// stores tickets in the control-plane database and is the default; Plane and
// Linear tickets arrive through [issue_sources] and [triggers.issue.*].
type Tracker struct {
	// Default names the tracker new tickets are filed in: builtin (default),
	// plane, or linear.
	Default string `toml:"default"`
	// KeyPrefix prefixes built-in ticket keys (MM-1, MM-2, ...). Default MM.
	KeyPrefix string `toml:"key_prefix"`
	// Repository is the logical repository a built-in ticket runs against when
	// the ticket names none.
	Repository string `toml:"repository"`
	// Command names the [commands.NAME] entry an approved built-in ticket runs.
	// Approving a ticket without it fails with a configuration error.
	Command string `toml:"command"`
	// Model optionally overrides the command's model.
	Model string `toml:"model"`
	// Projects optionally defines [tracker.projects.NAME] tables. A project
	// names a key prefix, repository, command, and model override for its own
	// tickets; the project name becomes the ticket key prefix.
	Projects map[string]TrackerProject `toml:"projects"`
}

// TrackerProject scopes built-in tickets to one project: its tickets use
// PROJECT-N keys and default to this repository, command, and model.
type TrackerProject struct {
	Repository string `toml:"repository"`
	Command    string `toml:"command"`
	Model      string `toml:"model"`
}

// ResolvedTracker is the validated tracker configuration with defaults applied.
type ResolvedTracker struct {
	Default    string
	KeyPrefix  string
	Repository string
	Command    string
	Model      string
	Projects   map[string]ResolvedProject
}

// ResolvedProject is a validated [tracker.projects.NAME] entry.
type ResolvedProject struct {
	Name       string
	Repository string
	Command    string
	Model      string
}

// DefaultTrackerKeyPrefix is the built-in ticket key prefix when unset.
const DefaultTrackerKeyPrefix = "MM"

var trackerKeyPrefixPattern = regexp.MustCompile(`^[A-Z][A-Z0-9]{0,9}$`)

// ResolveTracker validates [tracker] and applies defaults. A configured
// command must name a defined [commands.NAME] entry.
func (c Config) ResolveTracker() (ResolvedTracker, error) {
	resolved := ResolvedTracker{
		Default:    strings.TrimSpace(c.Tracker.Default),
		KeyPrefix:  strings.TrimSpace(c.Tracker.KeyPrefix),
		Repository: strings.TrimSpace(c.Tracker.Repository),
		Command:    strings.TrimSpace(c.Tracker.Command),
		Model:      strings.TrimSpace(c.Tracker.Model),
	}
	if resolved.Default == "" {
		resolved.Default = "builtin"
	}
	switch resolved.Default {
	case "builtin", "plane", "linear":
	default:
		return ResolvedTracker{}, fmt.Errorf("tracker.default must be builtin, plane, or linear, got %q", resolved.Default)
	}
	if resolved.KeyPrefix == "" {
		resolved.KeyPrefix = DefaultTrackerKeyPrefix
	}
	if !trackerKeyPrefixPattern.MatchString(resolved.KeyPrefix) {
		return ResolvedTracker{}, fmt.Errorf("tracker.key_prefix %q must be 1-10 uppercase letters or digits starting with a letter", resolved.KeyPrefix)
	}
	if resolved.Command != "" {
		if _, ok := c.Commands[resolved.Command]; !ok {
			return ResolvedTracker{}, fmt.Errorf("tracker.command %q is not defined; add a [commands.%s] table or change tracker.command", resolved.Command, resolved.Command)
		}
	}
	if len(c.Tracker.Projects) > 0 {
		resolved.Projects = make(map[string]ResolvedProject, len(c.Tracker.Projects))
		for name, project := range c.Tracker.Projects {
			projectName := strings.ToUpper(strings.TrimSpace(name))
			if !trackerKeyPrefixPattern.MatchString(projectName) {
				return ResolvedTracker{}, fmt.Errorf("tracker.projects.%q must be 1-10 uppercase letters or digits starting with a letter", name)
			}
			entry := ResolvedProject{
				Name:       projectName,
				Repository: strings.TrimSpace(project.Repository),
				Command:    strings.TrimSpace(project.Command),
				Model:      strings.TrimSpace(project.Model),
			}
			if entry.Command != "" {
				if _, ok := c.Commands[entry.Command]; !ok {
					return ResolvedTracker{}, fmt.Errorf("tracker.projects.%s.command %q is not defined; add a [commands.%s] table", name, entry.Command, entry.Command)
				}
			}
			resolved.Projects[projectName] = entry
		}
	}
	return resolved, nil
}
