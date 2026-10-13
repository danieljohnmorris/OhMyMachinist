package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/owainlewis/machinist/internal/config"
)

func TestShippedProfilesValidate(t *testing.T) {
	for name, body := range ShippedReviewProfiles() {
		if issues := validateProfileBody(body); len(issues) > 0 {
			t.Errorf("shipped profile %s has validation issues: %+v", name, issues)
		}
	}
	if _, ok := ShippedReviewProfiles()["default"]; !ok {
		t.Fatal("shipped default profile missing")
	}
	if _, ok := ShippedReviewProfiles()["dan-pr-review"]; !ok {
		t.Fatal("shipped dan-pr-review profile missing")
	}
}

func TestValidateProfileBodyLineNumbers(t *testing.T) {
	body := "name: bad\nversion: 1\nreviewers:\n  - name: a\n    prompt: x\nwat: true\n"
	issues := validateProfileBody(body)
	if len(issues) == 0 {
		t.Fatal("expected validation issues")
	}
	found := false
	for _, issue := range issues {
		if strings.Contains(issue.Message, "unknown top-level key: wat") {
			if issue.Line != 6 {
				t.Errorf("expected line 6 for wat, got %d", issue.Line)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("missing unknown-key issue: %+v", issues)
	}
}

func TestValidateProfileBodyRejectsUnknownPassRef(t *testing.T) {
	body := "name: bad\nversion: 1\nreviewers:\n  - name: a\n    prompt: x\nrouting:\n  fallback: [ghost]\n"
	issues := validateProfileBody(body)
	if len(issues) == 0 || !strings.Contains(issues[0].Message, "unknown pass: ghost") {
		t.Fatalf("expected unknown-pass issue, got %+v", issues)
	}
}

func TestGlobMatches(t *testing.T) {
	cases := []struct {
		pattern, target string
		want            bool
	}{
		{"**/*.ts", "a/b/c.ts", true},
		{"**/*.ts", "x.ts", true},
		{"**/*.ts", "a/b/c.py", false},
		{"scripts/**", "scripts/nested/x.sh", true},
		{"scripts/**", "scripts", false},
		{"Cargo.toml", "nested/Cargo.toml", true},
		{"src/*.md", "src/posts/a.md", false},
		{"src/*.md", "src/a.md", true},
	}
	for _, test := range cases {
		if got := globMatches(test.pattern, test.target); got != test.want {
			t.Errorf("globMatches(%q, %q) = %v, want %v", test.pattern, test.target, got, test.want)
		}
	}
}

func TestReviewProfileResolutionPrecedence(t *testing.T) {
	ctx := context.Background()
	server, _ := newTestHTTPServer(t)
	store := server.store

	// 1. No configuration: the shipped default is served.
	source, name, body, _, err := server.resolveReviewProfileForProject(ctx, "omacom/mobile")
	if err != nil {
		t.Fatal(err)
	}
	if source != "default" || name != "default" || !strings.Contains(body, "name: default") {
		t.Fatalf("expected shipped default, got source=%s name=%s", source, name)
	}

	// 2. Config default profile (dan-pr-review via config) is used when the
	// project has no setting and no settings-table default exists.
	configServer := &Server{store: store, reviewDefaultProfile: "dan-pr-review"}
	source, name, _, _, err = configServer.resolveReviewProfileForProject(ctx, "omacom/mobile")
	if err != nil {
		t.Fatal(err)
	}
	if source != "global" || name != "dan-pr-review" {
		t.Fatalf("expected config default dan-pr-review, got source=%s name=%s", source, name)
	}

	// 3. The settings-table default outranks the config value.
	if err := store.SaveSetting(ctx, settingReviewDefaultProfile, "dan-pr-review"); err != nil {
		t.Fatal(err)
	}
	source, name, _, _, err = server.resolveReviewProfileForProject(ctx, "omacom/mobile")
	if err != nil {
		t.Fatal(err)
	}
	if source != "global" || name != "dan-pr-review" {
		t.Fatalf("expected settings default, got source=%s name=%s", source, name)
	}

	// 4. A named library profile on the project outranks the global default.
	if err := store.SaveProjectReviewSetting(ctx, "omacom/mobile", ProjectReviewSetting{ProfileName: "default"}); err != nil {
		t.Fatal(err)
	}
	source, name, _, _, err = server.resolveReviewProfileForProject(ctx, "omacom/mobile")
	if err != nil {
		t.Fatal(err)
	}
	if source != "project" || name != "default" {
		t.Fatalf("expected project library profile, got source=%s name=%s", source, name)
	}

	// 5. An inline project profile body outranks everything else.
	inline := "name: inline\nversion: 1\nreviewers:\n  - name: a\n    prompt: hi\n"
	if err := store.SaveProjectReviewSetting(ctx, "omacom/mobile", ProjectReviewSetting{ProfileBody: inline, ExtraInstructions: "extra rules"}); err != nil {
		t.Fatal(err)
	}
	source, name, body, extra, err := server.resolveReviewProfileForProject(ctx, "omacom/mobile")
	if err != nil {
		t.Fatal(err)
	}
	if source != "project" || name != "inline" || body != inline || extra != "extra rules" {
		t.Fatalf("expected inline project profile, got source=%s name=%s body=%q extra=%q", source, name, body, extra)
	}
}

func TestReviewInstructionsMigratedToExtraInstructions(t *testing.T) {
	directory := t.TempDir()
	database := filepath.Join(directory, "machinist.db")
	store := openTestStore(t, database)

	// Simulate a MACH-26 database: legacy rows present, migration not run.
	if _, err := store.db.Exec(`INSERT INTO review_instructions(repository, instructions, updated_at) VALUES('omacom/mobile', 'Legacy rules.', '2025-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DELETE FROM settings WHERE key = ?`, settingReviewInstructionsMigrated); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DELETE FROM project_review_settings WHERE repository = 'omacom/mobile'`); err != nil {
		t.Fatal(err)
	}
	store.Close()

	reopened, err := OpenStore(database)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	setting, found, err := reopened.ProjectReviewSetting(context.Background(), "omacom/mobile")
	if err != nil || !found {
		t.Fatalf("expected migrated setting, found=%v err=%v", found, err)
	}
	if setting.ExtraInstructions != "Legacy rules." {
		t.Fatalf("expected legacy text as extra_instructions, got %q", setting.ExtraInstructions)
	}
	// Idempotent: reopening again does not duplicate or revert.
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenStore(database)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	setting, _, _ = again.ProjectReviewSetting(context.Background(), "omacom/mobile")
	if setting.ExtraInstructions != "Legacy rules." {
		t.Fatalf("migration not idempotent: %q", setting.ExtraInstructions)
	}
}

func TestReviewProfileHTTP(t *testing.T) {
	server, endpoint := newTestHTTPServer(t)
	server.githubRepositories = map[string]string{"mobile": "omacom/mobile"}
	csrf := server.csrfToken
	ctx := context.Background()

	// The shipped default resolves for an unconfigured project.
	response, err := http.Get(endpoint.URL + "/api/v1/review-profile?project=mobile")
	if err != nil {
		t.Fatal(err)
	}
	var payload reviewProfileResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if payload.Source != "default" || payload.ProfileName != "default" || !strings.Contains(payload.Profile, "name: default") {
		t.Fatalf("unexpected resolved profile: %+v", payload)
	}

	put := func(path, body string) *http.Response {
		request, err := http.NewRequest(http.MethodPut, endpoint.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Machinist-CSRF", csrf)
		request.Header.Set("Origin", endpoint.URL)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}

	// Saving an invalid profile is blocked with line-numbered errors.
	denied := put("/api/v1/review-profile", `{"project":"mobile","profile_body":"name: x\nversion: 1\nwat: 1\n"}`)
	if denied.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid profile, got %d: %s", denied.StatusCode, readAllBody(denied))
	}
	denied.Body.Close()

	// Saving a valid inline profile persists it and resolves it back.
	inline := "name: mobile-profile\nversion: 1\nreviewers:\n  - name: a\n    prompt: hi\n"
	accepted := put("/api/v1/review-profile", `{"project":"mobile","profile_body":`+jsonString(inline)+`}`)
	if accepted.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", accepted.StatusCode, readAllBody(accepted))
	}
	accepted.Body.Close()
	response, err = http.Get(endpoint.URL + "/api/v1/review-profile?project=mobile")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if payload.Source != "project" || payload.ProfileName != "mobile-profile" {
		t.Fatalf("expected project profile, got %+v", payload)
	}

	// The library list includes shipped profiles.
	response, err = http.Get(endpoint.URL + "/api/v1/review-profiles")
	if err != nil {
		t.Fatal(err)
	}
	var list reviewProfileListResponse
	if err := json.NewDecoder(response.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if list.Default != "default" {
		t.Fatalf("expected default name, got %q", list.Default)
	}
	var shippedSeen bool
	for _, profile := range list.Profiles {
		if profile.Name == "dan-pr-review" && profile.BuiltIn {
			shippedSeen = true
		}
	}
	if !shippedSeen {
		t.Fatal("expected dan-pr-review in the library list")
	}

	// Setting the global default persists.
	saved := put("/api/v1/review-default-profile", `{"name":"dan-pr-review"}`)
	if saved.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for default-profile save, got %d: %s", saved.StatusCode, readAllBody(saved))
	}
	saved.Body.Close()
	if name, err := server.defaultReviewProfileName(ctx); err != nil || name != "dan-pr-review" {
		t.Fatalf("expected dan-pr-review default, got %q err=%v", name, err)
	}
}

func TestReviewProfilePreview(t *testing.T) {
	_, endpoint := newTestHTTPServer(t)
	body := `{"project":"omacom/mobile","files":["src/content/a.md","main.go"]}`
	response, err := http.Post(endpoint.URL+"/api/v1/review-profile/preview", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.StatusCode, readAllBody(response))
	}
	var payload reviewPreviewResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	// The shipped default has no src/content routing; the fallback reviewers run.
	if payload.Source == "" || payload.Name == "" {
		t.Fatalf("unexpected preview source: %+v", payload)
	}
	if len(payload.Reviewers) == 0 {
		t.Fatal("expected reviewers in preview")
	}

	// A repo profile with prose routing shows the prose pass and matched lane.
	repoProfile := `{"project":"omacom/mobile","repo_profile":"name: preview-test\nversion: 1\nchecks:\n  lanes:\n    - lane: go\n      when: [\"**/*.go\"]\n      commands: [\"go build ./...\"]\nreviewers:\n  - name: code-reviewer\n    prompt: review\n  - name: prose\n    prompt: prose\nrouting:\n  fallback: [code-reviewer]\n  rules:\n    - when: [\"src/content/**/*.md\"]\n      passes: [prose]\n","files":["src/content/a.md","main.go"]}`
	response, err = http.Post(endpoint.URL+"/api/v1/review-profile/preview", "application/json", strings.NewReader(repoProfile))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.StatusCode, readAllBody(response))
	}
	var detailed reviewPreviewResponse
	if err := json.NewDecoder(response.Body).Decode(&detailed); err != nil {
		t.Fatal(err)
	}
	if detailed.Source != "repo" {
		t.Fatalf("expected repo source, got %q", detailed.Source)
	}
	passNames := map[string]bool{}
	for _, pass := range detailed.Reviewers {
		passNames[pass.Name] = true
	}
	if !passNames["prose"] || !passNames["code-reviewer"] {
		t.Fatalf("expected prose + code-reviewer passes, got %+v", detailed.Reviewers)
	}
	if len(detailed.Lanes) != 1 || detailed.Lanes[0].Lane != "go" || len(detailed.Lanes[0].Files) != 1 {
		t.Fatalf("expected one go lane with main.go, got %+v", detailed.Lanes)
	}
}

func TestReviewConfigDefaultProfile(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.toml")
	body := "[server]\ndatabase = \"" + filepath.Join(directory, "machinist.db") + "\"\nworker_token_file = \"" + filepath.Join(directory, "token") + "\"\n\n[review]\ndefault_profile = \"dan-pr-review\"\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Review.DefaultProfile != "dan-pr-review" {
		t.Fatalf("expected dan-pr-review default, got %q", loaded.Review.DefaultProfile)
	}
}

func jsonString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func readAllBody(response *http.Response) string {
	var buffer bytes.Buffer
	_, _ = buffer.ReadFrom(response.Body)
	return buffer.String()
}
