//go:build linux

package managedworker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// workerOOMDeath reports whether this worker's own cgroup was OOM-killed since
// the previous start and, if so, which run it was executing. The worker's
// own cgroup only hits its limit when the worker itself ran out of memory
// (per-job scopes contain each job), which is when a run must never be
// re-leased (MACH-62).
//
// An error means the check itself failed, which must never be read as a clean
// restart: the run that OOM-killed the worker would then be re-leased when its
// lease expires, the exact outcome MACH-62 forbids. The caller reports the
// error rather than swallowing it.
func workerOOMDeath(dataDirectory string) (runID string, peakBytes int64, died bool, err error) {
	dataDirectory = strings.TrimSpace(dataDirectory)
	if dataDirectory == "" {
		return "", 0, false, nil
	}
	eventsPath := workerCgroupFile("memory.events")
	if eventsPath == "" {
		return "", 0, false, nil
	}
	return oomDeathSince(eventsPath, workerCgroupFile("memory.peak"), dataDirectory)
}

// oomDeathSince compares the cgroup's OOM-kill counter at eventsPath against
// the baseline recorded under dataDirectory, so an OOM kill between two
// restarts is detectable. The paths are parameters so the logic can be tested
// without a real cgroup. Any I/O failure is returned, never folded into a
// "no OOM death" result.
func oomDeathSince(eventsPath, peakPath, dataDirectory string) (runID string, peakBytes int64, died bool, err error) {
	count, err := cgroupCounter(eventsPath, "oom_kill")
	if err != nil {
		return "", 0, false, fmt.Errorf("read %s: %w", eventsPath, err)
	}
	if err := os.MkdirAll(dataDirectory, 0o700); err != nil {
		return "", 0, false, fmt.Errorf("create data directory: %w", err)
	}
	statePath := filepath.Join(dataDirectory, "oom-death.state")
	previous, err := readCounter(statePath)
	if errors.Is(err, fs.ErrNotExist) {
		// First observation: remember the baseline, never guess a death.
		if writeErr := os.WriteFile(statePath, []byte(strconv.FormatInt(count, 10)+"\n"), 0o600); writeErr != nil {
			return "", 0, false, fmt.Errorf("write OOM baseline: %w", writeErr)
		}
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("read OOM state: %w", err)
	}
	if err := os.WriteFile(statePath, []byte(strconv.FormatInt(count, 10)+"\n"), 0o600); err != nil {
		return "", 0, false, fmt.Errorf("write OOM state: %w", err)
	}
	if count <= previous {
		return "", 0, false, nil
	}
	// The death is real from here on: the peak is informational, so a missing
	// or unreadable peak file must not turn the death into an error.
	if peakPath != "" {
		peakBytes, _ = readCounter(peakPath)
	}
	body, err := os.ReadFile(filepath.Join(dataDirectory, "lane.lock"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Died, but no run was recorded as in flight.
			return "", peakBytes, true, nil
		}
		return "", peakBytes, true, fmt.Errorf("read lane.lock: %w", err)
	}
	return strings.TrimSpace(string(body)), peakBytes, true, nil
}

// workerCgroupFile returns a file inside the worker process's own cgroup, or
// "" when it cannot be resolved.
func workerCgroupFile(name string) string {
	body, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(fields) != 3 || fields[0] != "0" {
			continue
		}
		relative := strings.TrimPrefix(fields[2], "/")
		if relative == "" {
			return ""
		}
		return filepath.Join("/sys/fs/cgroup", relative, name)
	}
	return ""
}

// cgroupCounter reads one counter line (for example "oom_kill 3").
func cgroupCounter(path, key string) (int64, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == key {
			return strconv.ParseInt(fields[1], 10, 64)
		}
	}
	return 0, nil
}

func readCounter(path string) (int64, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
}
