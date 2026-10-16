package controlplane

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/skills"
)

const testSkillMarkdown = "---\nname: feature-dev:code-reviewer\ndescription: Reviews changes\n---\n\nReview the diff.\n"

// newSkillsTestServer starts a control plane whose skills library lives in a
// fresh data directory, returning read and write token secrets.
func newSkillsTestServer(t *testing.T) (*Server, *httptest.Server, string, string) {
	t.Helper()
	t.Setenv("MACHINIST_DATA_DIR", t.TempDir())
	server, webServer := newAuthTestServer(t, config.AuthTokens)
	_, readSecret, err := server.store.CreateToken(t.Context(), "reader", TokenScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	_, writeSecret, err := server.store.CreateToken(t.Context(), "writer", TokenScopeWrite)
	if err != nil {
		t.Fatal(err)
	}
	return server, webServer, readSecret, writeSecret
}

// writeSkillSource creates an on-disk skill directory to install.
func writeSkillSource(t *testing.T, body string) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return directory
}

func zipSkill(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, body := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// uploadSkill posts a multipart upload with one part; field selects the form
// field name so the missing-field case can be exercised.
func uploadSkill(t *testing.T, endpoint, token, field, filename string, content []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(field, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, endpoint, &body)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// auditActions returns the recorded audit actions, newest first.
func auditActions(t *testing.T, server *Server) []string {
	t.Helper()
	entries, err := server.store.AuditLog(t.Context(), 50)
	if err != nil {
		t.Fatal(err)
	}
	actions := make([]string, 0, len(entries))
	for _, entry := range entries {
		actions = append(actions, entry["action"].(string))
	}
	return actions
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestSkillsAPIInstallListGet covers the read and install endpoints, the
// read/write scope rule, the always-present used_by field, and the audit
// entry a write leaves behind.
func TestSkillsAPIInstallListGet(t *testing.T) {
	server, webServer, readSecret, writeSecret := newSkillsTestServer(t)
	defer webServer.Close()
	source := writeSkillSource(t, testSkillMarkdown)

	// Reads require a token.
	response := authRequest(t, http.MethodGet, webServer.URL+"/api/v1/skills", "", nil)
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("skills without token status = %d, want 401", response.StatusCode)
	}
	response.Body.Close()

	// An empty library lists as an empty array, not null.
	listed := decode[struct {
		Skills []map[string]any `json:"skills"`
	}](t, authRequest(t, http.MethodGet, webServer.URL+"/api/v1/skills", readSecret, nil))
	if listed.Skills == nil || len(listed.Skills) != 0 {
		t.Fatalf("empty skills list = %#v", listed.Skills)
	}

	// A read token cannot install.
	response = authRequest(t, http.MethodPost, webServer.URL+"/api/v1/skills", readSecret, map[string]any{"source": source})
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("read-scope install status = %d, want 403", response.StatusCode)
	}
	response.Body.Close()

	installed := decode[skills.InstallResult](t, authRequest(t, http.MethodPost, webServer.URL+"/api/v1/skills", writeSecret, map[string]any{"source": source}))
	if installed.Name != "feature-dev:code-reviewer" || installed.Version == "" || !installed.Added {
		t.Fatalf("install result = %#v", installed)
	}
	if !containsString(auditActions(t, server), "skills.install") {
		t.Fatal("install was not audit-logged")
	}

	// The list carries the skill and an empty (never null) used_by list.
	listed = decode[struct {
		Skills []map[string]any `json:"skills"`
	}](t, authRequest(t, http.MethodGet, webServer.URL+"/api/v1/skills", readSecret, nil))
	if len(listed.Skills) != 1 || listed.Skills[0]["name"] != "feature-dev:code-reviewer" {
		t.Fatalf("skills list = %#v", listed.Skills)
	}
	if usedBy, ok := listed.Skills[0]["used_by"].([]any); !ok || len(usedBy) != 0 {
		t.Fatalf("used_by = %#v, want an empty list", listed.Skills[0]["used_by"])
	}
	if _, ok := listed.Skills[0]["versions"].([]any); !ok {
		t.Fatalf("versions missing from list entry: %#v", listed.Skills[0])
	}

	// One skill read returns the recorded source and version.
	view := decode[map[string]any](t, authRequest(t, http.MethodGet, webServer.URL+"/api/v1/skills/feature-dev:code-reviewer", readSecret, nil))
	if view["active"] != installed.Version || view["source"] != source {
		t.Fatalf("skill view = %#v", view)
	}

	// An unknown skill is 404.
	response = authRequest(t, http.MethodGet, webServer.URL+"/api/v1/skills/missing", readSecret, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("missing skill status = %d, want 404", response.StatusCode)
	}
	response.Body.Close()

	// Responses never carry a token.
	if strings.Contains(mustJSON(t, listed), writeSecret) || strings.Contains(mustJSON(t, view), writeSecret) {
		t.Fatal("skills response leaked a token")
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// TestSkillsAPIUpdateAndPin covers the update and pin endpoints, including the
// optional body, activation, and the error statuses.
func TestSkillsAPIUpdateAndPin(t *testing.T) {
	_, webServer, readSecret, writeSecret := newSkillsTestServer(t)
	defer webServer.Close()
	source := writeSkillSource(t, testSkillMarkdown)

	installed := decode[skills.InstallResult](t, authRequest(t, http.MethodPost, webServer.URL+"/api/v1/skills", writeSecret, map[string]any{"source": source}))

	// Update with no body re-resolves the same source and adds nothing.
	unchanged := decode[skills.InstallResult](t, authRequest(t, http.MethodPost, webServer.URL+"/api/v1/skills/feature-dev:code-reviewer/update", writeSecret, nil))
	if unchanged.Added || unchanged.Version != installed.Version {
		t.Fatalf("unchanged update = %#v", unchanged)
	}

	// A changed source adds a version; activate switches to it.
	updatedBody := strings.Replace(testSkillMarkdown, "Review the diff.", "Review the diff, carefully.", 1)
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), []byte(updatedBody), 0o644); err != nil {
		t.Fatal(err)
	}
	updated := decode[skills.InstallResult](t, authRequest(t, http.MethodPost, webServer.URL+"/api/v1/skills/feature-dev:code-reviewer/update", writeSecret, map[string]any{"activate": true}))
	if !updated.Added || updated.Version == installed.Version || updated.Active != updated.Version {
		t.Fatalf("update = %#v", updated)
	}

	// Pin rolls back to the first version and returns the updated view.
	view := decode[map[string]any](t, authRequest(t, http.MethodPost, webServer.URL+"/api/v1/skills/feature-dev:code-reviewer/pin", writeSecret, map[string]any{"version": installed.Version}))
	if view["active"] != installed.Version {
		t.Fatalf("pinned view = %#v", view)
	}
	if versions, ok := view["versions"].([]any); !ok || len(versions) != 2 {
		t.Fatalf("versions = %#v", view["versions"])
	}

	// Read scope cannot pin or update.
	response := authRequest(t, http.MethodPost, webServer.URL+"/api/v1/skills/feature-dev:code-reviewer/pin", readSecret, map[string]any{"version": installed.Version})
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("read-scope pin status = %d, want 403", response.StatusCode)
	}
	response.Body.Close()

	// Pinning an unknown version is 400, an unknown skill is 404.
	response = authRequest(t, http.MethodPost, webServer.URL+"/api/v1/skills/feature-dev:code-reviewer/pin", writeSecret, map[string]any{"version": "missing"})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown version status = %d, want 400", response.StatusCode)
	}
	response.Body.Close()
	response = authRequest(t, http.MethodPost, webServer.URL+"/api/v1/skills/missing/pin", writeSecret, map[string]any{"version": installed.Version})
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown skill pin status = %d, want 404", response.StatusCode)
	}
	response.Body.Close()
	response = authRequest(t, http.MethodPost, webServer.URL+"/api/v1/skills/missing/update", writeSecret, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown skill update status = %d, want 404", response.StatusCode)
	}
	response.Body.Close()

	// A pin without a version is 400.
	response = authRequest(t, http.MethodPost, webServer.URL+"/api/v1/skills/feature-dev:code-reviewer/pin", writeSecret, map[string]any{})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("pin without version status = %d, want 400", response.StatusCode)
	}
	response.Body.Close()

	// The GET path reflects the pin.
	final := decode[map[string]any](t, authRequest(t, http.MethodGet, webServer.URL+"/api/v1/skills/feature-dev:code-reviewer", readSecret, nil))
	if final["active"] != installed.Version {
		t.Fatalf("active after pin = %#v", final["active"])
	}
}

