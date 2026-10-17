package controlplane

import (
	"context"
	"fmt"
	"strings"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/pelletier/go-toml/v2"
)

// projectSettingPrefix names the settings keys that define [tracker.projects]
// entries: tracker.projects.<NAME> holds the TOML body of that project's table.
// The Projects view writes these; they overlay the config file the same way a
// config overlay does (settings win, tables replace per project), so projects
// become editable without restarting the server.
const projectSettingPrefix = "tracker.projects."

// applyProjectOverrides merges the tracker.projects.<NAME> settings into the
// config's [tracker.projects] tables before resolution. A malformed value is an
// error rather than a skipped entry: the setting shadows a file-defined project,
// and silently dropping it would make ticket routing disagree with the config.
func (s *Server) applyProjectOverrides(ctx context.Context, c *config.Config) error {
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return err
	}
	for key, value := range settings {
		name, ok := strings.CutPrefix(key, projectSettingPrefix)
		if !ok {
			continue
		}
		var project config.TrackerProject
		if err := toml.Unmarshal([]byte(value), &project); err != nil {
			return fmt.Errorf("setting %q: %w", key, err)
		}
		if c.Tracker.Projects == nil {
			c.Tracker.Projects = make(map[string]config.TrackerProject)
		}
		c.Tracker.Projects[strings.ToUpper(strings.TrimSpace(name))] = project
	}
	return nil
}
