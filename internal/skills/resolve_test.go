package skills

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func installTestSkill(t *testing.T, library *Library, name, version string) {
	t.Helper()
	dir := filepath.Join(library.Root, keyOf(t, name))
	if err := os.MkdirAll(filepath.Join(dir, version), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: test\n---\n\nbody of " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, version, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, _, err := library.load(name)
	if err != nil {
		manifest = Manifest{Name: name, Source: "test"}
	}
	manifest.Active = version
	manifest.Versions = append(manifest.Versions, Version{Version: version})
	if err := writeManifest(dir, manifest); err != nil {
		t.Fatal(err)
	}
}

func keyOf(t *testing.T, name string) string {
	t.Helper()
	key, err := Key(name)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func resolvedNames(resolution Resolution) []string {
	names := make([]string, 0, len(resolution.Skills))
	for _, skill := range resolution.Skills {
		names = append(names, skill.Name)
	}
	return names
}

func TestResolveComposesPrecedenceOrderAndDedupe(t *testing.T) {
	library := newTestLibrary(t)
	installTestSkill(t, library, "alpha", "v1")
	installTestSkill(t, library, "beta", "v1")
	installTestSkill(t, library, "gamma", "v1")

	resolver := &Resolver{Library: library}
	resolution, err := resolver.Resolve(ResolveRequest{
		Kind:           "build",
		Repo:           []Entry{{Name: "alpha"}, {Name: "beta"}},
		ProjectEntries: []Entry{{Name: "beta"}, {Name: "gamma"}},
		Global:         []Entry{{Name: "alpha"}, {Name: "delta"}},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"alpha", "beta", "gamma"}
	if got := resolvedNames(resolution); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("resolved %v, want %v", got, want)
	}
	if len(resolution.Warnings) != 1 || !strings.Contains(resolution.Warnings[0], "delta") {
		t.Fatalf("warnings = %v, want an unknown-skill warning for delta", resolution.Warnings)
	}
	if resolution.Skills[0].Scope != "repo" || resolution.Skills[2].Scope != "repo" {
		t.Fatalf("scopes = %q, %q; want repo", resolution.Skills[0].Scope, resolution.Skills[2].Scope)
	}

	// A global-only skill keeps the global scope.
	resolution, err = resolver.Resolve(ResolveRequest{Kind: "build", Global: []Entry{{Name: "gamma"}}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolution.Skills) != 1 || resolution.Skills[0].Scope != "global" {
		t.Fatalf("resolved = %+v, want one global skill", resolution.Skills)
	}
}

func TestResolveEnabledFalseDisablesInherited(t *testing.T) {
	library := newTestLibrary(t)
	installTestSkill(t, library, "alpha", "v1")
	installTestSkill(t, library, "beta", "v1")
	resolver := &Resolver{Library: library}

	disabled := false
	resolution, err := resolver.Resolve(ResolveRequest{
		Kind:           "build",
		Repo:           []Entry{{Name: "alpha", Enabled: &disabled}},
		ProjectEntries: []Entry{{Name: "alpha"}},
		Global:         []Entry{{Name: "beta"}},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := resolvedNames(resolution); strings.Join(got, ",") != "beta" {
		t.Fatalf("resolved %v, want only beta", got)
	}
}

func TestResolveVersionPin(t *testing.T) {
	library := newTestLibrary(t)
	installTestSkill(t, library, "alpha", "v1")
	installTestSkill(t, library, "alpha", "v2")
	// Reading a specific version returns that version's content.
	content, version, err := library.Read("alpha", "v1")
	if err != nil || version != "v1" || !strings.Contains(content, "body of alpha") {
		t.Fatalf("Read(alpha, v1) = %q, %q, %v", content, version, err)
	}

	resolver := &Resolver{Library: library}
	resolution, err := resolver.Resolve(ResolveRequest{Kind: "build", Repo: []Entry{{Name: "alpha@v1"}}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolution.Skills) != 1 || resolution.Skills[0].Version != "v1" {
		t.Fatalf("resolved = %+v, want alpha at v1", resolution.Skills)
	}
	if _, err := resolver.Resolve(ResolveRequest{Kind: "build", Repo: []Entry{{Name: "alpha@missing"}}}); err != nil {
		t.Fatalf("optional missing version should warn, got %v", err)
	}
}

func TestResolveRepoBundledSkill(t *testing.T) {
	library := newTestLibrary(t)
	bundle := RepoBundle{Commit: "deadbeef", Skills: map[string]string{"alpha": "bundled alpha"}}
	resolver := &Resolver{Library: library}
	resolution, err := resolver.Resolve(ResolveRequest{
		Kind:   "build",
		Repo:   []Entry{{Name: "alpha", Rule: "always run tests"}},
		Bundle: bundle,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolution.Skills) != 1 {
		t.Fatalf("resolved = %+v, want one bundled skill", resolution.Skills)
	}
	skill := resolution.Skills[0]
	if skill.Source != "repository" || skill.Scope != "repo" || skill.Version != "deadbeef" || skill.Content != "bundled alpha" {
		t.Fatalf("bundled skill = %+v", skill)
	}
	if skill.Rule != "always run tests" {
		t.Fatalf("rule = %q", skill.Rule)
	}
}

func TestResolveRequiredMissingFailsAndOptionalWarns(t *testing.T) {
	resolver := &Resolver{Library: newTestLibrary(t)}
	if _, err := resolver.Resolve(ResolveRequest{Kind: "build", Repo: []Entry{{Name: "ghost", Required: true}}}); err == nil {
		t.Fatal("required missing skill did not fail")
	}
	resolution, err := resolver.Resolve(ResolveRequest{Kind: "build", Repo: []Entry{{Name: "ghost"}}})
	if err != nil {
		t.Fatalf("optional missing skill failed: %v", err)
	}
	if len(resolution.Skills) != 0 || len(resolution.Warnings) != 1 {
		t.Fatalf("resolution = %+v, want no skills and one warning", resolution)
	}
}

func TestResolveAppliesToFilter(t *testing.T) {
	library := newTestLibrary(t)
	installTestSkill(t, library, "alpha", "v1")
	installTestSkill(t, library, "beta", "v1")
	resolver := &Resolver{Library: library}
	entries := []Entry{{Name: "alpha", AppliesTo: []string{"review"}}, {Name: "beta", AppliesTo: []string{"build", "review"}}}

	build, err := resolver.Resolve(ResolveRequest{Kind: "build", Repo: entries})
	if err != nil {
		t.Fatalf("Resolve build: %v", err)
	}
	if got := resolvedNames(build); strings.Join(got, ",") != "beta" {
		t.Fatalf("build resolved %v, want only beta", got)
	}
	review, err := resolver.Resolve(ResolveRequest{Kind: "review", Repo: entries})
	if err != nil {
		t.Fatalf("Resolve review: %v", err)
	}
	if got := resolvedNames(review); strings.Join(got, ",") != "alpha,beta" {
		t.Fatalf("review resolved %v", got)
	}
}

func TestParseConfigAcceptsSequenceAndMapping(t *testing.T) {
	entries, err := ParseConfig([]byte("skills:\n  - name: alpha\n    applies_to: [Review]\n    rule: check\n"))
	if err != nil {
		t.Fatalf("ParseConfig mapping: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "alpha" || entries[0].AppliesTo[0] != "review" {
		t.Fatalf("entries = %+v", entries)
	}
	entries, err = ParseConfig([]byte("- name: beta@v2\n  enabled: false\n"))
	if err != nil {
		t.Fatalf("ParseConfig sequence: %v", err)
	}
	if len(entries) != 1 || entries[0].Enabled == nil || *entries[0].Enabled {
		t.Fatalf("entries = %+v", entries)
	}
	empty, err := ParseConfig(nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("ParseConfig(nil) = %v, %v", empty, err)
	}
}

func TestBuildInjectionNativeWritesSkillsAndRules(t *testing.T) {
	library := newTestLibrary(t)
	installTestSkill(t, library, "feature-dev:code-reviewer", "v1")
	resolver := &Resolver{Library: library}
	resolution, err := resolver.Resolve(ResolveRequest{
		Kind: "build",
		Repo: []Entry{{Name: "feature-dev:code-reviewer", Rule: "Check error paths"}},
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	dir := t.TempDir()
	injected, err := BuildInjection(resolution, filepath.Join(dir, "agent"), true)
	if err != nil {
		t.Fatalf("BuildInjection: %v", err)
	}
	if injected.Skills[0].Method != "native" {
		t.Fatalf("method = %q, want native", injected.Skills[0].Method)
	}
	skillPath := filepath.Join(dir, "agent", "skills", keyOf(t, "feature-dev:code-reviewer"), "SKILL.md")
	body, err := os.ReadFile(skillPath)
	if err != nil {
		t.Fatalf("read injected skill: %v", err)
	}
	if !strings.Contains(string(body), "body of feature-dev:code-reviewer") {
		t.Fatalf("injected skill body = %q", body)
	}
	rules, err := os.ReadFile(filepath.Join(dir, "agent", "RULES.md"))
	if err != nil {
		t.Fatalf("read RULES.md: %v", err)
	}
	if !strings.Contains(string(rules), "Check error paths") {
		t.Fatalf("RULES.md = %q", rules)
	}
	overlay, err := os.ReadFile(filepath.Join(dir, "agent", "omp.yml"))
	if err != nil {
		t.Fatalf("read omp.yml: %v", err)
	}
	if !strings.Contains(string(overlay), filepath.ToSlash(filepath.Join(dir, "agent", "skills"))) {
		t.Fatalf("omp.yml = %q", overlay)
	}
}

func TestBuildInjectionPromptFallback(t *testing.T) {
	library := newTestLibrary(t)
	installTestSkill(t, library, "alpha", "v1")
	resolution, err := (&Resolver{Library: library}).Resolve(ResolveRequest{Kind: "build", Repo: []Entry{{Name: "alpha"}}})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	injected, err := BuildInjection(resolution, "", false)
	if err != nil {
		t.Fatalf("BuildInjection: %v", err)
	}
	if injected.Skills[0].Method != "prompt" {
		t.Fatalf("method = %q, want prompt", injected.Skills[0].Method)
	}
	for _, want := range []string{"alpha", "v1", "library", "body of alpha"} {
		if !strings.Contains(injected.Prompt, want) {
			t.Fatalf("prompt %q missing %q", injected.Prompt, want)
		}
	}
}

func TestLoadRepoBundleReadsBaseRef(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, ".machinist", "skills.yaml"), "skills:\n  - name: bundled\n")
	writeFile(t, filepath.Join(dir, ".machinist", "skills", "bundled", "SKILL.md"), "---\nname: bundled\ndescription: repo skill\n---\n\ndo the thing\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "--quiet", "-m", "add skills")

	bundle, err := LoadRepoBundle(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatalf("LoadRepoBundle: %v", err)
	}
	if bundle.Commit == "" {
		t.Fatal("bundle commit is empty")
	}
	if len(bundle.Entries) != 1 || bundle.Entries[0].Name != "bundled" {
		t.Fatalf("entries = %+v", bundle.Entries)
	}
	if !strings.Contains(bundle.Skills["bundled"], "do the thing") {
		t.Fatalf("bundled skills = %+v", bundle.Skills)
	}
}

func TestLoadRepoBundleUnknownRefFails(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "file.txt"), "hi\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "--quiet", "-m", "init")

	if _, err := LoadRepoBundle(context.Background(), dir, "no-such-ref"); err == nil {
		t.Fatal("LoadRepoBundle on an unknown ref: want error, got nil")
	}
}

func TestLoadRepoBundleNotARepositoryFails(t *testing.T) {
	if _, err := LoadRepoBundle(context.Background(), t.TempDir(), "HEAD"); err == nil {
		t.Fatal("LoadRepoBundle outside a repository: want error, got nil")
	}
}

func TestLoadRepoBundleEmptyRepositoryIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")

	bundle, err := LoadRepoBundle(context.Background(), dir, "HEAD")
	if err != nil {
		t.Fatalf("LoadRepoBundle in an empty repository: %v", err)
	}
	if bundle.Commit != "" || len(bundle.Entries) != 0 || len(bundle.Skills) != 0 {
		t.Fatalf("bundle = %+v, want empty", bundle)
	}
}

func TestLoadRepoBundleLsTreeFailureFails(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "file.txt"), "hi\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "--quiet", "-m", "init")

	// A ref that rev-parse accepts (a blob path) but ls-tree cannot resolve
	// must not be silently treated as "no bundled skills".
	if _, err := LoadRepoBundle(context.Background(), dir, "HEAD:file.txt"); err == nil {
		t.Fatal("LoadRepoBundle with an unresolvable tree: want error, got nil")
	}
}

func TestLoadRepoBundleUnreadableConfigFails(t *testing.T) {
	dir := t.TempDir()
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	writeFile(t, filepath.Join(dir, "file.txt"), "hi\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "--quiet", "-m", "init")

	// Record the config path as a gitlink to an object that is not present:
	// ls-tree lists the path (so it is not absent) but git show cannot read it.
	// That must surface rather than silently dropping every repo entry.
	runGit(t, dir, "update-index", "--add", "--cacheinfo", "160000,1111111111111111111111111111111111111111,.machinist/skills.yaml")
	runGit(t, dir, "commit", "--quiet", "-m", "gitlink")

	if _, err := LoadRepoBundle(context.Background(), dir, "HEAD"); err == nil {
		t.Fatal("LoadRepoBundle with an unreadable config: want error, got nil")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
