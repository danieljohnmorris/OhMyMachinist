package skills

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validSkill = "---\nname: feature-dev:code-reviewer\ndescription: Reviews changes\n---\n\nReview the diff.\n"

func newTestLibrary(t *testing.T) *Library {
	t.Helper()
	library, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	library.Scan = func(context.Context, string) (ScanResult, error) {
		return ScanResult{Scanned: true}, nil
	}
	library.Now = func() time.Time { return time.Unix(1700000000, 0).UTC() }
	return library
}

func writeSkillDir(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = name
}

func TestParseSourceClassifiesInputs(t *testing.T) {
	directory := t.TempDir()
	archive := filepath.Join(t.TempDir(), "skill.zip")
	if err := os.WriteFile(archive, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		input string
		kind  SourceKind
	}{
		{"https://github.com/owner/repo/blob/main/skills/foo/SKILL.md", SourceGitHub},
		{"https://github.com/owner/repo/tree/main/skills/foo", SourceGitHub},
		{directory, SourceDirectory},
		{archive, SourceZip},
	} {
		source, err := ParseSource(test.input)
		if err != nil {
			t.Fatalf("ParseSource(%q): %v", test.input, err)
		}
		if source.Kind != test.kind {
			t.Fatalf("ParseSource(%q) kind = %v, want %v", test.input, source.Kind, test.kind)
		}
	}
	if _, err := ParseSource("not-a-real-source"); err == nil {
		t.Fatal("ParseSource accepted a missing path")
	}
}

func TestParseGitHubSourceOverridesRef(t *testing.T) {
	for _, test := range []struct {
		input string
		dir   string
		ref   string
	}{
		{"https://github.com/owner/repo/tree/main/skills/foo@v1.2.3", "skills/foo", "v1.2.3"},
		{"https://github.com/owner/repo/blob/main/skills/foo/SKILL.md", "skills/foo", "main"},
		{"https://github.com/owner/repo/raw/main/SKILL.md", "", "main"},
		{"https://github.com/owner/repo", "", "HEAD"},
	} {
		parsed, err := parseGitHubSource(test.input)
		if err != nil {
			t.Fatalf("parseGitHubSource(%q): %v", test.input, err)
		}
		if parsed.dir != test.dir || parsed.ref != test.ref {
			t.Fatalf("parseGitHubSource(%q) = dir %q ref %q, want dir %q ref %q", test.input, parsed.dir, parsed.ref, test.dir, test.ref)
		}
	}
	if _, err := parseGitHubSource("https://github.com/owner/repo/blob/main/README.md"); err == nil {
		t.Fatal("parseGitHubSource accepted a non-SKILL.md blob")
	}
}

