package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/owainlewis/machinist/internal/controlplane"
	"github.com/owainlewis/machinist/internal/protocol"
)

func runFactoryCommand(t *testing.T, configPath string, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	exitCode := Execute(t.Context(), append([]string{args[0], "--config=" + configPath}, args[1:]...),
		strings.NewReader(""), &stdout, &stderr, "test")
	return exitCode, stdout.String(), stderr.String()
}

func TestCLIPauseWritesAuditAndState(t *testing.T) {
	configPath, database := writeTokenStoreConfig(t)
	store := openTestTokenStore(t, database)

	exitCode, stdout, stderr := runFactoryCommand(t, configPath, "pause", "--reason=spark build")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "state: draining") || !strings.Contains(stdout, "spark build") {
		t.Fatalf("stdout = %q, want the draining state with its reason", stdout)
	}

	status, err := store.FactoryStatus(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "draining" || status.Actor != "cli" {
		t.Fatalf("factory state = %#v", status)
	}

	exitCode, stdout, stderr = runFactoryCommand(t, configPath, "resume")
	if exitCode != 0 {
		t.Fatalf("resume exit code = %d, stderr = %q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "state: running") {
		t.Fatalf("resume stdout = %q", stdout)
	}

	entries, err := store.AuditLog(t.Context(), 20)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, entry := range entries {
		if action, ok := entry["action"].(string); ok {
			name, _ := entry["token_name"].(string)
			actions[action] = name
		}
	}
	if actions["factory.drain"] != "cli" || actions["factory.start"] != "cli" {
		t.Fatalf("audit entries = %#v", entries)
	}
}

func TestCLIPauseCancelAndJobCap(t *testing.T) {
	configPath, _ := writeTokenStoreConfig(t)

	exitCode, stdout, stderr := runFactoryCommand(t, configPath, "pause", "--cancel")
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "state: paused") {
		t.Fatalf("stdout = %q, want paused", stdout)
	}

	exitCode, stdout, stderr = runFactoryCommand(t, configPath, "job-cap", "3")
	if exitCode != 0 {
		t.Fatalf("job-cap exit code = %d, stderr = %q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "jobs-at-once: 3") {
		t.Fatalf("job-cap stdout = %q", stdout)
	}

	exitCode, _, stderr = runFactoryCommand(t, configPath, "job-cap", "0")
	if exitCode == 0 || !strings.Contains(stderr, "at least 1") {
		t.Fatalf("job-cap 0 exit code = %d, stderr = %q", exitCode, stderr)
	}
}

func TestCLILaneDisableAndList(t *testing.T) {
	configPath, _ := writeTokenStoreConfig(t)

	exitCode, _, stderr := runFactoryCommand(t, configPath, "lane", "disable", "lane-a")
	if exitCode != 0 {
		t.Fatalf("disable exit code = %d, stderr = %q", exitCode, stderr)
	}
	exitCode, stdout, stderr := runFactoryCommand(t, configPath, "lane", "list")
	if exitCode != 0 {
		t.Fatalf("list exit code = %d, stderr = %q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "state: running") {
		t.Fatalf("lane list stdout = %q", stdout)
	}
}

func writeFactoryCapConfig(t *testing.T, maxConcurrentJobs int) (string, string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "worker.token"), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(directory, "machinist.db")
	configPath := filepath.Join(directory, "config.toml")
	body := "[server]\n" +
		"database = " + strconv.Quote(filepath.ToSlash(database)) + "\n" +
		"worker_token_file = \"worker.token\"\n"
	if maxConcurrentJobs > 0 {
		body += "max_concurrent_jobs = " + strconv.Itoa(maxConcurrentJobs) + "\n"
	}
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, database
}

func registerLanes(t *testing.T, store *controlplane.Store, names ...string) {
	t.Helper()
	for index, name := range names {
		if _, err := store.Poll(t.Context(), protocol.PollRequest{
			InstanceID:   fmt.Sprintf("instance-%d", index),
			Name:         name,
			Executors:    []string{"codex"},
			Repositories: []string{"machinist"},
		}); err != nil {
			t.Fatalf("poll %s: %v", name, err)
		}
	}
}

func TestCLIFactoryPrintsConfiguredJobCap(t *testing.T) {
	configPath, database := writeFactoryCapConfig(t, 2)
	store := openTestTokenStore(t, database)
	registerLanes(t, store, "lane-a", "lane-b", "lane-c", "lane-d")

	for _, args := range [][]string{{"pause", "--reason=cap check"}, {"resume"}} {
		exitCode, stdout, stderr := runFactoryCommand(t, configPath, args...)
		if exitCode != 0 {
			t.Fatalf("%v exit code = %d, stderr = %q", args, exitCode, stderr)
		}
		if !strings.Contains(stdout, "jobs-at-once: 2") {
			t.Fatalf("%v stdout = %q, want jobs-at-once: 2", args, stdout)
		}
	}
}

func TestCLIJobCapAuditRecordsPreviousValue(t *testing.T) {
	configPath, database := writeFactoryCapConfig(t, 2)
	store := openTestTokenStore(t, database)
	registerLanes(t, store, "lane-a", "lane-b", "lane-c", "lane-d")

	exitCode, stdout, stderr := runFactoryCommand(t, configPath, "job-cap", "3")
	if exitCode != 0 {
		t.Fatalf("job-cap exit code = %d, stderr = %q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "jobs-at-once: 3") {
		t.Fatalf("job-cap stdout = %q, want jobs-at-once: 3", stdout)
	}

	exitCode, stdout, stderr = runFactoryCommand(t, configPath, "job-cap", "1")
	if exitCode != 0 {
		t.Fatalf("second job-cap exit code = %d, stderr = %q", exitCode, stderr)
	}

	entries, err := store.AuditLog(t.Context(), 20)
	if err != nil {
		t.Fatal(err)
	}
	var caps []string
	for _, entry := range entries {
		if entry["action"] == "factory.job_cap" {
			caps = append(caps, entry["detail"].(string))
		}
	}
	if len(caps) != 2 {
		t.Fatalf("job_cap audit entries = %#v, want two", entries)
	}
	if caps[1] != "old=2 new=3 requested=3" {
		t.Fatalf("first job_cap audit detail = %q, want old=2 new=3 requested=3", caps[1])
	}
	if caps[0] != "old=3 new=1 requested=1" {
		t.Fatalf("second job_cap audit detail = %q, want old=3 new=1 requested=1", caps[0])
	}
}

func TestCLIFactoryPrintsOverrideAndUnlimited(t *testing.T) {
	configPath, database := writeFactoryCapConfig(t, 0)
	store := openTestTokenStore(t, database)
	registerLanes(t, store, "lane-a", "lane-b")

	exitCode, stdout, stderr := runFactoryCommand(t, configPath, "pause")
	if exitCode != 0 {
		t.Fatalf("pause exit code = %d, stderr = %q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "jobs-at-once: unlimited") {
		t.Fatalf("pause stdout = %q, want jobs-at-once: unlimited", stdout)
	}

	if _, err := store.SetFactoryJobCap(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	exitCode, stdout, stderr = runFactoryCommand(t, configPath, "pause")
	if exitCode != 0 {
		t.Fatalf("pause with override exit code = %d, stderr = %q", exitCode, stderr)
	}
	if !strings.Contains(stdout, "jobs-at-once: 2") {
		t.Fatalf("pause with override stdout = %q, want jobs-at-once: 2", stdout)
	}
}
