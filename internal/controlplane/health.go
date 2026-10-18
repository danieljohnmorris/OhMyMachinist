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

// Health checks (MACH-25): Machinist probes configured endpoints (pipsqueek,
// the Sparks host, the GLM API) on a loop and spools an alert when one is
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
// previous state for that alert source, and POSTs the new state to the
// optional webhook.
func (w *failAlertWriter) writeNamed(name string, payload any) error {
	if w == nil || w.directory == "" {
		return nil
	}
	if err := ensureAlertDirectory(w.directory, w.spoolMode()); err != nil {
		return err
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("encode alert: %w", err)
	}
	body = append(body, '\n')
	path := filepath.Join(w.directory, name)
	if err := os.WriteFile(path, body, w.spoolMode()); err != nil {
		return fmt.Errorf("write alert %s: %w", path, err)
	}
	// The create mode is narrowed by the umask; re-apply it explicitly so the
	// group watcher keeps read access (MACH-72).
	if err := os.Chmod(path, w.spoolMode()); err != nil {
		return fmt.Errorf("mode alert %s: %w", path, err)
	}
	go w.deliver(body)
	return nil
}

// remove deletes a spooled alert file; a missing file is already clear.
func (w *failAlertWriter) remove(name string) error {
	if w == nil || w.directory == "" {
		return nil
	}
	if err := ensureAlertDirectory(w.directory, w.spoolMode()); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(w.directory, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
