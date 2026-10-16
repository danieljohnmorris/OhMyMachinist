package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/config"
)

func newHealthChecker(t *testing.T) (*healthChecker, string) {
	t.Helper()
	directory := t.TempDir()
	return &healthChecker{alerts: &failAlertWriter{directory: directory}, timeout: 2 * time.Second, now: time.Now}, directory
}

func TestHealthCheckUnreachableEndpointAlerts(t *testing.T) {
	checker, directory := newHealthChecker(t)
	checker.checkOne(context.Background(), "glm", "http://127.0.0.1:1/unreachable", checker.timeout)

	body, err := os.ReadFile(filepath.Join(directory, "health-glm.json"))
	if err != nil {
		t.Fatalf("expected a spooled health alert: %v", err)
	}
	var alert healthAlert
	if err := json.Unmarshal(body, &alert); err != nil {
		t.Fatal(err)
	}
	if alert.Name != "glm" || alert.Error == "" {
		t.Fatalf("unexpected alert: %+v", alert)
	}
}

func TestHealthCheckReachableEndpointIsSilent(t *testing.T) {
	checker, directory := newHealthChecker(t)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	checker.checkOne(context.Background(), "sparks", server.URL, checker.timeout)
	if _, err := os.Stat(filepath.Join(directory, "health-sparks.json")); !os.IsNotExist(err) {
		t.Fatalf("reachable endpoint spooled an alert: %v", err)
	}

	// A spooled alert clears once the endpoint answers again.
	if err := checker.alerts.writeNamed("health-sparks.json", healthAlert{Name: "sparks"}); err != nil {
		t.Fatal(err)
	}
	checker.checkOne(context.Background(), "sparks", server.URL, checker.timeout)
	if _, err := os.Stat(filepath.Join(directory, "health-sparks.json")); !os.IsNotExist(err) {
		t.Fatalf("recovered endpoint did not clear its alert: %v", err)
	}
}

func TestRunHealthChecksWiresConfig(t *testing.T) {
	server, _, _ := newHealingTestServer(t)
	directory := t.TempDir()
	server.health = config.Health{Endpoints: map[string]string{"glm": "http://127.0.0.1:1/unreachable"}}
	server.healthChecker = &healthChecker{alerts: &failAlertWriter{directory: directory}, timeout: 2 * time.Second, now: time.Now}
	if err := server.runHealthChecks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "health-glm.json")); err != nil {
		t.Fatalf("expected spooled alert: %v", err)
	}
}
