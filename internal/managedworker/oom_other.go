//go:build !linux

package managedworker

// workerOOMDeath reports no detectable OOM death off Linux, where there is no
// cgroup v2 counter to read (MACH-62).
func workerOOMDeath(dataDirectory string) (runID string, peakBytes int64, died bool) {
	return "", 0, false
}