func TestInstallFromDirectory(t *testing.T) {
	library := newTestLibrary(t)
	directory := t.TempDir()
	writeSkillDir(t, directory, "reviewer", validSkill)

	result, err := library.Install(context.Background(), Source{Kind: SourceDirectory, Location: directory})
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "feature-dev:code-reviewer" || !result.Added || result.Active == "" || result.Active != result.Version {
		t.Fatalf("install result = %#v", result)
	}
	installed, err := os.ReadFile(filepath.Join(library.Root, "feature-dev_code-reviewer", result.Version, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(installed) != validSkill {
		t.Fatalf("installed SKILL.md = %q", installed)
	}
	manifest := readManifestFile(t, library.Root, "feature-dev_code-reviewer")
	if manifest.Active != result.Version || len(manifest.Versions) != 1 || !manifest.Versions[0].Scanned {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestInstallFromZip(t *testing.T) {
	library := newTestLibrary(t)
	archive := filepath.Join(t.TempDir(), "skill.zip")
	writeSkillZip(t, archive, map[string]string{"SKILL.md": validSkill, "notes/extra.md": "extra\n"})

	result, err := library.Install(context.Background(), Source{Kind: SourceZip, Location: archive})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(library.Root, "feature-dev_code-reviewer", result.Version, "notes", "extra.md")); err != nil {
		t.Fatalf("nested file not installed: %v", err)
	}
}

// TestInstallFromUploadRecordsSentinel pins the source recorded for an upload:
// the caller's staged bytes are gone once the install finishes, so the
// manifest holds an "upload:" sentinel and Update asks for a re-upload instead
// of statting the deleted temporary path.
func TestInstallFromUploadRecordsSentinel(t *testing.T) {
	library := newTestLibrary(t)
	directory := t.TempDir()
	writeSkillDir(t, directory, "reviewer", validSkill)

	result, err := library.Install(context.Background(), Source{Kind: SourceDirectory, Location: directory, Uploaded: true})
	if err != nil {
		t.Fatal(err)
	}
	const want = "upload:feature-dev:code-reviewer"
	if result.Source != want {
		t.Fatalf("uploaded install source = %q, want %q", result.Source, want)
	}
	manifest := readManifestFile(t, library.Root, "feature-dev_code-reviewer")
	if manifest.Source != want {
		t.Fatalf("manifest source = %q, want %q", manifest.Source, want)
	}
	if strings.Contains(manifest.Source, directory) {
		t.Fatalf("manifest source leaked the staged path: %q", manifest.Source)
	}
	if _, err := library.Update(context.Background(), "feature-dev:code-reviewer", false); err == nil || !strings.Contains(err.Error(), "re-upload") {
		t.Fatalf("update of an uploaded skill = %v, want a re-upload error", err)
	}
}

func TestInstallFromGitHubResolvesRefToSHA(t *testing.T) {
	library := newTestLibrary(t)
	const sha = "0123456789abcdef0123456789abcdef01234567"
	server := fakeGitHub(t, sha, map[string]string{"skills/reviewer/SKILL.md": validSkill})
	defer server.Close()
	library.GitHub = &GitHub{APIBase: server.URL, RawBase: server.URL + "/raw", Client: server.Client()}

	result, err := library.Install(context.Background(), Source{Kind: SourceGitHub, Location: "https://github.com/owner/repo/tree/main/skills/reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != sha || result.Active != sha {
		t.Fatalf("version = %q, active = %q, want %s", result.Version, result.Active, sha)
	}
	manifest := readManifestFile(t, library.Root, "feature-dev_code-reviewer")
	if manifest.Source != "https://github.com/owner/repo/tree/main/skills/reviewer" {
		t.Fatalf("manifest source = %q", manifest.Source)
	}
	if manifest.Versions[0].Resolved != sha {
		t.Fatalf("resolved = %q, want %s", manifest.Versions[0].Resolved, sha)
	}
}

func TestInstallReadsTokenWithoutRecordingIt(t *testing.T) {
	library := newTestLibrary(t)
	const sha = "abcdefabcdefabcdefabcdefabcdefabcdefabcd"
	const token = "ghp_supersecrettokenvalue"
	server := fakeGitHubWithToken(t, sha, map[string]string{"skills/reviewer/SKILL.md": validSkill}, token)
	defer server.Close()
	library.GitHub = &GitHub{APIBase: server.URL, RawBase: server.URL + "/raw", Token: token, Client: server.Client()}

	result, err := library.Install(context.Background(), Source{Kind: SourceGitHub, Location: "https://github.com/owner/repo/tree/main/skills/reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join(library.Root, "feature-dev_code-reviewer", manifestName))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(manifest, []byte(token)) {
		t.Fatal("manifest contains the GitHub token")
	}
	if bytes.Contains([]byte(result.Source), []byte(token)) {
		t.Fatal("source contains the GitHub token")
	}
}

func TestUpdateKeepsOldVersionAndActive(t *testing.T) {
	library := newTestLibrary(t)
	directory := t.TempDir()
	writeSkillDir(t, directory, "reviewer", validSkill)
	first, err := library.Install(context.Background(), Source{Kind: SourceDirectory, Location: directory})
	if err != nil {
		t.Fatal(err)
	}

	updated := strings.Replace(validSkill, "Review the diff.", "Review the diff, carefully.", 1)
	writeSkillDir(t, directory, "reviewer", updated)
	second, err := library.Update(context.Background(), "feature-dev:code-reviewer", false)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Added || second.Version == first.Version {
		t.Fatalf("update result = %#v", second)
	}
	if second.Active != first.Version {
		t.Fatalf("active = %q, want the original %q", second.Active, first.Version)
	}
	manifest := readManifestFile(t, library.Root, "feature-dev_code-reviewer")
	if len(manifest.Versions) != 2 {
		t.Fatalf("versions = %#v", manifest.Versions)
	}
	if _, err := os.Stat(filepath.Join(library.Root, "feature-dev_code-reviewer", first.Version, "SKILL.md")); err != nil {
		t.Fatalf("old version removed: %v", err)
	}

	third, err := library.Update(context.Background(), "feature-dev:code-reviewer", true)
	if err != nil {
		t.Fatal(err)
	}
	if third.Added {
		t.Fatalf("unchanged source added a version: %#v", third)
	}
	if third.Active != second.Version {
		t.Fatalf("activate did not switch the active version: %#v", third)
	}
}

func TestPinRollsBack(t *testing.T) {
	library := newTestLibrary(t)
	directory := t.TempDir()
	writeSkillDir(t, directory, "reviewer", validSkill)
	first, err := library.Install(context.Background(), Source{Kind: SourceDirectory, Location: directory})
	if err != nil {
		t.Fatal(err)
	}
	writeSkillDir(t, directory, "reviewer", strings.Replace(validSkill, "Review the diff.", "Second revision.", 1))
	second, err := library.Update(context.Background(), "feature-dev:code-reviewer", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := library.Pin("feature-dev:code-reviewer", first.Version); err != nil {
		t.Fatal(err)
	}
	manifest := readManifestFile(t, library.Root, "feature-dev_code-reviewer")
	if manifest.Active != first.Version || second.Active != second.Version {
		t.Fatalf("manifest active = %q, want %q", manifest.Active, first.Version)
	}
	if err := library.Pin("feature-dev:code-reviewer", "missing"); err == nil {
		t.Fatal("Pin accepted an unknown version")
	}
}

func TestUninstall(t *testing.T) {
	library := newTestLibrary(t)
	directory := t.TempDir()
	writeSkillDir(t, directory, "reviewer", validSkill)
	if _, err := library.Install(context.Background(), Source{Kind: SourceDirectory, Location: directory}); err != nil {
		t.Fatal(err)
	}
	if err := library.Uninstall("feature-dev:code-reviewer", UninstallOptions{Referenced: true}); err == nil {
		t.Fatal("Uninstall removed a referenced skill without force")
	}
	if _, err := os.Stat(filepath.Join(library.Root, "feature-dev_code-reviewer")); err != nil {
		t.Fatalf("referenced skill was removed: %v", err)
	}
	if err := library.Uninstall("feature-dev:code-reviewer", UninstallOptions{Referenced: true, Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(library.Root, "feature-dev_code-reviewer")); !os.IsNotExist(err) {
		t.Fatalf("skill still present after uninstall: %v", err)
	}
	if err := library.Uninstall("feature-dev:code-reviewer", UninstallOptions{}); err == nil {
		t.Fatal("Uninstall accepted an unknown skill")
	}
}

func TestInstallRejectsBadFrontmatter(t *testing.T) {
	library := newTestLibrary(t)
	for _, body := range []string{
		"# no frontmatter\n",
		"---\nname: only-name\n---\n",
		"---\ndescription: no name\n---\n",
	} {
		directory := t.TempDir()
		writeSkillDir(t, directory, "reviewer", body)
		if _, err := library.Install(context.Background(), Source{Kind: SourceDirectory, Location: directory}); err == nil {
			t.Fatalf("Install accepted bad frontmatter %q", body)
		}
	}
}

func TestInstallRefusesDuplicateInstall(t *testing.T) {
	library := newTestLibrary(t)
	directory := t.TempDir()
	writeSkillDir(t, directory, "reviewer", validSkill)
	source := Source{Kind: SourceDirectory, Location: directory}
	if _, err := library.Install(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if _, err := library.Install(context.Background(), source); err == nil {
		t.Fatal("Install overwrote an installed skill")
	}
}

func TestGitleaksFindingsRejectVersion(t *testing.T) {
	library := newTestLibrary(t)
	library.Scan = func(context.Context, string) (ScanResult, error) {
		return ScanResult{Scanned: true, Findings: []string{"generic-api-key: SKILL.md:1"}}, nil
	}
	directory := t.TempDir()
	writeSkillDir(t, directory, "reviewer", validSkill)
	if _, err := library.Install(context.Background(), Source{Kind: SourceDirectory, Location: directory}); err == nil || !strings.Contains(err.Error(), "gitleaks") {
		t.Fatalf("Install error = %v", err)
	}
	installed, err := os.ReadDir(library.Root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range installed {
		if strings.HasPrefix(entry.Name(), "feature-dev") {
			t.Fatalf("rejected skill left files behind: %s", entry.Name())
		}
	}
}

func TestMissingScannerRecordsUnscannedAndWarns(t *testing.T) {
	library := newTestLibrary(t)
	library.Scan = func(context.Context, string) (ScanResult, error) { return ScanResult{Scanned: false}, nil }
	var warnings []string
	library.Warn = func(message string) { warnings = append(warnings, message) }
	directory := t.TempDir()
	writeSkillDir(t, directory, "reviewer", validSkill)
	if _, err := library.Install(context.Background(), Source{Kind: SourceDirectory, Location: directory}); err != nil {
		t.Fatal(err)
	}
	manifest := readManifestFile(t, library.Root, "feature-dev_code-reviewer")
	if manifest.Versions[0].Scanned {
		t.Fatal("manifest recorded a scan that did not run")
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "gitleaks") {
		t.Fatalf("warnings = %#v", warnings)
	}
}

func TestGitleaksScannerWithoutBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	result, err := GitleaksScanner(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if result.Scanned {
		t.Fatal("scanner reported a scan without gitleaks on PATH")
	}
}

func TestListAndVersions(t *testing.T) {
	library := newTestLibrary(t)
	directory := t.TempDir()
	writeSkillDir(t, directory, "reviewer", validSkill)
	if _, err := library.Install(context.Background(), Source{Kind: SourceDirectory, Location: directory}); err != nil {
		t.Fatal(err)
	}
	skills, err := library.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || skills[0].Name != "feature-dev:code-reviewer" || skills[0].Active == "" {
		t.Fatalf("List = %#v", skills)
	}
	manifest, err := library.Versions("feature-dev:code-reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Active != skills[0].Active {
		t.Fatalf("Versions active = %q, want %q", manifest.Active, skills[0].Active)
	}
}

func TestProfileReferences(t *testing.T) {
	body := `
reviewers:
  - name: reviewer
    skills: ["feature-dev:code-reviewer", "rust-review"]
  - name: docs
    prompt_from: "docs-reviewer#How to review"
`
	references := ProfileReferences(body)
	for _, want := range []string{"feature-dev:code-reviewer", "rust-review", "docs-reviewer"} {
		if !contains(references, want) {
			t.Fatalf("ProfileReferences = %#v, missing %q", references, want)
		}
	}
}

func readManifestFile(t *testing.T, root, key string) Manifest {
	t.Helper()
	manifest, err := readManifest(filepath.Join(root, key))
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func writeSkillZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	writer := zip.NewWriter(file)
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
}

func fakeGitHub(t *testing.T, sha string, files map[string]string) *httptest.Server {
	t.Helper()
	return fakeGitHubWithToken(t, sha, files, "")
}

func fakeGitHubWithToken(t *testing.T, sha string, files map[string]string, token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/owner/repo/commits/main", func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(fmt.Sprintf(`{"sha": %q}`, sha)))
	})
	mux.HandleFunc("/repos/owner/repo/git/trees/"+sha, func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		tree := make([]map[string]string, 0, len(files))
		for name := range files {
			tree = append(tree, map[string]string{"path": name, "type": "blob"})
		}
		body, err := json.Marshal(map[string]any{"tree": tree})
		if err != nil {
			t.Fatal(err)
		}
		w.Write(body)
	})
	mux.HandleFunc("/raw/", func(w http.ResponseWriter, r *http.Request) {
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/raw/owner/repo/"+sha+"/")
		body, ok := files[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(body))
	})
	return httptest.NewServer(mux)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
