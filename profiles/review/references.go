package profiles

import (
	"fmt"
	"strings"

	"github.com/owainlewis/machinist/internal/skills"
)

// SkillReferenced reports whether any shipped review profile still references
// the named skill through a reviewer's skills, skill, or prompt_from field. It
// backs the uninstall guard shared by the CLI, the HTTP API and the MCP tools.
// An error means the reference set could not be read; callers must not treat
// that as "unreferenced".
func SkillReferenced(name string) (bool, error) {
	key, err := skills.Key(name)
	if err != nil {
		return false, err
	}
	entries, err := FS.ReadDir(".")
	if err != nil {
		return false, fmt.Errorf("read review profiles: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		body, err := FS.ReadFile(entry.Name())
		if err != nil {
			return false, fmt.Errorf("read review profile %s: %w", entry.Name(), err)
		}
		for _, reference := range skills.ProfileReferences(string(body)) {
			if referenceKey, err := skills.Key(reference); err == nil && referenceKey == key {
				return true, nil
			}
		}
	}
	return false, nil
}
