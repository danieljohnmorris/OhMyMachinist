package managedworker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/owainlewis/machinist/internal/config"
)

func gitLane(t *testing.T) string {
	t.Helper()
	repository := filepath.Join(t.TempDir(), "lane")
	if output, err := exec.Command("git", "init", "--quiet", "--initial-branch=main", repository).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repository, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("tracked.txt", "original\n")
	commands := [][]string{
		{"git", "-C", repository, "config", "user.email", "machinist@example.com"},
		{"git", "-C", repository, "config", "user.name", "Machinist"},
		{"git", "-C", repository, "add", "tracked.txt"},
		{"git", "-C", repository, "commit", "--quiet", "-m", "base"},
	}
	for _, command := range commands {
		if output, err := exec.Command(command[0], command[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("git setup: %v: %s", err, output)
		}
	}
	return repository
}

func TestSaveLaneWIPStashesTrackedAndUntrackedWork(t *testing.T) {
	repository := gitLane(t)
	dataDirectory := t.TempDir()
	workerConfig := config.Worker{
		DataDirectory: dataDirectory,
		Repositories:  map[string]config.Repository{"machinist": {Path: repository}},
	}

	// A clean checkout saves nothing.
	if saved, err := saveLaneWIP(workerConfig, "machinist"); err != nil || saved != "" {
		t.Fatalf("clean lane = %q, %v", saved, err)
	}

	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, "untracked.txt"), []byte("scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	saved, err := saveLaneWIP(workerConfig, "machinist")
	if err != nil {
		t.Fatalf("save lane wip: %v", err)
	}
	if !strings.Contains(saved, "machinist-lane-wip-") || !strings.Contains(saved, "patch") {
		t.Fatalf("saved record = %q", saved)
	}

	// The lane is clean: the next attempt starts from a clean checkout.
	status, err := exec.Command("git", "-C", repository, "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.TrimSpace(string(status))) != 0 {
		t.Fatalf("lane not clean after save: %q", status)
	}

	// The patch sits outside the published run artifacts, under the worker
	// data directory, and carries the tracked edit and the untracked file.
	entries, err := os.ReadDir(filepath.Join(dataDirectory, "wip"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("wip directory = %#v, %v", entries, err)
	}
	patch, err := os.ReadFile(filepath.Join(dataDirectory, "wip", entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	patchText := string(patch)
	if !strings.Contains(patchText, "edited") || !strings.Contains(patchText, "untracked.txt") {
		t.Fatalf("patch missing saved work: %q", patchText)
	}
	if !strings.Contains(saved, entries[0].Name()) {
		t.Fatalf("saved record %q does not name the patch %s", saved, entries[0].Name())
	}
}

func TestSaveLaneWIPWithoutDataDirectoryStillStashes(t *testing.T) {
	repository := gitLane(t)
	workerConfig := config.Worker{Repositories: map[string]config.Repository{"machinist": {Path: repository}}}
	if err := os.WriteFile(filepath.Join(repository, "tracked.txt"), []byte("edited\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	saved, err := saveLaneWIP(workerConfig, "machinist")
	if err != nil {
		t.Fatalf("save lane wip: %v", err)
	}
	if !strings.Contains(saved, "machinist-lane-wip-") {
		t.Fatalf("saved record = %q", saved)
	}
}
