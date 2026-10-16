package cli

import (
	"bytes"
	"strings"
	"testing"
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
