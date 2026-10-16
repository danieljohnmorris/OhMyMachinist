//go:build linux

package managedworker

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOOMDeathReportsCounterIncreaseAcrossRestarts(t *testing.T) {
	dataDirectory := t.TempDir()
	eventsPath := filepath.Join(t.TempDir(), "memory.events")
	if err := os.WriteFile(eventsPath, []byte("low 0\nmax 0\noom 0\noom_kill 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// First observation records the baseline and never guesses a death.
	runID, _, died, err := oomDeathSince(eventsPath, "", dataDirectory)
	if err != nil || died || runID != "" {
		t.Fatalf("first observation = %q, %v, %v", runID, died, err)
	}
	// A later restart sees a higher counter and the run that was in flight.
	if err := os.WriteFile(filepath.Join(dataDirectory, "lane.lock"), []byte("run-42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventsPath, []byte("oom_kill 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runID, _, died, err = oomDeathSince(eventsPath, "", dataDirectory)
	if err != nil {
		t.Fatalf("second observation: %v", err)
	}
	if !died || runID != "run-42" {
		t.Fatalf("second observation = %q, %v", runID, died)
	}
	// The baseline advanced, so a restart with no new kill is clean.
	runID, _, died, err = oomDeathSince(eventsPath, "", dataDirectory)
	if err != nil || died || runID != "" {
		t.Fatalf("third observation = %q, %v, %v", runID, died, err)
	}
}

func TestOOMDeathSurfacesCounterReadFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.events")
	if _, _, died, err := oomDeathSince(missing, "", t.TempDir()); err == nil || died {
		t.Fatalf("missing counter = died %v, err %v", died, err)
	}
}

func TestOOMDeathSurfacesUnusableDataDirectory(t *testing.T) {
	eventsPath := filepath.Join(t.TempDir(), "memory.events")
	if err := os.WriteFile(eventsPath, []byte("oom_kill 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, died, err := oomDeathSince(eventsPath, "", blocker); err == nil || died {
		t.Fatalf("unusable data directory = died %v, err %v", died, err)
	}
}
