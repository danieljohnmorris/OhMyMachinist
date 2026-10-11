package managedworker

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestReadCompleteLinesSkipsPartialTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("one\ntwo\npart"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	chunk, next, err := readCompleteLines(file, 0)
	if err != nil || chunk != "one\ntwo\n" || next != 8 {
		t.Fatalf("first read = %q %d %v", chunk, next, err)
	}
	chunk, again, err := readCompleteLines(file, next)
	if err != nil || chunk != "" || again != next {
		t.Fatalf("partial read = %q %d %v", chunk, again, err)
	}
}

func TestStopLiveEventsOnlyForPermanentFailures(t *testing.T) {
	for status, want := range map[int]bool{http.StatusConflict: true, http.StatusNotFound: true, http.StatusMethodNotAllowed: true, http.StatusRequestEntityTooLarge: true, http.StatusBadGateway: false} {
		if got := stopLiveEvents(&ResponseError{Status: status}); got != want {
			t.Fatalf("status %d stop = %v", status, got)
		}
	}
	if stopLiveEvents(errors.New("network down")) {
		t.Fatal("network error stopped streaming")
	}
}
