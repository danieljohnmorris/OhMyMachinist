package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// manifestName is the per-skill record stored beside the version directories.
const manifestName = "manifest.json"

// Manifest is the on-disk record for one skill at <root>/<key>/manifest.json.
type Manifest struct {
	// Name is the skill name from the SKILL.md frontmatter (for example
	// feature-dev:code-reviewer).
	Name string `json:"name"`
	// Source is the recorded install source: a GitHub URL, a local path, or a
	// zip path. It never contains credentials.
	Source string `json:"source"`
	// Active is the version the library currently resolves.
	Active string `json:"active"`
	// Versions lists every installed version, oldest first.
	Versions []Version `json:"versions"`
}

// Version is one immutable install of a skill.
type Version struct {
	// Version is the identity and directory name: a commit SHA for GitHub
	// sources, or sha256-<hex> for content-hashed sources.
	Version string `json:"version"`
	// Resolved is the commit SHA (GitHub) or content hash (sha256:<hex>) the
	// version was built from.
	Resolved string `json:"resolved"`
	// InstalledAt records when the version was installed.
	InstalledAt time.Time `json:"installed_at"`
	// Scanned reports whether the secret scan actually ran (gitleaks present).
	Scanned bool `json:"scanned"`
}

// version returns the recorded entry for a version identity.
func (m Manifest) version(identity string) (Version, bool) {
	for _, entry := range m.Versions {
		if entry.Version == identity {
			return entry, true
		}
	}
	return Version{}, false
}

// readManifest loads the manifest for a skill directory. A missing manifest
// returns os.ErrNotExist so callers can distinguish "not installed".
func readManifest(dir string) (Manifest, error) {
	body, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse %s: %w", filepath.Join(dir, manifestName), err)
	}
	return manifest, nil
}

// writeManifest replaces the manifest atomically so a crash cannot leave a
// half-written record beside the version directories.
func writeManifest(dir string, manifest Manifest) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp, err := os.CreateTemp(dir, manifestName+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, filepath.Join(dir, manifestName))
}
