package runner

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/config"
)

func TestExecuteWritesRedactedRawAgentLog(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a", 36)
	script := "cat >/dev/null; printf 'line one\\n'; printf '%s\\n' '" + secret + "'; printf 'stderr line\\n' >&2"
	command := config.ResolvedCommand{
		Name:       "plan",
		Command:    []string{"/bin/sh", "-c", script},
		Prompt:     "go\n",
		Timeout:    5 * time.Second,
		Definition: "/definition/config.toml",
		Hash:       "test-hash",
	}
	result, err := Execute(context.Background(), Options{
		Command:       command,
		Repository:    newGitRepository(t),
		DataDirectory: t.TempDir(),
		Stdout:        io.Discard,
		Stderr:        io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.RawLogPath == "" {
		t.Fatal("raw log path is empty")
	}
	body, err := os.ReadFile(result.RawLogPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "line one") {
		t.Fatalf("raw log missing stdout: %q", text)
	}
	if strings.Contains(text, secret) || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("raw log not redacted: %q", text)
	}
	if strings.Contains(text, "stderr line") {
		t.Fatalf("raw log captured stderr: %q", text)
	}
}

func TestExecuteCapsRawAgentLog(t *testing.T) {
	const limit = int64(1 << 20)
	script := "cat >/dev/null; yes 'x' | head -c 2000000"
	command := config.ResolvedCommand{
		Name:       "plan",
		Command:    []string{"/bin/sh", "-c", script},
		Prompt:     "go\n",
		Timeout:    5 * time.Second,
		Definition: "/definition/config.toml",
		Hash:       "test-hash",
	}
	result, err := Execute(context.Background(), Options{
		Command:        command,
		Repository:     newGitRepository(t),
		DataDirectory:  t.TempDir(),
		RawLogMaxBytes: limit,
		Stdout:         io.Discard,
		Stderr:         io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(result.RawLogPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "[raw agent log truncated at 1 MB]") {
		t.Fatalf("missing truncation marker: %q", text[len(text)-80:])
	}
	if int64(len(text)) > limit+128 {
		t.Fatalf("raw log not capped: %d bytes", len(text))
	}
}
