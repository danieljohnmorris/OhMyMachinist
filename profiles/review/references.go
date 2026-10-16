package profiles

import (
	"fmt"
	"io/fs"
	"strings"

	"github.com/owainlewis/machinist/internal/skills"
)

// SkillReferenced reports whether any shipped review profile still references
// the named skill through a reviewer's skills, skill, or prompt_from field. It
// backs the uninstall guard shared by the CLI, the HTTP API and the MCP tools.
// An error means the reference set could not be read — including a profile
// whose YAML does not parse, which is reported with the file name — callers
// must not treat that as "unreferenced".
func SkillReferenced(name string) (bool, error) {
	return skillReferenced(FS, name)
}

func skillReferenced(fsys fs.FS, name string) (bool, error) {
	key, err := skills.Key(name)
	if err != nil {
		return false, err
	}
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return false, fmt.Errorf("read review profiles: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		body, err := fs.ReadFile(fsys, entry.Name())
		if err != nil {
			return false, fmt.Errorf("read review profile %s: %w", entry.Name(), err)
		}
		references, err := skills.ProfileReferences(string(body))
		if err != nil {
			return false, fmt.Errorf("parse review profile %s: %w", entry.Name(), err)
		}
		for _, reference := range references {
			if referenceKey, err := skills.Key(reference); err == nil && referenceKey == key {
				return true, nil
			}
		}
	}
	return false, nil
}