// TestSkillsAPIUpload covers the multipart zip and SKILL.md uploads plus the
// rejection cases.
func TestSkillsAPIUpload(t *testing.T) {
	_, webServer, readSecret, writeSecret := newSkillsTestServer(t)
	defer webServer.Close()
	endpoint := webServer.URL + "/api/v1/skills/upload"

	// A read token cannot upload.
	if response := uploadSkill(t, endpoint, readSecret, "file", "SKILL.md", []byte(testSkillMarkdown)); response.StatusCode != http.StatusForbidden {
		t.Fatalf("read-scope upload status = %d, want 403", response.StatusCode)
	} else {
		response.Body.Close()
	}

	// A bare SKILL.md installs.
	response := uploadSkill(t, endpoint, writeSecret, "file", "SKILL.md", []byte(testSkillMarkdown))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("SKILL.md upload status = %d", response.StatusCode)
	}
	response.Body.Close()

	// A zip archive installs a second skill.
	archive := zipSkill(t, map[string]string{"SKILL.md": strings.Replace(testSkillMarkdown, "feature-dev:code-reviewer", "docs-reviewer", 1)})
	response = uploadSkill(t, endpoint, writeSecret, "file", "skill.zip", archive)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("zip upload status = %d", response.StatusCode)
	}
	response.Body.Close()

	listed := decode[struct {
		Skills []map[string]any `json:"skills"`
	}](t, authRequest(t, http.MethodGet, webServer.URL+"/api/v1/skills", readSecret, nil))
	if len(listed.Skills) != 2 {
		t.Fatalf("uploaded skills = %#v", listed.Skills)
	}

	// An uploaded skill records a stable sentinel source, never the deleted
	// staging path.
	sources := map[string]any{}
	for _, skill := range listed.Skills {
		sources[skill["name"].(string)] = skill["source"]
	}
	if sources["feature-dev:code-reviewer"] != "upload:feature-dev:code-reviewer" || sources["docs-reviewer"] != "upload:docs-reviewer" {
		t.Fatalf("uploaded sources = %#v", sources)
	}

	// Re-uploading an uploaded skill adds an active version: the documented way
	// to update a skill whose staging path the server deleted.
	response = uploadSkill(t, endpoint, writeSecret, "file", "SKILL.md", []byte(strings.Replace(testSkillMarkdown, "Review the diff.", "Review the diff twice.", 1)))
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("re-upload status = %d, want 201", response.StatusCode)
	}
	var reuploaded skills.InstallResult
	if err := json.NewDecoder(response.Body).Decode(&reuploaded); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if !reuploaded.Added || reuploaded.Active != reuploaded.Version {
		t.Fatalf("re-upload result = %#v, want a new active version", reuploaded)
	}
	versions := decode[struct {
		Versions []skills.Version `json:"versions"`
	}](t, authRequest(t, http.MethodGet, webServer.URL+"/api/v1/skills/feature-dev:code-reviewer", readSecret, nil))
	if len(versions.Versions) != 2 {
		t.Fatalf("versions after re-upload = %d, want 2", len(versions.Versions))
	}

	// Updating an uploaded skill reports it must be re-uploaded instead of
	// failing to stat the removed temporary file.
	response = authRequest(t, http.MethodPost, webServer.URL+"/api/v1/skills/feature-dev:code-reviewer/update", writeSecret, nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("uploaded update status = %d, want 400", response.StatusCode)
	}
	var failure struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&failure); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if !strings.Contains(failure.Error, "re-upload") {
		t.Fatalf("uploaded update error = %q, want a re-upload message", failure.Error)
	}

	// Unsupported upload types and a missing field are rejected.
	response = uploadSkill(t, endpoint, writeSecret, "file", "notes.txt", []byte("nothing"))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsupported upload status = %d, want 400", response.StatusCode)
	}
	response.Body.Close()
	response = uploadSkill(t, endpoint, writeSecret, "upload", "SKILL.md", []byte(testSkillMarkdown))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing file field status = %d, want 400", response.StatusCode)
	}
	response.Body.Close()

	// A zip whose root has no SKILL.md is rejected.
	response = uploadSkill(t, endpoint, writeSecret, "file", "empty.zip", zipSkill(t, map[string]string{"README.md": "no skill"}))
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("skill-less zip status = %d, want 400", response.StatusCode)
	}
	response.Body.Close()
}

