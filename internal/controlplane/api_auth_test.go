package controlplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/owainlewis/machinist/internal/config"
)

// newAuthTestServer starts a control plane with the given auth mode.
func newAuthTestServer(t *testing.T, authMode string) (*Server, *httptest.Server) {
	t.Helper()
	directory := t.TempDir()
	promptPath := filepath.Join(directory, "plan.md")
	if err := os.WriteFile(promptPath, []byte("Plan this request:\n{{machinist.prompt}}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	definitionPath := filepath.Join(directory, "config.toml")
	if err := os.WriteFile(definitionPath, []byte("[commands.plan]\nexecutor = \"test\"\nprompt_file = \"plan.md\"\ntimeout = \"1m\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := openTestStore(t, filepath.Join(directory, "machinist.db"))
	server, err := NewServer(store, definitionPath, "secret", 0, authMode)
	if err != nil {
		t.Fatal(err)
	}
	return server, httptest.NewServer(server.Handler())
}

func authRequest(t *testing.T, method, endpoint, token string, body any) *http.Response {
	t.Helper()
	var payload *bytes.Reader
	if body == nil {
		payload = bytes.NewReader(nil)
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, endpoint, payload)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decode[T any](t *testing.T, response *http.Response) T {
	t.Helper()
	defer response.Body.Close()
	var value T
	if err := json.NewDecoder(response.Body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

// seedRepository registers the test repository so submissions resolve it.
func seedRepository(t *testing.T, server *Server) {
	t.Helper()
	if _, err := server.store.db.Exec(`INSERT INTO known_repositories(repository) VALUES('machinist')`); err != nil {
		t.Fatal(err)
	}
}

// TestAPIAuthOnRejectsMissingToken covers auth enabled by default: machine
// reads without a token are unauthorized, and valid tokens pass.
func TestAPIAuthOnRejectsMissingToken(t *testing.T) {
	server, webServer := newAuthTestServer(t, config.AuthTokens)
	defer webServer.Close()
	seedRepository(t, server)
	_, readSecret, err := server.store.CreateToken(t.Context(), "reader", TokenScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	_, writeSecret, err := server.store.CreateToken(t.Context(), "writer", TokenScopeWrite)
	if err != nil {
		t.Fatal(err)
	}

	response := authRequest(t, http.MethodGet, webServer.URL+"/api/v1/jobs", "", nil)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("jobs without token status = %d, want 401", response.StatusCode)
	}
	response.Body.Close()

	response = authRequest(t, http.MethodGet, webServer.URL+"/api/v1/jobs", "mmt_bogus", nil)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("jobs with bogus token status = %d, want 401", response.StatusCode)
	}
	response.Body.Close()

	jobs := decode[struct {
		Jobs []Job `json:"jobs"`
	}](t, authRequest(t, http.MethodGet, webServer.URL+"/api/v1/jobs", readSecret, nil))
	if jobs.Jobs == nil {
		t.Fatal("jobs list should never be null")
	}

	created := decode[struct {
		ID string `json:"id"`
	}](t, authRequest(t, http.MethodPost, webServer.URL+"/api/v1/jobs", writeSecret, map[string]any{
		"repository": "machinist", "command": "plan", "prompt": "Work locally",
	}))
	if created.ID == "" {
		t.Fatalf("job submission rejected: %s", created.ID)
	}
}

// TestAPIScopeDenial covers read tokens being rejected on write endpoints.
func TestAPIScopeDenial(t *testing.T) {
	server, webServer := newAuthTestServer(t, config.AuthTokens)
	defer webServer.Close()
	_, readSecret, err := server.store.CreateToken(t.Context(), "reader", TokenScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	_, writeSecret, err := server.store.CreateToken(t.Context(), "writer", TokenScopeWrite)
	if err != nil {
		t.Fatal(err)
	}

	// Read scope cannot queue jobs.
	response := authRequest(t, http.MethodPost, webServer.URL+"/api/v1/jobs", readSecret, map[string]any{
		"repository": "machinist", "command": "plan", "prompt": "Work locally",
	})
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("read-scope submission status = %d, want 403", response.StatusCode)
	}
	response.Body.Close()

	// Read scope cannot write settings or manage tokens.
	response = authRequest(t, http.MethodPut, webServer.URL+"/api/v1/settings/review_default_profile", readSecret, map[string]string{"value": "x"})
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("read-scope setting status = %d, want 403", response.StatusCode)
	}
	response.Body.Close()
	response = authRequest(t, http.MethodPost, webServer.URL+"/api/v1/tokens", readSecret, map[string]string{"name": "sneaky", "scope": "write"})
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("read-scope token create status = %d, want 403", response.StatusCode)
	}
	response.Body.Close()

	// Write scope can write settings, and the change is audit-logged.
	if response := authRequest(t, http.MethodPut, webServer.URL+"/api/v1/settings/review_default_profile", writeSecret, map[string]string{"value": "x"}); response.StatusCode != http.StatusOK {
		t.Fatalf("write-scope setting status = %d, want 200", response.StatusCode)
	} else {
		response.Body.Close()
	}
	entries, err := server.store.AuditLog(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 || entries[0]["action"] != "settings.write" || entries[0]["token_name"] != "writer" {
		t.Fatalf("audit entries = %#v", entries)
	}
}

// TestAPIAuthOffCoversTheWarningContract: opt-out auth serves reads and
// writes without tokens (the startup warning itself is covered by the
// warnOnAuthDisabled call in Serve).
func TestAPIAuthOff(t *testing.T) {
	server, webServer := newAuthTestServer(t, config.AuthNone)
	defer webServer.Close()
	seedRepository(t, server)

	jobs := decode[struct {
		Jobs []Job `json:"jobs"`
	}](t, authRequest(t, http.MethodGet, webServer.URL+"/api/v1/jobs", "", nil))
	if jobs.Jobs == nil {
		t.Fatal("jobs list should never be null")
	}

	response := authRequest(t, http.MethodPost, webServer.URL+"/api/v1/jobs", "", map[string]any{
		"repository": "machinist", "command": "plan", "prompt": "Work locally",
	})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("auth-off submission status = %d, want 201", response.StatusCode)
	}
	response.Body.Close()

	// Changes made while auth is off are still audit-logged as anonymous.
	entries, err := server.store.AuditLog(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 || entries[0]["token_name"] != "anonymous" {
		t.Fatalf("auth-off audit entries = %#v", entries)
	}
}

// TestAPIRejectsPublicBind pins the trusted-interface rule for auth opt-out.
func TestAPIRejectsPublicBind(t *testing.T) {
	if err := config.ValidateListenAuth(config.AuthNone, "127.0.0.1:7331"); err != nil {
		t.Fatalf("loopback auth-off rejected: %v", err)
	}
	if err := config.ValidateListenAuth(config.AuthNone, "100.100.1.1:7331"); err != nil {
		t.Fatalf("tailscale auth-off rejected: %v", err)
	}
	if err := config.ValidateListenAuth(config.AuthNone, "192.168.1.10:7331"); err == nil {
		t.Fatal("auth-off on private LAN address must be rejected")
	}
	if err := config.ValidateListenAuth(config.AuthTokens, "127.0.0.1:7331"); err != nil {
		t.Fatalf("loopback token auth rejected: %v", err)
	}
	if err := config.ValidateListenAuth(config.AuthTokens, "192.168.1.10:7331"); err == nil {
		t.Fatal("public bind must be rejected regardless of auth mode")
	}
}

// TestMCPAuthAndScope pins the MCP transport contract: initialize, tools/list,
// tools/call with scopes, and denial for read tokens on write tools.
func TestMCPAuthAndScope(t *testing.T) {
	server, webServer := newAuthTestServer(t, config.AuthTokens)
	defer webServer.Close()
	_, readSecret, err := server.store.CreateToken(t.Context(), "reader", TokenScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	_, writeSecret, err := server.store.CreateToken(t.Context(), "writer", TokenScopeWrite)
	if err != nil {
		t.Fatal(err)
	}

	callMCP := func(token, method string, params any, id int) (int, map[string]any) {
		body := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
		if params != nil {
			body["params"] = params
		}
		response := authRequest(t, http.MethodPost, webServer.URL+"/mcp", token, body)
		defer response.Body.Close()
		var payload map[string]any
		if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, payload
	}

	// No token → 401 before any JSON-RPC handling.
	if status, _ := callMCP("", "initialize", map[string]any{}, 1); status != http.StatusUnauthorized {
		t.Fatalf("MCP without token status = %d, want 401", status)
	}

	status, payload := callMCP(readSecret, "initialize", map[string]any{}, 1)
	if status != http.StatusOK || payload["result"] == nil {
		t.Fatalf("initialize = %d %v", status, payload)
	}
	status, payload = callMCP(readSecret, "tools/list", map[string]any{}, 2)
	result := payload["result"].(map[string]any)
	tools := result["tools"].([]any)
	if status != http.StatusOK || len(tools) == 0 {
		t.Fatalf("tools/list = %d with %d tools", status, len(tools))
	}

	// Read token may call read tools.
	status, payload = callMCP(readSecret, "tools/call", map[string]any{"name": "get_board", "arguments": map[string]any{}}, 3)
	if status != http.StatusOK || payload["result"] == nil {
		t.Fatalf("get_board = %d %v", status, payload)
	}

	// Read token is denied write tools.
	status, payload = callMCP(readSecret, "tools/call", map[string]any{"name": "set_setting", "arguments": map[string]any{"key": "k", "value": "v"}}, 4)
	if status != http.StatusOK {
		t.Fatalf("denied set_setting status = %d, want 200 JSON-RPC", status)
	}
	if !strings.Contains(payload["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), "write-scope") {
		t.Fatalf("denied set_setting result = %v", payload)
	}

	// Write token can queue a job through the same core as the HTTP API.
	seedRepository(t, server)
	status, payload = callMCP(writeSecret, "tools/call", map[string]any{"name": "queue_job", "arguments": map[string]any{
		"repository": "machinist", "command": "plan", "prompt": "Work locally",
	}}, 5)
	if status != http.StatusOK {
		t.Fatalf("queue_job status = %d %v", status, payload)
	}
	if _, err := json.Marshal(payload); err != nil {
		t.Fatal(err)
	}
	text := payload["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "job_") {
		t.Fatalf("queue_job result text = %s", text)
	}

	// Unknown methods are JSON-RPC errors.
	if _, payload = callMCP(readSecret, "bogus/method", map[string]any{}, 6); payload["error"] == nil {
		t.Fatal("unknown method should return a JSON-RPC error")
	}

	// GET is not part of the served transport.
	response := authRequest(t, http.MethodGet, webServer.URL+"/mcp", readSecret, nil)
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp status = %d, want 405", response.StatusCode)
	}
	response.Body.Close()
}

// TestTokenHashesOnly pins that plaintext secrets are never persisted.
func TestTokenHashesOnly(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	_, secret, err := store.CreateToken(t.Context(), "bot", TokenScopeWrite)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := store.db.QueryRow(`SELECT token_hash FROM api_tokens`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, strings.TrimPrefix(secret, "mmt_")) || stored == secret {
		t.Fatalf("plaintext secret found in storage: %s", stored)
	}
	if _, scope, ok := store.AuthenticateToken(t.Context(), secret); !ok || scope != TokenScopeWrite {
		t.Fatal("valid token failed to authenticate")
	}
	if _, _, ok := store.AuthenticateToken(t.Context(), secret+"x"); ok {
		t.Fatal("tampered token authenticated")
	}
	if err := store.RevokeToken(t.Context(), "missing"); err == nil {
		t.Fatal("revoking an unknown token should fail")
	}
}
