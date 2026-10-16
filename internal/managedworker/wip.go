package managedworker

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/config"
)

// saveLaneWIP saves the lane checkout's uncommitted work, tracked and
// untracked (MACH-65): a git stash push --include-untracked plus the stash
// patch under the worker data directory, outside the published run artifacts,
// as in MACH-59. It returns a record of where the work was saved, empty when
// the checkout was already clean. The lane is left clean, so the next attempt
// starts from a clean checkout.
func saveLaneWIP(workerConfig config.Worker, repository string) (string, error) {
	lane, err := workerConfig.ResolveRepository(repository)
	if err != nil {
		return "", err
	}
	status := exec.Command("git", "-C", lane, "status", "--porcelain")
	output, statusErr := status.Output()
	if statusErr != nil || len(strings.TrimSpace(string(output))) == 0 {
		return "", nil
	}
	label := "machinist-lane-wip-" + time.Now().UTC().Format("20060102-150405")
	stash := exec.Command("git", "-C", lane, "stash", "push", "--include-untracked", "-m", label)
	if stashOutput, stashErr := stash.CombinedOutput(); stashErr != nil {
		return "", fmt.Errorf("git stash push: %s: %w", strings.TrimSpace(string(stashOutput)), stashErr)
	}
	saved := fmt.Sprintf("stash %q in lane checkout %s", label, lane)
	if strings.TrimSpace(workerConfig.DataDirectory) == "" {
		return saved, nil
	}
	show := exec.Command("git", "-C", lane, "stash", "show", "--patch", "--include-untracked", "stash@{0}")
	if patch, showErr := show.Output(); showErr == nil && len(patch) > 0 {
		directory := filepath.Join(workerConfig.DataDirectory, "wip")
		if mkdirErr := os.MkdirAll(directory, 0o700); mkdirErr == nil {
			path := filepath.Join(directory, label+".patch")
			if writeErr := os.WriteFile(path, patch, 0o600); writeErr == nil {
				saved += "; patch " + path
			}
		}
	}
	return saved, nil
}