// TestSkillsAPIUninstall covers the delete endpoint and its scope rule.
func TestSkillsAPIUninstall(t *testing.T) {
	_, webServer, readSecret, writeSecret := newSkillsTestServer(t)
	defer webServer.Close()
	source := writeSkillSource(t, testSkillMarkdown)
	decode[skills.InstallResult](t, authRequest(t, http.MethodPost, webServer.URL+"/api/v1/skills", writeSecret, map[string]any{"source": source}))

	// Read scope cannot delete.
	response := authRequest(t, http.MethodDelete, webServer.URL+"/api/v1/skills/feature-dev:code-reviewer", readSecret, nil)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("read-scope delete status = %d, want 403", response.StatusCode)
	}
	response.Body.Close()

	// Write scope deletes; the skill is then gone.
	response = authRequest(t, http.MethodDelete, webServer.URL+"/api/v1/skills/feature-dev:code-reviewer", writeSecret, nil)
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", response.StatusCode)
	}
	response.Body.Close()
	response = authRequest(t, http.MethodGet, webServer.URL+"/api/v1/skills/feature-dev:code-reviewer", readSecret, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted skill status = %d, want 404", response.StatusCode)
	}
	response.Body.Close()

	// Deleting it again is 404.
	response = authRequest(t, http.MethodDelete, webServer.URL+"/api/v1/skills/feature-dev:code-reviewer", writeSecret, nil)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete status = %d, want 404", response.StatusCode)
	}
	response.Body.Close()
}

