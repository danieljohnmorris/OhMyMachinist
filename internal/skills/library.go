// Package skills owns the versioned skills library under the Machinist data
// directory: <data>/skills/<name>/<version>/ with a manifest.json per skill.
//
// Installs come from a GitHub SKILL.md or skill directory URL, a local path,
// or a zip archive. Every version is validated (SKILL.md frontmatter) and
// scanned for secrets before it is written; updates keep the old version and
// leave the active version alone unless asked.
package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// dataDirEnv overrides the Machinist data directory (the parent of skills/).
const dataDirEnv = "MACHINIST_DATA_DIR"

// DefaultDataDir is the Machinist data directory. It can be overridden with
// MACHINIST_DATA_DIR; otherwise it is ~/.machinist.
func DefaultDataDir() string {
	if dir := strings.TrimSpace(os.Getenv(dataDirEnv)); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".machinist")
	}
	return filepath.Join(home, ".machinist")
}

// DefaultRoot is the skills library root: <data>/skills.
func DefaultRoot() string {
	return filepath.Join(DefaultDataDir(), "skills")
}

// Library is the on-disk skills library.
type Library struct {
	// Root is the library directory (<data>/skills).
	Root string
	// Warn receives non-fatal notices, such as a missing gitleaks binary. It
	// may be nil.
	Warn func(string)
	// Scan runs the secret scan for a staged version. It defaults to
	// GitleaksScanner.
	Scan Scanner
	// Now returns the install timestamp; it defaults to time.Now.
	Now func() time.Time
	// GitHub is the GitHub API client; it defaults to the public API with a
	// token from GITHUB_TOKEN.
	GitHub *GitHub
}

// Open prepares a library at root (DefaultRoot when empty) and creates the
// directory when needed.
func Open(root string) (*Library, error) {
	if strings.TrimSpace(root) == "" {
		root = DefaultRoot()
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve skills library: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return nil, fmt.Errorf("create skills library: %w", err)
	}
	return &Library{
		Root:   filepath.Clean(absolute),
		Scan:   GitleaksScanner,
		Now:    time.Now,
		GitHub: defaultGitHub(),
	}, nil
}

func (l *Library) github() *GitHub {
	if l.GitHub != nil {
		return l.GitHub
	}
	return defaultGitHub()
}

func (l *Library) scan() Scanner {
	if l.Scan != nil {
		return l.Scan
	}
	return GitleaksScanner
}

func (l *Library) now() time.Time {
	if l.Now != nil {
		return l.Now().UTC()
	}
	return time.Now().UTC()
}

func (l *Library) warn(message string) {
	if l.Warn != nil {
		l.Warn(message)
	}
}

// Skill is one installed skill as listed from the library.
type Skill struct {
	Name     string    `json:"name"`
	Source   string    `json:"source"`
	Active   string    `json:"active"`
	Versions []Version `json:"versions"`
}

// InstallResult reports the outcome of an install or update.
type InstallResult struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Version string `json:"version"`
	Active  string `json:"active"`
	// Added is false when the resolved version was already installed.
	Added bool `json:"added"`
}

// ErrNotInstalled reports that a skill is not present in the library. Callers
// wrap it with the skill name and use errors.Is to tell it from a read error.
var ErrNotInstalled = errors.New("not installed")

// UninstallOptions controls whether a referenced skill may be removed.
type UninstallOptions struct {
	// Referenced is set by the caller when a review profile still uses the
	// skill; the removal is refused unless Force is set.
	Referenced bool
	// Force removes the skill even when it is referenced.
	Force bool
}

