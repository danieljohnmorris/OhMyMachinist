package controlplane

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/owainlewis/machinist/internal/config"
)

func TestProjectSettingsOverlayTrackerConfig(t *testing.T) {
	directory := t.TempDir()
	definitionPath := filepath.Join(directory, "config.toml")
	definition := "[commands.plan]\nexecutor = \"test\"\nprompt_file = \"plan.md\"\ntimeout = \"1m\"\n\n[tracker.projects.FILED]\nrepository = \"file-repo\"\ncommand = \"plan\"\n"
	if err := os.WriteFile(definitionPath, []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, filepath.Join(directory, "machinist.db"))
	server, err := NewServer(store, definitionPath, "secret", 0, config.AuthTokens)
	if err != nil {
		t.Fatal(err)
	}
	webServer := httptest.NewServer(server.Handler())
	defer webServer.Close()

	// A settings-defined project and an override of a file-defined project.
	for key, value := range map[string]string{
		"tracker.projects.SPARKS": "repository = \"sparks-repo\"\ncommand = \"plan\"\nmodel = \"m1\"\n",
		"tracker.projects.filed":  "repository = \"override-repo\"\n",
	} {
		if response := authRequest(t, http.MethodPut, webServer.URL+"/api/v1/settings/"+key, "secret", map[string]string{"value": value}); response.StatusCode != http.StatusOK {
			response.Body.Close()
			t.Fatalf("put %s status = %d", key, response.StatusCode)
		} else {
			response.Body.Close()
		}
	}

	response := authRequest(t, http.MethodGet, webServer.URL+"/api/v1/trackers", "secret", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("trackers status = %d", response.StatusCode)
	}
	var body struct {
		Projects map[string]struct {
			Name       string
			Repository string
			Command    string
			Model      string
		} `json:"projects"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Projects["SPARKS"].Name != "SPARKS" || body.Projects["SPARKS"].Repository != "sparks-repo" || body.Projects["SPARKS"].Command != "plan" || body.Projects["SPARKS"].Model != "m1" {
		t.Fatalf("projects = %#v, want SPARKS from the setting", body.Projects)
	}
	if body.Projects["FILED"].Repository != "override-repo" {
		t.Fatalf("FILED = %#v, want the setting to override the config file", body.Projects["FILED"])
	}

	created := authRequest(t, http.MethodPost, webServer.URL+"/api/v1/tickets", "secret", map[string]string{"title": "Ship it", "project": "sparks"})
	if created.StatusCode != http.StatusCreated {
		created.Body.Close()
		t.Fatalf("create status = %d, want 201 for a settings-defined project", created.StatusCode)
	}
	var ticket Ticket
	if err := json.NewDecoder(created.Body).Decode(&ticket); err != nil {
		t.Fatal(err)
	}
	created.Body.Close()
	if ticket.Key != "SPARKS-1" || ticket.Repository != "sparks-repo" {
		t.Fatalf("ticket = %#v, want a SPARKS-1 key and the project repository", ticket)
	}
}

func TestMalformedProjectSettingFailsLoudly(t *testing.T) {
	directory := t.TempDir()
	definitionPath := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(definitionPath, []byte("[commands.plan]\nexecutor = \"test\"\nprompt_file = \"plan.md\"\ntimeout = \"1m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, filepath.Join(directory, "machinist.db"))
	server, err := NewServer(store, definitionPath, "secret", 0, config.AuthTokens)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutSetting(t.Context(), "tracker.projects.SPARKS", "repository = [broken"); err != nil {
		t.Fatal(err)
	}
	webServer := httptest.NewServer(server.Handler())
	defer webServer.Close()

	response := authRequest(t, http.MethodGet, webServer.URL+"/api/v1/trackers", "secret", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusInternalServerError {
		t.Fatalf("trackers status = %d, want 500 for a malformed project setting", response.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Error, "tracker.projects.SPARKS") {
		t.Fatalf("error = %q, want it to name the malformed setting", body.Error)
	}
}
