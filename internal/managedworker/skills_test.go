package managedworker

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/protocol"
	"github.com/owainlewis/machinist/internal/skills"
)

func TestPrepareSkillsInjectsRepoSkillIntoAgentDirectory(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("MACHINIST_DATA_DIR", directory)
	library, err := skills.Open(filepath.Join(directory, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	library.Scan = func(context.Context, string) (skills.ScanResult, error) {
		return skills.ScanResult{Scanned: true}, nil
	}
	source := filepath.Join(directory, "source")
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	skill := "---\nname: rust-review\ndescription: Reviews Rust\n---\n\nCheck unsafe blocks.\n"
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := library.Install(context.Background(), skills.Source{Kind: skills.SourceDirectory, Location: source}); err != nil {
		t.Fatal(err)
	}

	repository := filepath.Join(directory, "repository")
	if output, err := exec.Command("git", "init", "--quiet", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := os.MkdirAll(filepath.Join(repository, ".machinist"), 0o755); err != nil {
		t.Fatal(err)
	}
	configBody := "skills:\n  - name: rust-review\n    rule: Always test unsafe code.\n"
	if err := os.WriteFile(filepath.Join(repository, ".machinist", "skills.yaml"), []byte(configBody), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"add", "."},
		{"commit", "--quiet", "-m", "skills"},
	} {
		command := exec.Command("git", append([]string{"-C", repository}, args...)...)
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
		}
	}

	dataDirectory := filepath.Join(directory, "worker")
	worker := &Worker{config: config.Worker{DataDirectory: dataDirectory}, stderr: io.Discard}
	command := config.ResolvedCommand{Name: "build-pr", Executor: "omp-glm", Command: []string{"omp-glm", "medium"}, Output: config.ExecutorOutputOmpJSON}
	spec := protocol.RunSpec{ID: "run_0123456789abcdef01234567", Command: "build-pr", Executor: "omp-glm", Repository: "machinist"}

	env, recorded, err := worker.prepareSkills(context.Background(), spec, &command, repository)
	if err != nil {
		t.Fatalf("prepareSkills: %v", err)
	}
	if len(env) != 1 || !strings.HasPrefix(env[0], ompAgentDirEnv+"=") {
		t.Fatalf("env = %v, want %s", env, ompAgentDirEnv)
	}
	agentDir := strings.TrimPrefix(env[0], ompAgentDirEnv+"=")
	if agentDir != filepath.Join(dataDirectory, "runs", spec.ID, "agent") {
		t.Fatalf("agent dir = %q", agentDir)
	}
	if recorded == nil || len(recorded.Skills) != 1 {
		t.Fatalf("recorded = %#v", recorded)
	}
	if got := recorded.Skills[0]; got.Name != "rust-review" || got.Scope != "repo" || got.Method != "native" || got.Version == "" {
		t.Fatalf("recorded skill = %#v", got)
	}
	body, err := os.ReadFile(filepath.Join(agentDir, "skills", "rust-review", "SKILL.md"))
	if err != nil {
		t.Fatalf("read injected skill: %v", err)
	}
	if !strings.Contains(string(body), "Check unsafe blocks") {
		t.Fatalf("injected skill = %q", body)
	}
	rules, err := os.ReadFile(filepath.Join(agentDir, "RULES.md"))
	if err != nil {
		t.Fatalf("read RULES.md: %v", err)
	}
	if !strings.Contains(string(rules), "Always test unsafe code.") {
		t.Fatalf("RULES.md = %q", rules)
	}
}

func TestPrepareSkillsFiltersEntriesByRunKind(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("MACHINIST_DATA_DIR", directory)
	library, err := skills.Open(filepath.Join(directory, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	library.Scan = func(context.Context, string) (skills.ScanResult, error) {
		return skills.ScanResult{Scanned: true}, nil
	}
	installSkill(t, library, directory, "build-helper")
	installSkill(t, library, directory, "review-helper")

	repository := filepath.Join(directory, "repository")
	if output, err := exec.Command("git", "init", "--quiet", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := os.MkdirAll(filepath.Join(repository, ".machinist"), 0o755); err != nil {
		t.Fatal(err)
	}
	configBody := "skills:\n  - name: build-helper\n    applies_to: [build]\n  - name: review-helper\n    applies_to: [review]\n"
	if err := os.WriteFile(filepath.Join(repository, ".machinist", "skills.yaml"), []byte(configBody), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, repository)

	worker := &Worker{config: config.Worker{DataDirectory: filepath.Join(directory, "worker")}, stderr: io.Discard}
	cases := []struct {
		name     string
		command  string
		executor string
		want     string
	}{
		{name: "build command", command: "build-pr", executor: "omp-glm", want: "build-helper"},
		{name: "review command", command: "review-pr", executor: "omp-glm", want: "review-helper"},
		{name: "review executor", command: "deploy", executor: "omp-reviewer", want: "review-helper"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			command := config.ResolvedCommand{Name: tc.command, Executor: tc.executor, Command: []string{tc.executor, "medium"}}
			spec := protocol.RunSpec{ID: "run_0123456789abcdef01234567", Command: tc.command, Executor: tc.executor, Repository: "machinist"}
			_, recorded, err := worker.prepareSkills(context.Background(), spec, &command, repository)
			if err != nil {
				t.Fatalf("prepareSkills: %v", err)
			}
			if recorded == nil || len(recorded.Skills) != 1 || recorded.Skills[0].Name != tc.want {
				t.Fatalf("recorded = %#v, want only %q", recorded, tc.want)
			}
		})
	}
}

func installSkill(t *testing.T, library *skills.Library, directory, name string) {
	t.Helper()
	source := filepath.Join(directory, "sources", name)
	if err := os.MkdirAll(source, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("---\nname: %s\ndescription: %s skill\n---\n\n%s body\n", name, name, name)
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := library.Install(context.Background(), skills.Source{Kind: skills.SourceDirectory, Location: source}); err != nil {
		t.Fatal(err)
	}
}

func gitCommitAll(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"add", "."},
		{"commit", "--quiet", "-m", "skills"},
	} {
		command := exec.Command("git", append([]string{"-C", dir}, args...)...)
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, output)
		}
	}
}