// List returns every installed skill, sorted by name.
func (l *Library) List() ([]Skill, error) {
	entries, err := os.ReadDir(l.Root)
	if err != nil {
		return nil, err
	}
	var skills []Skill
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifest, err := readManifest(filepath.Join(l.Root, entry.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		skills = append(skills, Skill{Name: manifest.Name, Source: manifest.Source, Active: manifest.Active, Versions: manifest.Versions})
	}
	slices.SortFunc(skills, func(a, b Skill) int { return strings.Compare(a.Name, b.Name) })
	return skills, nil
}

// Install resolves a source and adds it as a new, active skill. It refuses to
// overwrite an installed skill so history is never silently dropped.
func (l *Library) Install(ctx context.Context, source Source) (InstallResult, error) {
	resolved, err := l.fetch(ctx, source)
	if err != nil {
		return InstallResult{}, err
	}
	key, err := Key(resolved.name)
	if err != nil {
		return InstallResult{}, err
	}
	dir := filepath.Join(l.Root, key)
	if _, err := os.Stat(filepath.Join(dir, manifestName)); err == nil {
		return InstallResult{}, fmt.Errorf("skill %q is already installed; run update instead", resolved.name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return InstallResult{}, err
	}
	manifest := Manifest{Name: resolved.name, Source: resolved.source}
	if _, err := l.addVersion(ctx, dir, &manifest, resolved, true); err != nil {
		return InstallResult{}, err
	}
	if err := writeManifest(dir, manifest); err != nil {
		return InstallResult{}, err
	}
	return InstallResult{Name: manifest.Name, Source: manifest.Source, Version: resolved.version, Active: manifest.Active, Added: true}, nil
}

// Update re-resolves the recorded source and adds the new version, keeping
// every old version. The active version is left alone unless activate is set.
func (l *Library) Update(ctx context.Context, name string, activate bool) (InstallResult, error) {
	manifest, dir, err := l.load(name)
	if err != nil {
		return InstallResult{}, err
	}
	if isUploadSource(manifest.Source) {
		return InstallResult{}, fmt.Errorf("skill %q was installed from an upload; re-upload it to add a version", manifest.Name)
	}
	source, err := ParseSource(manifest.Source)
	if err != nil {
		return InstallResult{}, fmt.Errorf("re-resolve source for %q: %w", manifest.Name, err)
	}
	resolved, err := l.fetch(ctx, source)
	if err != nil {
		return InstallResult{}, err
	}
	if key, err := Key(resolved.name); err != nil {
		return InstallResult{}, err
	} else if key != filepath.Base(dir) {
		return InstallResult{}, fmt.Errorf("source for %q now resolves to skill %q", manifest.Name, resolved.name)
	}
	added, err := l.addVersion(ctx, dir, &manifest, resolved, activate)
	if err != nil {
		return InstallResult{}, err
	}
	if err := writeManifest(dir, manifest); err != nil {
		return InstallResult{}, err
	}
	return InstallResult{Name: manifest.Name, Source: manifest.Source, Version: resolved.version, Active: manifest.Active, Added: added}, nil
}

// Pin sets the active version to any installed version.
func (l *Library) Pin(name, version string) error {
	manifest, dir, err := l.load(name)
	if err != nil {
		return err
	}
	if _, ok := manifest.version(version); !ok {
		return fmt.Errorf("skill %q has no version %q", manifest.Name, version)
	}
	manifest.Active = version
	return writeManifest(dir, manifest)
}

// Uninstall removes a skill and every version. A skill that a caller reports
// as referenced is refused unless Force is set.
func (l *Library) Uninstall(name string, options UninstallOptions) error {
	manifest, dir, err := l.load(name)
	if err != nil {
		return err
	}
	if options.Referenced && !options.Force {
		return fmt.Errorf("skill %q is referenced by a review profile; pass force to remove it", manifest.Name)
	}
	return os.RemoveAll(dir)
}

// Versions returns the manifest for one skill.
func (l *Library) Versions(name string) (Manifest, error) {
	manifest, _, err := l.load(name)
	return manifest, err
}

func (l *Library) load(name string) (Manifest, string, error) {
	key, err := Key(name)
	if err != nil {
		return Manifest{}, "", err
	}
	dir := filepath.Join(l.Root, key)
	manifest, err := readManifest(dir)
	if errors.Is(err, os.ErrNotExist) {
		return Manifest{}, "", fmt.Errorf("skill %q is %w", name, ErrNotInstalled)
	}
	if err != nil {
		return Manifest{}, "", err
	}
	return manifest, dir, nil
}

// addVersion validates, scans, and stages a resolved skill, then records the
// version in the manifest. It returns whether a new version directory was
// written.
func (l *Library) addVersion(ctx context.Context, dir string, manifest *Manifest, resolved fetched, activate bool) (bool, error) {
	if manifest.Name == "" {
		manifest.Name = resolved.name
	}
	if existing, ok := manifest.version(resolved.version); ok {
		if activate && manifest.Active != existing.Version {
			manifest.Active = existing.Version
		}
		return false, nil
	}
	versionDir := filepath.Join(dir, resolved.version)
	scanned, err := l.writeVersion(ctx, versionDir, resolved.files)
	if err != nil {
		return false, err
	}
	manifest.Versions = append(manifest.Versions, Version{
		Version:     resolved.version,
		Resolved:    resolved.resolved,
		InstalledAt: l.now(),
		Scanned:     scanned,
	})
	if activate || manifest.Active == "" {
		manifest.Active = resolved.version
	}
	return true, nil
}

// writeVersion stages a skill in a temporary directory, scans it, and moves it
// into place. Nothing becomes active until the scan is clean.
func (l *Library) writeVersion(ctx context.Context, versionDir string, files map[string][]byte) (bool, error) {
	staging, err := os.MkdirTemp(l.Root, ".staging-*")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(staging)
	for name, body := range files {
		target := filepath.Join(staging, filepath.FromSlash(name))
		if !strings.HasPrefix(target, staging+string(os.PathSeparator)) {
			return false, fmt.Errorf("skill file %q escapes its directory", name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return false, err
		}
		if err := os.WriteFile(target, body, 0o644); err != nil {
			return false, err
		}
	}
	scan, err := l.scan()(ctx, staging)
	if err != nil {
		return false, err
	}
	if len(scan.Findings) > 0 {
		return false, fmt.Errorf("skill rejected: gitleaks found secrets: %s", strings.Join(scan.Findings, "; "))
	}
	if !scan.Scanned {
		l.warn("gitleaks is not on PATH; installed without a secret scan")
	}
	if err := os.MkdirAll(filepath.Dir(versionDir), 0o755); err != nil {
		return false, err
	}
	if err := os.RemoveAll(versionDir); err != nil {
		return false, err
	}
	if err := os.Rename(staging, versionDir); err != nil {
		return false, err
	}
	return scan.Scanned, nil
}

// fetch resolves a parsed source into staged files, an identity, and the
// validated skill name.
func (l *Library) fetch(ctx context.Context, source Source) (fetched, error) {
	var resolved fetched
	switch source.Kind {
	case SourceGitHub:
		value, err := l.fetchGitHub(ctx, source.Location)
		if err != nil {
			return fetched{}, err
		}
		resolved = value
	case SourceDirectory:
		files, err := readSkillDirectory(source.Location)
		if err != nil {
			return fetched{}, err
		}
		version, hash := contentIdentity(files)
		resolved = fetched{source: source.Location, version: version, resolved: hash, files: files}
	case SourceZip:
		files, err := readSkillZip(source.Location)
		if err != nil {
			return fetched{}, err
		}
		version, hash := contentIdentity(files)
		resolved = fetched{source: source.Location, version: version, resolved: hash, files: files}
	default:
		return fetched{}, fmt.Errorf("unsupported skill source")
	}
	body, ok := resolved.files["SKILL.md"]
	if !ok {
		return fetched{}, errors.New("skill source has no SKILL.md at its root")
	}
	name, err := validateSkillFile(body)
	if err != nil {
		return fetched{}, err
	}
	resolved.name = name
	if source.Uploaded {
		resolved.source = uploadSource(name)
	}
	return resolved, nil
}
