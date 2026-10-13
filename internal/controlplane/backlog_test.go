package controlplane

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/owainlewis/machinist/internal/backlog"
)

func TestBacklogIssueKeysSequencedPerProject(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	defer store.Close()
	first, err := store.CreateBacklogIssue(t.Context(), "MACH", "Fix the sidebar", "The sidebar overlaps the footer.", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateBacklogIssue(t.Context(), "MACH", "Add a dark theme", "Respect system preference.", "")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateBacklogIssue(t.Context(), "SPARK", "Speed up indexing", "Indexing takes minutes.", "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Key != "MACH-1" || second.Key != "MACH-2" || other.Key != "SPARK-1" {
		t.Fatalf("keys = %s, %s, %s", first.Key, second.Key, other.Key)
	}
	if first.State != backlog.StateBacklog || first.Revision != 1 || first.ID == "" {
		t.Fatalf("created issue = %#v", first)
	}
	// A failed create does not consume a sequence number.
	if _, err := store.CreateBacklogIssue(t.Context(), "MACH", " ", "", ""); err == nil {
		t.Fatal("empty title accepted")
	}
	third, err := store.CreateBacklogIssue(t.Context(), "MACH", "Write release notes", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if third.Key != "MACH-3" {
		t.Fatalf("key = %s, want MACH-3", third.Key)
	}
}

func TestBacklogCreateThenListAndPatch(t *testing.T) {
	_, web := newTestHTTPServer(t)
	defer web.Close()
	status := getStatus(t, web.URL)
	headers := map[string]string{"Origin": web.URL, "X-Machinist-CSRF": status.CSRFToken}

	created := postJSON(t, web.URL+"/api/v1/backlog", map[string]string{"project": "MACH", "title": "Fix the sidebar", "spec": "The sidebar overlaps the footer."}, headers)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create %d", created.StatusCode)
	}
	var issue backlog.Issue
	if err := json.NewDecoder(created.Body).Decode(&issue); err != nil {
		t.Fatal(err)
	}
	created.Body.Close()
	if issue.Key != "MACH-1" || issue.State != "backlog" || issue.Revision != 1 {
		t.Fatalf("created issue = %#v", issue)
	}

	// Creating and editing never enqueue a job.
	before := getStatus(t, web.URL)
	if len(before.Jobs) != 0 {
		t.Fatalf("create enqueued %d jobs", len(before.Jobs))
	}

	patch := patchJSON(t, web.URL+"/api/v1/backlog/"+issue.ID, map[string]any{"state": "todo"}, headers)
	if patch.StatusCode != http.StatusOK {
		t.Fatalf("patch %d", patch.StatusCode)
	}
	var updated backlog.Issue
	if err := json.NewDecoder(patch.Body).Decode(&updated); err != nil {
		t.Fatal(err)
	}
	patch.Body.Close()
	if updated.Revision != 2 || updated.State != "todo" {
		t.Fatalf("patched issue = %#v", updated)
	}

	response, err := http.Get(web.URL + "/api/v1/backlog?project=MACH&state=todo&q=sidebar")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var listed []backlog.Issue
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != issue.ID {
		t.Fatalf("listed = %#v", listed)
	}

	after := getStatus(t, web.URL)
	if len(after.Jobs) != 0 {
		t.Fatalf("create and patch enqueued %d jobs", len(after.Jobs))
	}
}

func TestBacklogStartPinsSpecAndStampsSource(t *testing.T) {
	s, web := newTestHTTPServer(t)
	defer web.Close()
	f, err := os.OpenFile(s.definitionPath, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("\n[workflows.plan]\nsteps=[{command='plan'}]\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := s.store.db.Exec(`INSERT INTO known_repositories(repository) VALUES('machinist')`); err != nil {
		t.Fatal(err)
	}
	status := getStatus(t, web.URL)
	headers := map[string]string{"Origin": web.URL, "X-Machinist-CSRF": status.CSRFToken}

	created := postJSON(t, web.URL+"/api/v1/backlog", map[string]string{"project": "MACH", "title": "Fix the sidebar", "spec": "The sidebar overlaps the footer."}, headers)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create %d", created.StatusCode)
	}
	var issue backlog.Issue
	if err := json.NewDecoder(created.Body).Decode(&issue); err != nil {
		t.Fatal(err)
	}
	created.Body.Close()

	started := postJSON(t, web.URL+"/api/v1/backlog/"+issue.ID+"/start", map[string]string{"repository": "machinist", "workflow": "plan"}, headers)
	if started.StatusCode != http.StatusCreated {
		t.Fatalf("start %d", started.StatusCode)
	}
	var startResponse struct {
		JobID string        `json:"job_id"`
		Issue backlog.Issue `json:"issue"`
	}
	if err := json.NewDecoder(started.Body).Decode(&startResponse); err != nil {
		t.Fatal(err)
	}
	started.Body.Close()
	if startResponse.JobID == "" {
		t.Fatal("start returned no job id")
	}
	if startResponse.Issue.State != "started" {
		t.Fatalf("issue state = %s, want started", startResponse.Issue.State)
	}

	job := jobByID(t, s.store, startResponse.JobID)
	if job.Task == nil || job.Task.Spec != "The sidebar overlaps the footer." {
		t.Fatalf("job spec not pinned: %+v", job.Task)
	}
	if job.Title != "MACH-1: Fix the sidebar" {
		t.Fatalf("job title = %q", job.Title)
	}
	if job.SourceKind != "backlog" || job.BacklogIssueID != issue.ID || job.BacklogIssueKey != "MACH-1" {
		t.Fatalf("job source identity = %q, %q, %q", job.SourceKind, job.BacklogIssueID, job.BacklogIssueKey)
	}

	// Editing the issue never rewrites the pinned job spec and never enqueues.
	patch := patchJSON(t, web.URL+"/api/v1/backlog/"+issue.ID, map[string]any{"spec": "Rewrite the sidebar entirely."}, headers)
	if patch.StatusCode != http.StatusOK {
		t.Fatalf("patch %d", patch.StatusCode)
	}
	patch.Body.Close()
	if job = jobByID(t, s.store, startResponse.JobID); job.Task == nil || job.Task.Spec != "The sidebar overlaps the footer." {
		t.Fatalf("pinned job spec changed: %+v", job.Task)
	}

	// A second start still creates work but moves the state only once.
	second := postJSON(t, web.URL+"/api/v1/backlog/"+issue.ID+"/start", map[string]string{"repository": "machinist", "workflow": "plan"}, headers)
	if second.StatusCode != http.StatusCreated {
		t.Fatalf("second start %d", second.StatusCode)
	}
	second.Body.Close()
	refreshed, err := s.store.GetBacklogIssue(t.Context(), issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.State != "started" || refreshed.Revision != 3 {
		t.Fatalf("issue after starts = %#v", refreshed)
	}
}

func TestBacklogStartOnceOnlyMovesBacklogAndTodo(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	defer store.Close()
	issue, err := store.CreateBacklogIssue(t.Context(), "MACH", "Fix the sidebar", "Overlap.", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.UpdateBacklogIssue(t.Context(), issue.ID, backlog.Update{State: new("completed")}); err != nil {
		t.Fatal(err)
	}
	if err = store.MarkBacklogStartedOnce(t.Context(), issue.ID); err != nil {
		t.Fatal(err)
	}
	refreshed, err := store.GetBacklogIssue(t.Context(), issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.State != backlog.StateCompleted {
		t.Fatalf("state = %s, want completed to stay human-controlled", refreshed.State)
	}
	if err = store.MarkBacklogStartedOnce(t.Context(), issue.ID); err != nil {
		t.Fatal(err)
	}
}

func TestJobSourceKindStampsMetadataPayload(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "machinist.db"))
	defer store.Close()
	issue, err := store.CreateBacklogIssue(t.Context(), "MACH", "Fix the sidebar", "Overlap.", "")
	if err != nil {
		t.Fatal(err)
	}
	jobID, err := store.CreateJob(t.Context(), "Overlap.", "machinist", "plan", testAgent("plan", "Overlap."))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.StampJobSource(t.Context(), jobID, "backlog", issue.ID); err != nil {
		t.Fatal(err)
	}
	if err = store.StampJobSource(t.Context(), jobID, "carrier", ""); err == nil {
		t.Fatal("unknown source kind accepted")
	}
	job := jobByID(t, store, jobID)
	if job.SourceKind != "backlog" || job.BacklogIssueID != issue.ID || job.BacklogIssueKey != "MACH-1" {
		t.Fatalf("job source identity = %q, %q, %q", job.SourceKind, job.BacklogIssueID, job.BacklogIssueKey)
	}
	// A legacy job without a source kind carries no identity.
	legacy, err := store.CreateJob(t.Context(), "request", "machinist", "plan", testAgent("plan", "Plan request"))
	if err != nil {
		t.Fatal(err)
	}
	if job = jobByID(t, store, legacy); job.SourceKind != "" || job.BacklogIssueID != "" {
		t.Fatalf("legacy job identity = %q, %q", job.SourceKind, job.BacklogIssueID)
	}
}

func TestOpenStoreMigratesVersionEightDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "machinist.db")
	store := openTestStore(t, path)
	if _, err := store.CreateJob(t.Context(), "request", "machinist", "plan", testAgent("plan", "Plan request")); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = legacy.Exec(`DROP TABLE backlog_issues; ALTER TABLE job_metadata DROP COLUMN source_kind; ALTER TABLE job_metadata DROP COLUMN backlog_issue_id; PRAGMA user_version=8;`); err != nil {
		t.Fatal(err)
	}
	if err = legacy.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := OpenStore(path)
	if err != nil {
		t.Fatalf("open v8 database: %v", err)
	}
	defer migrated.Close()
	var version int
	if err = migrated.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 9 {
		t.Fatalf("schema version = %d, %v, want 9", version, err)
	}
	var columns int
	if err = migrated.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('job_metadata') WHERE name IN ('source_kind','backlog_issue_id')`).Scan(&columns); err != nil || columns != 2 {
		t.Fatalf("source columns = %d, %v, want 2", columns, err)
	}
	if _, err = migrated.CreateBacklogIssue(t.Context(), "MACH", "Fix the sidebar", "Overlap.", ""); err != nil {
		t.Fatalf("create on migrated database: %v", err)
	}
}

func patchJSON(t *testing.T, endpoint string, body any, headers map[string]string) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPatch, endpoint, strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