// TestSkillsAPIBadInput covers the remaining validation paths.
func TestSkillsAPIBadInput(t *testing.T) {
	_, webServer, _, writeSecret := newSkillsTestServer(t)
	defer webServer.Close()

	// An unknown source path is rejected.
	response := authRequest(t, http.MethodPost, webServer.URL+"/api/v1/skills", writeSecret, map[string]any{"source": filepath.Join(t.TempDir(), "missing")})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown source status = %d, want 400", response.StatusCode)
	}
	response.Body.Close()

	// A missing source field is rejected.
	response = authRequest(t, http.MethodPost, webServer.URL+"/api/v1/skills", writeSecret, map[string]any{})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty source status = %d, want 400", response.StatusCode)
	}
	response.Body.Close()

	// Malformed JSON is rejected.
	malformed, err := http.NewRequest(http.MethodPost, webServer.URL+"/api/v1/skills", strings.NewReader(`{"source":`))
	if err != nil {
		t.Fatal(err)
	}
	malformed.Header.Set("Authorization", "Bearer "+writeSecret)
	malformed.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(malformed)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed JSON status = %d, want 400", response.StatusCode)
	}
	response.Body.Close()

	// A skill name that cannot address a directory is rejected.
	response = authRequest(t, http.MethodGet, webServer.URL+"/api/v1/skills/bad%5Cname", writeSecret, nil)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unusable skill name status = %d, want 400", response.StatusCode)
	}
	response.Body.Close()
}

// TestMCPSkillsTools pins the MCP contract for the skills library.
func TestMCPSkillsTools(t *testing.T) {
	_, webServer, readSecret, writeSecret := newSkillsTestServer(t)
	defer webServer.Close()
	source := writeSkillSource(t, testSkillMarkdown)

	call := func(token, method string, params any, id int) map[string]any {
		t.Helper()
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
		return payload
	}
	callTool := func(token, name string, arguments map[string]any, id int) string {
		t.Helper()
		return mcpText(call(token, "tools/call", map[string]any{"name": name, "arguments": arguments}, id))
	}

	// tools/list advertises every skills tool.
	payload := call(readSecret, "tools/list", map[string]any{}, 1)
	tools, _ := payload["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, tool := range tools {
		if entry, ok := tool.(map[string]any); ok {
			names[entry["name"].(string)] = true
		}
	}
	for _, want := range []string{"list_skills", "get_skill", "install_skill", "update_skill", "pin_skill", "uninstall_skill"} {
		if !names[want] {
			t.Fatalf("tools/list is missing %q", want)
		}
	}

	// list_skills is a read tool and starts empty.
	if text := callTool(readSecret, "list_skills", map[string]any{}, 2); !strings.Contains(text, `"skills"`) {
		t.Fatalf("list_skills = %s", text)
	}

	// A read token cannot install.
	if text := callTool(readSecret, "install_skill", map[string]any{"source": source}, 3); !strings.Contains(text, "write-scope") {
		t.Fatalf("read-scope install_skill = %s", text)
	}

	// A write token installs, then reads back through the tools.
	if text := callTool(writeSecret, "install_skill", map[string]any{"source": source}, 4); !strings.Contains(text, "feature-dev:code-reviewer") {
		t.Fatalf("install_skill = %s", text)
	}
	listText := callTool(readSecret, "list_skills", map[string]any{}, 5)
	if !strings.Contains(listText, "feature-dev:code-reviewer") || !strings.Contains(listText, "used_by") {
		t.Fatalf("list_skills after install = %s", listText)
	}
	if text := callTool(readSecret, "get_skill", map[string]any{"name": "feature-dev:code-reviewer"}, 6); !strings.Contains(text, `"versions"`) {
		t.Fatalf("get_skill = %s", text)
	}

	// uninstall_skill removes it.
	if text := callTool(writeSecret, "uninstall_skill", map[string]any{"name": "feature-dev:code-reviewer"}, 7); !strings.Contains(text, "removed") {
		t.Fatalf("uninstall_skill = %s", text)
	}
	if text := callTool(readSecret, "get_skill", map[string]any{"name": "feature-dev:code-reviewer"}, 8); !strings.Contains(text, "not installed") {
		t.Fatalf("get_skill after uninstall = %s", text)
	}
}

// mcpText extracts the first text content item from a tools/call result.
func mcpText(payload map[string]any) string {
	result, ok := payload["result"].(map[string]any)
	if !ok {
		return ""
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		return ""
	}
	item, ok := content[0].(map[string]any)
	if !ok {
		return ""
	}
	text, _ := item["text"].(string)
	return text
}
