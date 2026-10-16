//go:build linux

package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cgroupRoot is the cgroup v2 mount point. Hosts that mount it elsewhere can
// point MACHINIST_MEMORY_CGROUP_ROOT at the real path.
const cgroupRoot = "/sys/fs/cgroup"

// memoryScope is one job's memory-limited cgroup (MACH-62). Creating it puts
// the job (and everything it starts) under the limits, so a runaway job is
// killed instead of the worker.
type cgroupScope struct {
	path      string
	peakLimit int64

	mu       sync.Mutex
	sampled  int64
	stop     chan struct{}
	stopOnce sync.Once
	waited   sync.WaitGroup
}

// createMemoryScope creates a child cgroup under the worker's own cgroup with
// the configured limits. It returns (nil, nil) when no limit is configured;
// an error when a limit is configured but the scope cannot be created, so the
// caller can record it (the worker-level systemd backstop still applies).
func createMemoryScope(limit MemoryLimit, name string) (memoryScope, error) {
	if limit.Empty() {
		return nil, nil
	}
	parent, err := memoryCgroupParent()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(parent, "machinist-"+name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("create memory cgroup %q: %w", path, err)
	}
	scope := &cgroupScope{path: path, peakLimit: limit.Max}
	for _, field := range []struct {
		file  string
		value int64
	}{
		{"memory.high", limit.High},
		{"memory.max", limit.Max},
		{"memory.swap.max", limit.SwapMax},
	} {
		if field.value <= 0 {
			continue
		}
		// memory.swap.max is absent when swap accounting is off; that is not
		// fatal, the other limits still bound the job.
		if err := os.WriteFile(filepath.Join(path, field.file), []byte(strconv.FormatInt(field.value, 10)), 0o644); err != nil && field.file != "memory.swap.max" {
			_ = scope.close()
			return nil, fmt.Errorf("set %s on %q: %w", field.file, path, err)
		}
	}
	return scope, nil
}

// attach moves the process (and its threads) into the job's cgroup.
func (scope *cgroupScope) attach(pid int) error {
	if scope == nil {
		return nil
	}
	if err := os.WriteFile(filepath.Join(scope.path, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Errorf("attach pid %d to memory cgroup: %w", pid, err)
	}
	scope.stop = make(chan struct{})
	scope.waited.Add(1)
	go scope.sample()
	return nil
}

// sample tracks the highest memory.current seen, so a peak is reported even on
// kernels without memory.peak.
func (scope *cgroupScope) sample() {
	defer scope.waited.Done()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if body, err := os.ReadFile(filepath.Join(scope.path, "memory.current")); err == nil {
			if value, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64); err == nil {
				scope.mu.Lock()
				if value > scope.sampled {
					scope.sampled = value
				}
				scope.mu.Unlock()
			}
		}
		select {
		case <-scope.stop:
			return
		case <-ticker.C:
		}
	}
}

// stats reports the scope's peak memory and OOM kill count.
func (scope *cgroupScope) stats() MemoryStats {
	if scope == nil {
		return MemoryStats{}
	}
	scope.stopSampling()
	var result MemoryStats
	// memory.peak is the authoritative peak on modern kernels.
	if body, err := os.ReadFile(filepath.Join(scope.path, "memory.peak")); err == nil {
		result.PeakBytes, _ = strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
	}
	scope.mu.Lock()
	if scope.sampled > result.PeakBytes {
		result.PeakBytes = scope.sampled
	}
	scope.mu.Unlock()
	if result.PeakBytes == 0 && scope.peakLimit > 0 {
		result.PeakBytes = scope.peakLimit
	}
	if body, err := os.ReadFile(filepath.Join(scope.path, "memory.events")); err == nil {
		for _, line := range strings.Split(string(body), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "oom_kill" {
				result.OOMKills, _ = strconv.ParseInt(fields[1], 10, 64)
			}
		}
	}
	return result
}

func (scope *cgroupScope) stopSampling() {
	scope.stopOnce.Do(func() {
		if scope.stop != nil {
			close(scope.stop)
			scope.waited.Wait()
		}
	})
}

// close kills any leftover processes in the scope and removes it.
func (scope *cgroupScope) close() error {
	if scope == nil {
		return nil
	}
	scope.stopSampling()
	_ = os.WriteFile(filepath.Join(scope.path, "cgroup.kill"), []byte("1"), 0o644)
	deadline := time.Now().Add(2 * time.Second)
	for {
		body, err := os.ReadFile(filepath.Join(scope.path, "cgroup.procs"))
		if err == nil && strings.TrimSpace(string(body)) == "" {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := os.Remove(scope.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove memory cgroup %q: %w", scope.path, err)
	}
	return nil
}

// memoryCgroupParent returns the cgroup the job scopes are created under: the
// configured override, else the worker process's own cgroup (which systemd
// delegates to the service via Delegate=yes).
func memoryCgroupParent() (string, error) {
	if override := strings.TrimSpace(os.Getenv("MACHINIST_MEMORY_CGROUP_PARENT")); override != "" {
		return override, nil
	}
	body, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("read /proc/self/cgroup: %w", err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(fields) != 3 || fields[0] != "0" {
			continue
		}
		relative := strings.TrimPrefix(fields[2], "/")
		if relative == "" {
			return "", errors.New("worker runs at the cgroup root; cannot create a job scope")
		}
		return filepath.Join(cgroupRoot, relative), nil
	}
	return "", errors.New("no cgroup v2 entry found for the worker")
}
