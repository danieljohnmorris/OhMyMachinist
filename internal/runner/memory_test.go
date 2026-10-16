package runner

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestClassifyResourceFailure(t *testing.T) {
	cases := []struct {
		name     string
		stats    MemoryStats
		exitCode int
		tail     string
		wantOOM  bool
		wantPeak int64
	}{
		{"cgroup oom kill", MemoryStats{PeakBytes: 24 << 30, OOMKills: 1}, 137, "", true, 24 << 30},
		{"sigkill with oom record", MemoryStats{PeakBytes: 11 << 30}, 137, "Out of memory: Killed process 42", true, 11 << 30},
		{"oom_kill marker", MemoryStats{}, 137, "memory.events oom_kill 3", true, 0},
		{"sigkill without record", MemoryStats{}, 137, "killed by signal", false, 0},
		{"ordinary failure", MemoryStats{OOMKills: 0}, 1, "compile error", false, 0},
		{"success is never oom", MemoryStats{OOMKills: 2}, 0, "out of memory", false, 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			class, peak, ok := classifyResourceFailure(testCase.stats, testCase.exitCode, testCase.tail)
			if ok != testCase.wantOOM {
				t.Fatalf("classified=%v, want %v", ok, testCase.wantOOM)
			}
			if !ok {
				return
			}
			if class != ErrorClassOOM {
				t.Fatalf("class = %q, want %q", class, ErrorClassOOM)
			}
			if peak != testCase.wantPeak {
				t.Fatalf("peak = %d, want %d", peak, testCase.wantPeak)
			}
		})
	}
}

func TestOOMFailureMessageNamesCommandAndPeak(t *testing.T) {
	message := oomFailureMessage("omp-glm-pr", 24<<30)
	if message != "out of memory in omp-glm-pr, peak 24.0 GB" {
		t.Fatalf("message = %q", message)
	}
	if got := oomFailureMessage("  ", 0); got != "out of memory in the job, peak 0.0 GB" {
		t.Fatalf("empty-command message = %q", got)
	}
}

func TestMemoryLimitEmpty(t *testing.T) {
	if !(MemoryLimit{}).Empty() {
		t.Fatal("zero limit should be empty")
	}
	if (MemoryLimit{Max: 1}).Empty() {
		t.Fatal("non-zero limit should not be empty")
	}
}

func TestTailWriterKeepsLastBytes(t *testing.T) {
	var destination bytes.Buffer
	writer := newTailWriter(&destination, 8)
	_, _ = writer.Write([]byte("abcdef"))
	_, _ = writer.Write([]byte("ghijkl"))
	if writer.String() != "efghijkl" {
		t.Fatalf("tail = %q", writer.String())
	}
	if destination.String() != "abcdefghijkl" {
		t.Fatalf("destination = %q", destination.String())
	}
}

// TestMemoryScopeKillsRunawayJob is the MACH-62 acceptance test for the
// per-job cap: a job that allocates without limit is killed by the kernel
// while its parent (the worker stand-in) keeps running. It skips when the host
// provides no writable cgroup v2 delegation.
func TestMemoryScopeKillsRunawayJob(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("per-job memory scopes require Linux cgroup v2")
	}
	scope, err := createMemoryScope(MemoryLimit{Max: 128 << 20, SwapMax: 0}, "oomtest")
	if err != nil {
		t.Skipf("no writable cgroup v2 delegation: %v", err)
	}
	if scope == nil {
		t.Skip("memory scopes unavailable")
	}
	defer func() { _ = scope.close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", "-c", `chunk=$(head -c 1000000 /dev/zero | tr '\0' a); blob=""; while :; do blob="$blob$chunk"; done`)
	command.Env = sanitizedEnvironment(os.Environ())
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := scope.attach(command.Process.Pid); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Skipf("cannot attach to scope: %v", err)
	}
	_ = command.Wait()

	stats := scope.stats()
	if stats.OOMKills == 0 {
		t.Fatalf("expected an OOM kill in the scope, stats = %#v", stats)
	}
}
