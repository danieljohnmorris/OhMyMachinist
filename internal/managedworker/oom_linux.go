//go:build linux

package managedworker

import (
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
func workerOOMDeath(dataDirectory string) (runID string, peakBytes int64, died bool) {
	dataDirectory = strings.TrimSpace(dataDirectory)
	if dataDirectory == "" {
		return "", 0, false
	}
	eventsPath := workerCgroupFile("memory.events")
	if eventsPath == "" {
		return "", 0, false
	}
	count, err := cgroupCounter(eventsPath, "oom_kill")
	if err != nil {
		return "", 0, false
	}
	statePath := filepath.Join(dataDirectory, "oom-death.state")
	if err := os.MkdirAll(dataDirectory, 0o700); err != nil {
		return "", 0, false
	}
	previous, err := readCounter(statePath)
	if err != nil {
		// First observation: remember the baseline, never guess a death.
		_ = os.WriteFile(statePath, []byte(strconv.FormatInt(count, 10)+"\n"), 0o600)
		return "", 0, false
	}
	if err := os.WriteFile(statePath, []byte(strconv.FormatInt(count, 10)+"\n"), 0o600); err != nil {
		return "", 0, false
	}
	if count <= previous {
		return "", 0, false
	}
	peakBytes, _ = readCounter(workerCgroupFile("memory.peak"))
	body, err := os.ReadFile(filepath.Join(dataDirectory, "lane.lock"))
	if err != nil {
		return "", peakBytes, true
	}
	return strings.TrimSpace(string(body)), peakBytes, true
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
