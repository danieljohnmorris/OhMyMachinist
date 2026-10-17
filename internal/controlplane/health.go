package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/owainlewis/machinist/internal/config"
)

// Health checks (MACH-25): Machinist probes configured endpoints (the Sparks
// host, the GLM API) on a loop and spools an alert when one is
// unreachable, replacing the external Factory Alerts watcher as the thing
// that notices dead infrastructure.

// healthAlert is the JSON spooled for an unreachable endpoint.
type healthAlert struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Error     string `json:"error"`
	CheckedAt string `json:"checked_at"`
}

// healthChecker probes configured endpoints and spools alerts.
type healthChecker struct {
	alerts  *failAlertWriter
	client  *http.Client
	timeout time.Duration
	now     func() time.Time
}

func healthCheckInterval(health config.Health) time.Duration {
	interval, err := health.Interval()
	if err != nil || interval <= 0 {
		return time.Minute
	}
	return interval
}

// runHealthChecks probes every configured endpoint; failures overwrite that
// endpoint's alert file so the latest state is always spooled. Recovered
// endpoints clear their alert.
func (s *Server) runHealthChecks(ctx context.Context) error {
	if s.healthChecker == nil || len(s.health.Endpoints) == 0 {
		return nil
	}
	return s.healthChecker.checkAll(ctx, s.health)
}

func (c *healthChecker) checkAll(ctx context.Context, health config.Health) error {
	timeout := c.timeout
	if parsed, err := health.RequestTimeout(); err == nil {
		timeout = parsed
	}
	for name, url := range health.Endpoints {
		if err := c.checkOne(ctx, name, url, timeout); err != nil {
			log.Printf("health check %s (%s): %v", name, url, err)
			continue
		}
	}
	return nil
}

func (c *healthChecker) checkOne(ctx context.Context, name, url string, timeout time.Duration) error {
	unreachable := probeEndpoint(ctx, url, timeout)
	if unreachable == "" {
		return c.alerts.remove("health-" + name + ".json")
	}
	alert := healthAlert{Name: name, URL: url, Error: unreachable, CheckedAt: c.now().UTC().Format(time.RFC3339)}
	if err := c.alerts.writeNamed("health-"+name+".json", alert); err != nil {
		return fmt.Errorf("spool health alert for %s: %w", name, err)
	}
	return nil
}

// probeEndpoint returns an empty string when the endpoint answers (any HTTP
// status counts: the host being up is the question, not the route) and the
// error text when it does not.
func probeEndpoint(ctx context.Context, url string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err.Error()
	}
	client := &http.Client{Timeout: timeout}
	response, err := client.Do(request)
	if err != nil {
		return err.Error()
	}
	response.Body.Close()
	return ""
}

// writeNamed spools one JSON alert under a fixed name, overwriting the
// previous state for that alert source.
func (w *failAlertWriter) writeNamed(name string, payload any) error {
	if w == nil || w.directory == "" {
		return nil
	}
	if err := os.MkdirAll(w.directory, 0o755); err != nil {
		return fmt.Errorf("create alert directory: %w", err)
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encode alert: %w", err)
	}
	path := filepath.Join(w.directory, name)
	if err := os.WriteFile(path, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("write alert %s: %w", path, err)
	}
	return nil
}

// remove deletes a spooled alert file; a missing file is already clear.
func (w *failAlertWriter) remove(name string) error {
	if w == nil || w.directory == "" {
		return nil
	}
	if err := os.MkdirAll(w.directory, 0o755); err != nil {
		return fmt.Errorf("create alert directory: %w", err)
	}
	if err := os.Remove(filepath.Join(w.directory, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
