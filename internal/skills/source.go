package skills

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// SourceKind identifies where a skill install comes from.
type SourceKind int

const (
	// SourceGitHub is a github.com SKILL.md or skill directory URL.
	SourceGitHub SourceKind = iota
	// SourceDirectory is a local directory (or a local SKILL.md path).
	SourceDirectory
	// SourceZip is a local zip archive of a skill directory.
	SourceZip
)

// Source is a parsed, re-resolvable skill source. Update re-resolves it.
type Source struct {
	Kind     SourceKind
	Location string
	// Uploaded marks a source read from bytes the caller uploaded and staged
	// in a temporary location. The staged file is removed once the install
	// finishes, so the recorded manifest source is a stable uploadSource
	// sentinel instead of that path, and Update reports that the skill must be
	// re-uploaded to add a version.
	Uploaded bool
}

// uploadSourcePrefix marks the recorded source of a skill installed from an
// upload. It is never a real path or URL, so callers can tell an upload apart
// from a re-resolvable source.
const uploadSourcePrefix = "upload:"

// uploadSource is the manifest source recorded for a skill installed from an
// uploaded archive or SKILL.md.
func uploadSource(name string) string { return uploadSourcePrefix + name }

// isUploadSource reports whether a recorded source came from an upload.
func isUploadSource(source string) bool {
	return strings.HasPrefix(source, uploadSourcePrefix)
}

// ParseSource classifies a CLI argument as a GitHub URL, a local directory, or
// a zip archive. Local paths are resolved to absolute paths so an update can
// re-read them from any working directory.
func ParseSource(input string) (Source, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return Source{}, fmt.Errorf("empty skill source")
	}
	if isGitHubURL(trimmed) {
		return Source{Kind: SourceGitHub, Location: trimmed}, nil
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return Source{}, fmt.Errorf("resolve skill source %q: %w", trimmed, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Source{}, fmt.Errorf("skill source %q: %w", trimmed, err)
	}
	if info.IsDir() {
		return Source{Kind: SourceDirectory, Location: abs}, nil
	}
	if strings.EqualFold(filepath.Ext(abs), ".zip") {
		return Source{Kind: SourceZip, Location: abs}, nil
	}
	if filepath.Base(abs) == "SKILL.md" {
		return Source{Kind: SourceDirectory, Location: filepath.Dir(abs)}, nil
	}
	return Source{}, fmt.Errorf("unsupported skill source %q: expected a GitHub URL, a skill directory, or a zip", trimmed)
}

// fetched is the resolved content of a source before it is written to the
// library.
type fetched struct {
	name     string
	source   string
	version  string
	resolved string
	files    map[string][]byte
}

// limits keep a skill install small and bounded.
const (
	maxSkillFileBytes = 1 << 20
	maxSkillFiles     = 2000
	maxSkillTotal     = 8 << 20
)

func readSkillDirectory(dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(current string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(dir, current)
		if err != nil {
			return err
		}
		return addSkillFile(files, filepath.ToSlash(relative), current)
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func addSkillFile(files map[string][]byte, name, source string) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, maxSkillFileBytes+1))
	if err != nil {
		return err
	}
	return storeSkillFile(files, name, body)
}

func storeSkillFile(files map[string][]byte, name string, body []byte) error {
	if len(body) > maxSkillFileBytes {
		return fmt.Errorf("skill file %q exceeds %d bytes", name, maxSkillFileBytes)
	}
	if len(files) >= maxSkillFiles {
		return fmt.Errorf("skill contains more than %d files", maxSkillFiles)
	}
	total := len(body)
	for _, existing := range files {
		total += len(existing)
	}
	if total > maxSkillTotal {
		return fmt.Errorf("skill exceeds %d bytes in total", maxSkillTotal)
	}
	files[name] = body
	return nil
}

func readSkillZip(archive string) (map[string][]byte, error) {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return nil, fmt.Errorf("read skill archive %q: %w", archive, err)
	}
	defer reader.Close()
	files := map[string][]byte{}
	for _, entry := range reader.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		name := path.Clean(strings.TrimPrefix(filepath.ToSlash(entry.Name), "./"))
		if name == "." || strings.HasPrefix(name, "../") || path.IsAbs(name) {
			return nil, fmt.Errorf("skill archive %q contains an unsafe path: %q", archive, entry.Name)
		}
		body, err := readZipFile(entry)
		if err != nil {
			return nil, fmt.Errorf("read %s from %q: %w", name, archive, err)
		}
		if err := storeSkillFile(files, name, body); err != nil {
			return nil, err
		}
	}
	return files, nil
}

func readZipFile(entry *zip.File) ([]byte, error) {
	file, err := entry.Open()
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(io.LimitReader(file, maxSkillFileBytes+1))
}

// contentIdentity hashes a skill tree deterministically so an unchanged
// source re-resolves to the same version.
func contentIdentity(files map[string][]byte) (version, resolved string) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	hash := sha256.New()
	for _, name := range names {
		fmt.Fprintf(hash, "%s\x00%d\x00", name, len(files[name]))
		hash.Write(files[name])
		hash.Write([]byte{0})
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	return "sha256-" + sum[:16], "sha256:" + sum
}
