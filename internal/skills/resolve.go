package skills

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// This file composes the ordered skills a run loads from three layers:
//
//  1. .machinist/skills.yaml in the repository at the base ref,
//  2. the project setting skills.project.<project>,
//  3. the global setting skills.default.
//
// The layers compose in that order (repo, then project, then global), de-dupe
// by name, and a higher layer disables a name from a lower one with
// enabled: false. A repo-bundled .machinist/skills/<name>/SKILL.md counts as a
// repo-scoped skill whose version is the repository commit. Nothing is fetched
// at run time: an unknown entry fails the run when required and warns when not.

// Entry is one skills list entry, from a repository skills.yaml or a settings
// value. Name may carry an explicit version as name@version.
type Entry struct {
	Name      string   `yaml:"name" json:"name"`
	AppliesTo []string `yaml:"applies_to" json:"applies_to"`
	Rule      string   `yaml:"rule" json:"rule"`
	Required  bool     `yaml:"required" json:"required"`
	Enabled   *bool    `yaml:"enabled" json:"enabled"`
}

// SkillsConfig is a mapping document ({skills: [...]}) such as skills.yaml.
type SkillsConfig struct {
	Skills []Entry `yaml:"skills" json:"skills"`
}

// ParseConfig parses a skills list: either a bare YAML/JSON sequence of entries
// or a mapping with a skills key. An empty document yields no entries.
func ParseConfig(body []byte) ([]Entry, error) {
	if strings.TrimSpace(string(body)) == "" {
		return nil, nil
	}
	var list []Entry
	if err := yaml.Unmarshal(body, &list); err == nil {
		return normalizeEntries(list), nil
	}
	var config SkillsConfig
	if err := yaml.Unmarshal(body, &config); err != nil {
		return nil, fmt.Errorf("parse skills config: %w", err)
	}
	return normalizeEntries(config.Skills), nil
}

func normalizeEntries(entries []Entry) []Entry {
	normalized := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		entry.Name = strings.TrimSpace(entry.Name)
		if len(entry.AppliesTo) > 0 {
			applies := make([]string, 0, len(entry.AppliesTo))
			for _, value := range entry.AppliesTo {
				applies = append(applies, strings.ToLower(strings.TrimSpace(value)))
			}
			entry.AppliesTo = applies
		}
		normalized = append(normalized, entry)
	}
	return normalized
}

// SplitName splits name@version into its name and version parts. Version is
// empty when the entry does not pin one.
func SplitName(value string) (string, string) {
	value = strings.TrimSpace(value)
	if index := strings.LastIndex(value, "@"); index > 0 {
		return strings.TrimSpace(value[:index]), strings.TrimSpace(value[index+1:])
	}
	return value, ""
}

// RepoBundle is the repo-scoped skills configuration found at a git ref: the
// parsed .machinist/skills.yaml entries, the repo-bundled skills keyed by name,
// and the commit that identifies their version.
type RepoBundle struct {
	Commit  string
	Entries []Entry
	Skills  map[string]string
}

// repoSkillsPath and repoConfigPath are the repository-scoped skill locations.
const (
	repoConfigPath = ".machinist/skills.yaml"
	repoSkillsPath = ".machinist/skills"
)

// LoadRepoBundle reads .machinist/skills.yaml and every .machinist/skills/<name>/
// SKILL.md from the checkout at ref (git show), so the base-ref files are read
// even when the working tree has moved on. A missing file is not an error; an
// unreadable ref or malformed config is.
func LoadRepoBundle(ctx context.Context, dir, ref string) (RepoBundle, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = "HEAD"
	}
	bundle := RepoBundle{Skills: map[string]string{}}
	commit, err := gitShow(ctx, dir, "rev-parse", ref)
	if err != nil {
		// A repository with no commits has no repo-scoped skills; every other
		// failure (unknown ref, missing git, not a repository) must surface so
		// a repo's skills.yaml and bundled skills are never silently skipped.
		if empty, probeErr := isEmptyRepository(ctx, dir); probeErr == nil && empty {
			return bundle, nil
		}
		return RepoBundle{}, fmt.Errorf("resolve repository ref %q: %w", ref, err)
	}
	bundle.Commit = strings.TrimSpace(commit)
	// Distinguish an absent config from a git failure the same way the skills
	// listing below does: ls-tree exits 0 with empty output when the path is
	// absent, so a non-zero exit must surface rather than silently dropping
	// every repo entry, including required ones.
	tracked, err := gitShow(ctx, dir, "ls-tree", "--name-only", ref, "--", repoConfigPath)
	if err != nil {
		return RepoBundle{}, fmt.Errorf("probe repo skills config at %q: %w", ref, err)
	}
	if strings.TrimSpace(tracked) != "" {
		body, err := gitShow(ctx, dir, "show", ref+":"+repoConfigPath)
		if err != nil {
			return RepoBundle{}, fmt.Errorf("read repo skills config at %q: %w", ref, err)
		}
		entries, err := ParseConfig([]byte(body))
		if err != nil {
			return RepoBundle{}, err
		}
		bundle.Entries = entries
	}
	// An absent path is not an error (ls-tree exits 0 with empty output); any
	// non-zero exit is a real failure and must surface rather than dropping
	// every repo-bundled skill.
	tree, err := gitShow(ctx, dir, "ls-tree", "-r", "--name-only", ref, "--", repoSkillsPath)
	if err != nil {
		return RepoBundle{}, fmt.Errorf("list repo-bundled skills at %q: %w", ref, err)
	}
	for _, path := range strings.Split(tree, "\n") {
		path = strings.TrimSpace(path)
		if !strings.HasSuffix(path, "/SKILL.md") {
			continue
		}
		body, err := gitShow(ctx, dir, "show", ref+":"+path)
		if err != nil {
			return RepoBundle{}, err
		}
		name, err := validateSkillFile([]byte(body))
		if err != nil {
			return RepoBundle{}, fmt.Errorf("repo-bundled skill %s: %w", path, err)
		}
		bundle.Skills[name] = body
	}
	return bundle, nil
}

