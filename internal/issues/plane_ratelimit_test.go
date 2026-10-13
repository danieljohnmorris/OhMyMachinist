package issues

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlaneRequestRetriesRateLimit(t *testing.T) {
	previous := planeRetryAfter
	planeRetryAfter = func(*http.Response) time.Duration { return time.Millisecond }
	t.Cleanup(func() { planeRetryAfter = previous })
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if calls.Add(1) < 3 {
			response.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = response.Write([]byte(`{"id":"i","project":"p","state":"s"}`))
	}))
	defer server.Close()
	client, err := NewPlaneWorkspaceClient(PlaneConfig{URL: server.URL, Workspace: "w", APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	ref, ok, err := client.resolveKey(t.Context(), "MACH-1")
	if err != nil || !ok || ref.ID != "i" {
		t.Fatalf("resolveKey = %+v %v %v", ref, ok, err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
}

func TestPlaneRetryAfterUsesResetHeader(t *testing.T) {
	response := &http.Response{Header: http.Header{}}
	response.Header.Set("X-RateLimit-Reset", "0")
	if wait := planeRetryAfter(response); wait != time.Second {
		t.Fatalf("past reset wait = %v", wait)
	}
	response.Header.Set("X-RateLimit-Reset", "99999999999")
	if wait := planeRetryAfter(response); wait != 65*time.Second {
		t.Fatalf("far reset wait = %v", wait)
	}
}
