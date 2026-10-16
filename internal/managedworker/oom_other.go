//go:build !linux

package managedworker

// workerOOMDeath reports no detectable OOM death off Linux, where there is no
// cgroup v2 counter to read (MACH-62). There is no I/O to fail, so it never
// returns an error.
func workerOOMDeath(dataDirectory string) (runID string, peakBytes int64, died bool, err error) {
	return "", 0, false, nil
}
