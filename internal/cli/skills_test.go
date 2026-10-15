package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/owainlewis/machinist/internal/skills"
)

func TestSkillsCommands(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(home, "data")
	t.Setenv("HOME", home)
	t.Setenv("MACHINIST_DATA_DIR", dataDir)

	skillDir := filepath.Join(home, "reviewer")
	source := "---\nname: feature-dev:code-reviewer\ndescription: Reviews changes\n---\n\nReview the diff.\n"
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(args ...string) (string, string, int) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := Execute(t.Context(), args, strings.NewReader(""), &stdout, &stderr, "test")
		return stdout.String(), stderr.String(), code
	}

	stdout, stderr, code := run("skills", "install", skillDir)
	if code != 0 {
		t.Fatalf("install exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "installed skill feature-dev:code-reviewer") {
		t.Fatalf("install output = %q", stdout)
	}

	first := readManifestVersion(t, dataDir)

	stdout, stderr, code = run("skills", "list")
	if code != 0 || !strings.Contains(stdout, "feature-dev:code-reviewer") || !strings.Contains(stdout, "active="+first) {
		t.Fatalf("list exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	updated := strings.Replace(source, "Review the diff.", "Review the diff, carefully.", 1)
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = run("skills", "update", "feature-dev:code-reviewer")
	if code != 0 || !strings.Contains(stdout, "active "+first) {
		t.Fatalf("update exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	stdout, stderr, code = run("skills", "pin", "feature-dev:code-reviewer", first)
	if code != 0 || !strings.Contains(stdout, "pinned skill") {
		t.Fatalf("pin exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}

	if _, stderr, code = run("skills", "uninstall", "feature-dev:code-reviewer"); code == 0 {
		t.Fatalf("uninstall removed a referenced skill: stderr = %q", stderr)
	}
	if !strings.Contains(stderr, "referenced") {
		t.Fatalf("uninstall stderr = %q", stderr)
	}

	stdout, stderr, code = run("skills", "uninstall", "feature-dev:code-reviewer", "--force")
	if code != 0 || !strings.Contains(stdout, "uninstalled skill") {
		t.Fatalf("uninstall --force exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "skills", "feature-dev_code-reviewer")); !os.IsNotExist(err) {
		t.Fatalf("skill directory still present: %v", err)
	}
}

func TestReviewInstallSkillAlias(t *testing.T) {
	home := t.TempDir()
	dataDir := filepath.Join(home, "data")
	t.Setenv("HOME", home)
	t.Setenv("MACHINIST_DATA_DIR", dataDir)
	skillDir := filepath.Join(home, "reviewer")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: docs-reviewer\ndescription: Docs\n---\n\nCheck docs.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Execute(t.Context(), []string{"review", "install-skill", skillDir}, strings.NewReader(""), &stdout, &stderr, "test"); code != 0 {
		t.Fatalf("alias exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "installed skill docs-reviewer") {
		t.Fatalf("alias output = %q", stdout.String())
	}
}

func readManifestVersion(t *testing.T, dataDir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dataDir, "skills", "feature-dev_code-reviewer", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest skills.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatal(err)
	}
	return manifest.Versions[0].Version
}
