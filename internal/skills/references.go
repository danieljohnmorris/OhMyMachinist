package skills

import (
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ProfileReferences returns the skill names a review profile body references
// through reviewer `skills`, `skill`, or `prompt_from`. It backs the
// referenced check that refuses to uninstall a skill still in use. A body
// that cannot be parsed is an error, never an empty reference set: callers
// must treat an unreadable profile as one that may reference any skill.
func ProfileReferences(body string) ([]string, error) {
	var parsed struct {
		Reviewers []struct {
			Skills     []string `yaml:"skills"`
			Skill      string   `yaml:"skill"`
			PromptFrom string   `yaml:"prompt_from"`
		} `yaml:"reviewers"`
	}
	if err := yaml.Unmarshal([]byte(body), &parsed); err != nil {
		return nil, fmt.Errorf("parse profile: %w", err)
	}
	var names []string
	for _, reviewer := range parsed.Reviewers {
		names = append(names, reviewer.Skills...)
		if reviewer.Skill != "" {
			names = append(names, reviewer.Skill)
		}
		if reference := strings.TrimSpace(strings.SplitN(reviewer.PromptFrom, "#", 2)[0]); reference != "" {
			names = append(names, reference)
		}
	}
	return names, nil
}
