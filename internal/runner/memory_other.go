//go:build !linux

package runner

import "errors"

// createMemoryScope reports that per-job memory scopes are unavailable on this
// platform. A configured limit is a hard error so the worker can say why the
// cap is not being applied rather than silently ignoring it.
func createMemoryScope(limit MemoryLimit, name string) (memoryScope, error) {
	if limit.Empty() {
		return nil, nil
	}
	return nil, errors.New("per-job memory limits require Linux cgroup v2")
}
