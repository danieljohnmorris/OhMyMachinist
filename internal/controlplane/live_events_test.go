package controlplane

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/owainlewis/machinist/internal/protocol"
)

func TestLiveEventsAppendWhileRunningAndCompletionReplaces(t *testing.T) {
	server, webServer := newTestHTTPServer(t)
	defer webServer.Close()
	auth := map[string]string{"Authorization": "Bearer secret"}
	if _, err := server.store.CreateJob(t.Context(), "request", "machinist", "plan", testAgent("plan", "Plan request")); err != nil {
		t.Fatal(err)
	}
	pollResponse := postJSON(t, webServer.URL+"/api/v1/workers/poll", pollRequest("worker-a", []string{"codex"}, []string{"machinist"}), auth)
	var polled protocol.PollResponse
	if err := json.NewDecoder(pollResponse.Body).Decode(&polled); err != nil {
		t.Fatal(err)
	}
	pollResponse.Body.Close()
	if polled.Run == nil {
		t.Fatal("poll returned no run")
	}
	endpoint := webServer.URL + "/api/v1/runs/" + polled.Run.ID + "/events"
	push := func(events protocol.LiveEvents, headers map[string]string) int {
		response := postJSON(t, endpoint, events, headers)
		response.Body.Close()
		return response.StatusCode
	}
	lease := polled.Run.LeaseToken
	if status := push(protocol.LiveEvents{InstanceID: "worker-a", LeaseToken: lease, Events: "a\n"}, nil); status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated push status = %d", status)
	}
	if status := push(protocol.LiveEvents{InstanceID: "worker-b", LeaseToken: lease, Events: "x\n"}, auth); status != http.StatusConflict {
		t.Fatalf("other worker push status = %d", status)
	}
	for _, chunk := range []string{"line 1\n", "line 2\n"} {
		if status := push(protocol.LiveEvents{InstanceID: "worker-a", LeaseToken: lease, Events: chunk}, auth); status != http.StatusNoContent {
			t.Fatalf("push status = %d", status)
		}
	}
	if got := readRunEvents(t, server, webServer.URL, polled.Run.ID); got.State != "running" || got.Events != "line 1\nline 2\n" {
		t.Fatalf("live events = %#v", got)
	}

	completion := protocol.Completion{InstanceID: "worker-a", LeaseToken: lease, State: "succeeded", Events: "full log\n", Result: json.RawMessage(`{}`)}
	completed := postJSON(t, webServer.URL+"/api/v1/runs/"+polled.Run.ID+"/complete", completion, auth)
	if completed.StatusCode != http.StatusNoContent {
		t.Fatalf("completion status = %d", completed.StatusCode)
	}
	completed.Body.Close()
	if got := readRunEvents(t, server, webServer.URL, polled.Run.ID); got.State != "succeeded" || got.Events != "full log\n" {
		t.Fatalf("completed events = %#v", got)
	}
	if status := push(protocol.LiveEvents{InstanceID: "worker-a", LeaseToken: lease, Events: "late\n"}, auth); status != http.StatusConflict {
		t.Fatalf("push after completion status = %d", status)
	}
}

func TestRunEventsRequiresSameOriginOrWorkerToken(t *testing.T) {
	server, webServer := newTestHTTPServer(t)
	defer webServer.Close()
	request, err := http.NewRequest(http.MethodGet, webServer.URL+"/api/v1/runs/missing/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("anonymous status = %d", response.StatusCode)
	}
	request.Header.Set("X-Machinist-CSRF", server.csrfToken)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing run status = %d", response.StatusCode)
	}
}

func readRunEvents(t *testing.T, server *Server, base, runID string) RunEvents {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, base+"/api/v1/runs/"+runID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Machinist-CSRF", server.csrfToken)
	request.Header.Set("Sec-Fetch-Site", "same-origin")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d", response.StatusCode)
	}
	var output RunEvents
	if err := json.NewDecoder(response.Body).Decode(&output); err != nil {
		t.Fatal(err)
	}
	return output
}