// gitShow runs git and returns stdout. Missing objects return an error the
// caller can treat as "absent".
func gitShow(ctx context.Context, dir string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	command.Env = os.Environ()
	output, err := command.Output()
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// isEmptyRepository reports whether dir is a git repository with no commits.
// It lets LoadRepoBundle tolerate a genuinely empty repository while still
// surfacing every other rev-parse failure.
func isEmptyRepository(ctx context.Context, dir string) (bool, error) {
	output, err := gitShow(ctx, dir, "rev-list", "--count", "--all")
	if err != nil {
		return false, err
	}
	count, err := strconv.Atoi(strings.TrimSpace(output))
	if err != nil {
		return false, err
	}
	return count == 0, nil
}

// ResolveRequest describes one run's skills layers.
type ResolveRequest struct {
	// Project is the project key for the skills.project.<project> setting.
	Project string
	// Kind is "build" or "review"; empty loads every entry.
	Kind string
	// Repo, ProjectEntries and Global are the three layers, highest first.
	Repo           []Entry
	ProjectEntries []Entry
	Global         []Entry
	// Bundle is the repo-scoped configuration and skills at the base ref.
	Bundle RepoBundle
}

// Selected is one resolved skill with its content and provenance.
type Selected struct {
	Name     string
	Version  string
	Scope    string // repo | global
	Source   string // repository | library
	Rule     string
	Required bool
	Content  string
	// Method is set when the skill is injected: native or prompt.
	Method string
}

// Resolution is the result of composing and resolving a run's skills.
type Resolution struct {
	Skills   []Selected
	Warnings []string
}

// Resolver resolves installed skills from the versioned library. A nil Library
// means only repo-bundled skills can resolve.
type Resolver struct {
	Library *Library
	// Warn receives non-fatal notices. It may be nil.
	Warn func(string)
}

// Resolve composes the layers and resolves each entry to installed content.
// A repo-bundled skill wins over the library. Unknown required entries return
// an error; unknown optional entries are skipped with a warning.
func (r *Resolver) Resolve(request ResolveRequest) (Resolution, error) {
	var resolution Resolution
	for _, entry := range compose(request) {
		base, version := SplitName(entry.Name)
		if base == "" {
			continue
		}
		if content, ok := request.Bundle.Skills[base]; ok {
			resolution.Skills = append(resolution.Skills, Selected{
				Name: base, Version: request.Bundle.Commit, Scope: "repo", Source: "repository",
				Rule: entry.Rule, Required: entry.Required, Content: content,
			})
			continue
		}
		if r.Library != nil {
			content, resolvedVersion, err := r.Library.Read(base, version)
			if err == nil {
				resolution.Skills = append(resolution.Skills, Selected{
					Name: base, Version: resolvedVersion, Scope: entry.Scope, Source: "library",
					Rule: entry.Rule, Required: entry.Required, Content: content,
				})
				continue
			}
			if !errors.Is(err, ErrNotInstalled) && !errors.Is(err, ErrNoVersion) {
				return Resolution{}, err
			}
		}
		if entry.Required {
			return Resolution{}, fmt.Errorf("required skill %q is not installed", entry.Name)
		}
		warning := fmt.Sprintf("skill %q is not installed; skipping", entry.Name)
		resolution.Warnings = append(resolution.Warnings, warning)
		if r.Warn != nil {
			r.Warn(warning)
		}
	}
	return resolution, nil
}

type composed struct {
	Name     string
	Rule     string
	Required bool
	Scope    string
}

// compose merges the layers in precedence order and de-dupes by base name.
// The first layer to mention a name decides it, so a higher layer can both add
// a skill and disable an inherited one with enabled: false.
func compose(request ResolveRequest) []composed {
	layers := []struct {
		entries []Entry
		scope   string
	}{
		{request.Repo, "repo"},
		{request.ProjectEntries, "repo"},
		{request.Global, "global"},
	}
	seen := map[string]bool{}
	var out []composed
	for _, layer := range layers {
		for _, entry := range layer.entries {
			if !appliesTo(entry, request.Kind) {
				continue
			}
			base, _ := SplitName(entry.Name)
			if base == "" || seen[base] {
				continue
			}
			seen[base] = true
			if entry.Enabled != nil && !*entry.Enabled {
				continue
			}
			out = append(out, composed{Name: entry.Name, Rule: entry.Rule, Required: entry.Required, Scope: layer.scope})
		}
	}
	return out
}

// appliesTo reports whether an entry runs for the given kind. An empty
// applies_to (the default) or an empty kind matches everything.
func appliesTo(entry Entry, kind string) bool {
	if kind == "" || len(entry.AppliesTo) == 0 {
		return true
	}
	kind = strings.ToLower(strings.TrimSpace(kind))
	for _, value := range entry.AppliesTo {
		if value == kind {
			return true
		}
	}
	return false
}

// ompConfigName is the per-run omp overlay that points skills discovery at the
// injection's skills/ directory.
const ompConfigName = "omp.yml"

// rulesName is the per-run rules file every entry rule is appended to.
const rulesName = "RULES.md"

// Injected is the per-run skill payload written for an executor.
type Injected struct {
	// Dir is the per-run agent directory, or "" for a prompt-only fallback.
	Dir    string
	Files  map[string][]byte
	Skills []Selected
	// Prompt is appended to the executor prompt for non-native executors.
	Prompt   string
	Warnings []string
}

// BuildInjection writes the per-run agent directory for the resolved skills.
//
// Native executors (omp) get skills/<name>/SKILL.md for every skill, each rule
// appended to RULES.md, and an omp config overlay that adds skills/ to
// skills.customDirectories so skill://name resolves. Other executors get each
// SKILL.md appended to the returned prompt under a header naming its source and
// version. Nothing is written when no skill resolved.
func BuildInjection(resolution Resolution, dir string, native bool) (Injected, error) {
	injected := Injected{Files: map[string][]byte{}, Warnings: resolution.Warnings}
	if len(resolution.Skills) == 0 {
		return injected, nil
	}
	if !native {
		var prompt strings.Builder
		for _, skill := range resolution.Skills {
			skill.Method = "prompt"
			injected.Skills = append(injected.Skills, skill)
			fmt.Fprintf(&prompt, "### Skill %s (version %s, source %s, scope %s)\n\n%s\n\n", skill.Name, skill.Version, skill.Source, skill.Scope, strings.TrimSpace(skill.Content))
		}
		injected.Prompt = strings.TrimSpace(prompt.String())
		return injected, nil
	}
	injected.Dir = dir
	var rules strings.Builder
	for _, skill := range resolution.Skills {
		key, err := Key(skill.Name)
		if err != nil {
			return Injected{}, fmt.Errorf("skill %q: %w", skill.Name, err)
		}
		skill.Method = "native"
		injected.Skills = append(injected.Skills, skill)
		injected.Files[filepath.ToSlash(filepath.Join("skills", key, "SKILL.md"))] = []byte(skill.Content)
		if rule := strings.TrimSpace(skill.Rule); rule != "" {
			fmt.Fprintf(&rules, "## %s\n\n%s\n\n", skill.Name, rule)
		}
	}
	if rules.Len() > 0 {
		injected.Files[rulesName] = []byte(rules.String())
	}
	injected.Files[ompConfigName] = []byte(fmt.Sprintf("skills:\n  customDirectories:\n    - %s\n", filepath.ToSlash(filepath.Join(dir, "skills"))))
	if dir == "" {
		return injected, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Injected{}, fmt.Errorf("create skills agent directory: %w", err)
	}
	for name, body := range injected.Files {
		target := filepath.Join(dir, filepath.FromSlash(name))
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) {
			return Injected{}, fmt.Errorf("skill path %q escapes the agent directory", name)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return Injected{}, err
		}
		if err := os.WriteFile(target, body, 0o600); err != nil {
			return Injected{}, err
		}
	}
	return injected, nil
}
