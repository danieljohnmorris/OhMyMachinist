package skills

import (
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"
)

type skillMetadata struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// validateSkillFile checks the SKILL.md frontmatter and returns the skill name.
// Both name and description are required; anything else is rejected with a
// clear error before the skill touches the library.
func validateSkillFile(body []byte) (string, error) {
	block, ok := frontmatterBlock(string(body))
	if !ok {
		return "", errors.New(`SKILL.md must start with a "---" frontmatter block`)
	}
	var metadata skillMetadata
	if err := yaml.Unmarshal([]byte(block), &metadata); err != nil {
		return "", fmt.Errorf("parse SKILL.md frontmatter: %w", err)
	}
	name := strings.TrimSpace(metadata.Name)
	if name == "" {
		return "", errors.New(`SKILL.md frontmatter must set a non-empty "name"`)
	}
	if strings.TrimSpace(metadata.Description) == "" {
		return "", errors.New(`SKILL.md frontmatter must set a non-empty "description"`)
	}
	return name, nil
}

// frontmatterBlock returns the YAML between the opening and closing "---"
// fences, or ok=false when the document has no well-formed frontmatter.
func frontmatterBlock(text string) (string, bool) {
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	start := -1
	for index, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.TrimSpace(line) == "---" {
			start = index
		}
		break
	}
	if start < 0 {
		return "", false
	}
	for index := start + 1; index < len(lines); index++ {
		if strings.TrimSpace(lines[index]) == "---" {
			return strings.Join(lines[start+1:index], "\n"), true
		}
	}
	return "", false
}

// skillKey maps a skill name to its library directory name. Namespaced skills
// (feature-dev:code-reviewer) map to feature-dev_code-reviewer. The name comes
// from untrusted frontmatter, so anything that could escape the root is
// rejected.
// Key returns the library directory name for a skill name.
func Key(name string) (string, error) {
	key := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(name), ":", "_"))
	if key == "" || key == "." || key == ".." || strings.ContainsAny(key, `/\`) {
		return "", fmt.Errorf("skill has an unusable name: %q", name)
	}
	return key, nil
}
