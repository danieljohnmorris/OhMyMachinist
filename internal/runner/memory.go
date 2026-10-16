package runner

import (
	"fmt"
	"regexp"
	"strings"
)

// MemoryLimit is the resolved per-job memory cap in bytes (MACH-62). A zero
// field means that limit is unset, so Empty reports whether the whole cap is
// disabled and no per-job scope is needed.
type MemoryLimit struct {
	High    int64
	Max     int64
	SwapMax int64
}

// Empty reports whether no memory limit is configured.
func (limit MemoryLimit) Empty() bool {
	return limit.High == 0 && limit.Max == 0 && limit.SwapMax == 0
}

// MemoryStats is what a job's memory scope used and whether the kernel
// OOM-killed anything inside it.
type MemoryStats struct {
	PeakBytes int64
	OOMKills  int64
}

// memoryScope is a per-run memory-limited cgroup (MACH-62). Only Linux
// provides one; elsewhere createMemoryScope returns nil. A nil scope means no
// per-job cap and the worker-level systemd backstop is the only bound.
type memoryScope interface {
	attach(pid int) error
	stats() MemoryStats
	close() error
}

// ErrorClassOOM marks a run the kernel killed for using too much memory. The
// failure hook and Plane comment use it to explain the failure and to hold the
// job instead of retrying it (MACH-62).
const ErrorClassOOM = "oom"

// oomRecordPattern matches the OOM records a killed process or the kernel may
// leave in a run's stderr when no cgroup counter is available.
var oomRecordPattern = regexp.MustCompile(`(?i)out of memory|oom[_-]?kill|killed process`)

// oomRecord reports whether text carries an out-of-memory record.
func oomRecord(text string) bool {
	return oomRecordPattern.MatchString(text)
}

// classifyResourceFailure reports whether a failed run was an out-of-memory
// kill and the peak memory to record. A cgroup oom_kill counter is
// authoritative; without one, a SIGKILL (exit 137) plus an OOM record in the
// captured output is the signal. It never classifies a successful run.
func classifyResourceFailure(stats MemoryStats, exitCode int, stderrTail string) (string, int64, bool) {
	if exitCode == 0 {
		return "", 0, false
	}
	if stats.OOMKills > 0 {
		return ErrorClassOOM, stats.PeakBytes, true
	}
	if exitCode == terminatedExitCode() && oomRecord(stderrTail) {
		return ErrorClassOOM, stats.PeakBytes, true
	}
	return "", 0, false
}

// oomFailureMessage renders the required failure text: "out of memory in
// <command>, peak N GB".
func oomFailureMessage(command string, peakBytes int64) string {
	name := strings.TrimSpace(command)
	if name == "" {
		name = "the job"
	}
	return fmt.Sprintf("out of memory in %s, peak %s", name, formatGigabytes(peakBytes))
}

func formatGigabytes(bytes int64) string {
	if bytes < 0 {
		bytes = 0
	}
	return fmt.Sprintf("%.1f GB", float64(bytes)/(1<<30))
}
