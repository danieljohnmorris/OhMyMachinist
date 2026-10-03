package factory

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/owainlewis/machinist/internal/config"
	_ "modernc.org/sqlite"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Service, *sql.DB) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for _, args := range [][]string{{"init", root}, {"-C", root, "config", "user.name", "Fixture"}, {"-C", root, "config", "user.email", "fixture@example.com"}} {
		if b, e := exec.Command("git", args...).CombinedOutput(); e != nil {
			t.Fatalf("git %s %v", b, e)
		}
	}
	if e := os.WriteFile(filepath.Join(root, "file.txt"), []byte("initial\n"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{{"-C", root, "add", "."}, {"-C", root, "commit", "-m", "initial"}} {
		if b, e := exec.Command("git", args...).CombinedOutput(); e != nil {
			t.Fatalf("git %s %v", b, e)
		}
	}
	db, e := sql.Open("sqlite", filepath.Join(t.TempDir(), "factory.db"))
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	cfg := config.ResolvedFactory{Enabled: true, Foreman: "foreman", DefaultPipeline: "default", Projects: map[string]config.FactoryProject{"project": {Name: "Project", Path: root, GitHub: "example/project", Host: "local"}}, Agents: map[string]config.ResolvedAgent{"foreman": {Runtime: "claude", Prompt: "Foreman"}, "planner": {Runtime: "claude", Prompt: "original planning instructions"}, "builder": {Runtime: "claude", Prompt: "Builder"}}, Pipelines: map[string]config.FactoryPipeline{"default": {Steps: []config.FactoryStep{{ID: "plan", Type: "agent", Agent: "planner", Stage: "design"}, {ID: "design", Type: "approval", Subject: "design", Stage: "design"}, {ID: "build", Type: "agent", Agent: "builder", Stage: "build"}, {ID: "checks", Type: "script", Command: []string{"git", "status", "--porcelain"}, Timeout: "1m", Stage: "build"}, {ID: "review", Type: "agent", Agent: "reviewer", Stage: "review"}, {ID: "approve", Type: "approval", Subject: "code", Stage: "review"}}}}}
	s, e := New(db, cfg, "machinist")
	if e != nil {
		t.Fatal(e)
	}
	s.SetRunner(func(ctx context.Context, r RunRequest, emit func(Event), permission func(context.Context, string) (bool, error)) (string, error) {
		emit(Event{Kind: "message_delta", Text: "saved response"})
		return "provider-1", nil
	})
	t.Cleanup(func() { s.Close(); time.Sleep(10 * time.Millisecond); db.Close() })
	return s, db
}
func call(s *Service, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/api/factory/"+path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func idle(t *testing.T, s *Service) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		done := s.active == "" && len(s.queue) == 0
		s.mu.Unlock()
		if done {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("agent did not stop")
}
func TestConversationsPersistAndMessageRetryDoesNotReplay(t *testing.T) {
	s, db := fixture(t)
	w := call(s, "POST", "projects/project/messages", `{"request_id":"one","message":"Hello"}`, "")
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	idle(t, s)
	w = call(s, "POST", "projects/project/messages", `{"request_id":"one","message":"Hello"}`, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	idle(t, s)
	s.mu.Lock()
	v, _ := s.foreman("project")
	events, _ := s.events(v.ID, 0)
	s.mu.Unlock()
	if len(events) != 3 {
		t.Fatalf("duplicated turn: %#v", events)
	}
	s.Close()
	restored, e := New(db, s.cfg, "machinist")
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	if len(restored.sessions) != 1 || restored.active != "" || len(restored.queue) != 0 {
		t.Fatal("restart replayed a conversation")
	}
	if restored.sessions[v.ID].ProviderID != "provider-1" {
		t.Fatal("lost provider conversation")
	}
}
func TestTaskSnapshotsWorkspaceAndApprovalOrder(t *testing.T) {
	s, _ := fixture(t)
	s.mu.Lock()
	first, e := s.create("project", "One", "Brief", "")
	if e != nil {
		t.Fatal(e)
	}
	second, e := s.create("project", "Two", "Brief", "")
	if e != nil {
		t.Fatal(e)
	}
	if first.Directory == second.Directory || first.Branch == second.Branch {
		t.Fatal("tasks share workspace")
	}
	s.cfg.Agents["planner"] = config.ResolvedAgent{Prompt: "changed"}
	if first.Agents["planner"].Prompt != "original planning instructions" {
		t.Fatal("live snapshot changed")
	}
	first.Design = "approved design"
	first.Version++
	s.advance(first)
	if _, e = s.start(first); e == nil {
		t.Fatal("agent skipped design approval")
	}
	if e = s.approve(first, 1, "design"); e == nil {
		t.Fatal("stale design approved")
	}
	if e = s.approve(first, first.Version, "code"); e == nil {
		t.Fatal("wrong subject approved")
	}
	if e = s.approve(first, first.Version, "design"); e != nil {
		t.Fatal(e)
	}
	if first.Step != 2 {
		t.Fatal("approval did not advance")
	}
	s.mu.Unlock()
	idle(t, s)
}
func TestCodeApprovalRequiresExactCleanRevisionAndChecks(t *testing.T) {
	s, _ := fixture(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	task, e := s.create("project", "One", "Brief", "")
	if e != nil {
		t.Fatal(e)
	}
	task.Step = 5
	task.Status = "awaiting_approval"
	task.ApprovalSubject = "code"
	task.Revision = task.BaseRevision
	if e = s.approve(task, task.Version, "code"); e == nil {
		t.Fatal("code approved without checks")
	}
	task.Checks = []Check{{Passed: true, Revision: task.Revision}}
	if e = os.WriteFile(filepath.Join(task.Directory, "file.txt"), []byte("dirty"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = s.approve(task, task.Version, "code"); e == nil {
		t.Fatal("dirty workspace approved")
	}
	if _, e = s.git(task.ProjectID, task.Directory, "checkout", "--", "file.txt"); e != nil {
		t.Fatal(e)
	}
	if e = s.approve(task, task.Version, "code"); e != nil {
		t.Fatal(e)
	}
	if task.Stage == "Done" || task.Status == "done" {
		t.Fatal("approval falsely meant merged")
	}
}
func TestScopedToolsCannotApproveOrOperateAnotherTask(t *testing.T) {
	s, _ := fixture(t)
	s.mu.Lock()
	v, _ := s.foreman("project")
	s.active = v.ID
	s.tokens["secret"] = v.ID
	s.mu.Unlock()
	w := call(s, "POST", "tools/create_task", `{"title":"One","brief":"Brief"}`, "secret")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var response struct{ Task Task }
	if e := json.Unmarshal(w.Body.Bytes(), &response); e != nil {
		t.Fatal(e)
	}
	w = call(s, "POST", "tools/approve", `{}`, "secret")
	if w.Code != http.StatusNotFound {
		t.Fatal("tool grants approval")
	}
	w = call(s, "POST", "tools/report", `{"task_id":"other","report_id":"r","summary":"done","outcome":"complete"}`, "secret")
	if w.Code != 403 {
		t.Fatal("foreman can fake worker result")
	}
	public := call(s, "GET", "status", "", "").Body.String()
	if strings.Contains(public, "secret") || strings.Contains(public, s.cfg.Projects["project"].Path) {
		t.Fatal("public response leaks credentials or paths")
	}
	s.mu.Lock()
	s.active = ""
	s.mu.Unlock()
}
func TestInterruptedTurnsAreNotRequeued(t *testing.T) {
	s, db := fixture(t)
	s.mu.Lock()
	v, _ := s.foreman("project")
	v.Status = "running"
	v.Pending = "Do work"
	v.ProviderID = "provider"
	if e := s.saveSession(v); e != nil {
		t.Fatal(e)
	}
	s.mu.Unlock()
	restored, e := New(db, s.cfg, "machinist")
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	if restored.sessions[v.ID].Status != "interrupted" || len(restored.queue) > 0 {
		t.Fatal("interrupted run replayed")
	}
}
func TestPRIdentityAndRepairLimit(t *testing.T) {
	s, _ := fixture(t)
	for _, raw := range []string{"https://github.com/other/project/pull/1", "https://github.com/example/project/pull/1?x=y", "http://github.com/example/project/pull/1", "https://github.com/example/project/pull/0"} {
		if validPR(raw, "example/project") {
			t.Fatal(raw)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	task, e := s.create("project", "One", "Brief", "")
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		if e = s.repair(task, "Fix it"); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = s.start(task); e == nil {
		t.Fatal("unbounded repair loop")
	}
}

func TestRecoveryCannotBeBypassedWithMessagesOrTools(t *testing.T) {
	s, _ := fixture(t)
	s.mu.Lock()
	v, _ := s.foreman("project")
	v.Status = "interrupted"
	_ = s.saveSession(v)
	task, e := s.create("project", "One", "Brief", "")
	if e != nil {
		t.Fatal(e)
	}
	task.Status = "interrupted"
	if _, e = s.start(task); e == nil {
		t.Fatal("foreman bypassed recovery")
	}
	s.mu.Unlock()
	w := call(s, "POST", "projects/project/messages", `{"request_id":"retry","message":"continue"}`, "")
	if w.Code != 409 {
		t.Fatal("message bypassed interrupted confirmation")
	}
	w = call(s, "POST", "sessions/"+v.ID+"/resume", `{"confirmed_stopped":false}`, "")
	if w.Code != 409 {
		t.Fatal("resume without confirmation")
	}
	w = call(s, "POST", "sessions/"+v.ID+"/resume", `{"confirmed_stopped":true}`, "")
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	idle(t, s)
}
func TestBusyTaskCannotChangeUnderItsWorker(t *testing.T) {
	s, _ := fixture(t)
	s.mu.Lock()
	task, e := s.create("project", "One", "Brief", "")
	if e != nil {
		t.Fatal(e)
	}
	v := &Session{ID: "worker", TaskID: task.ID, Step: 0, Status: "running"}
	s.sessions[v.ID] = v
	if e = s.changes(task, task.Version, "Change it"); e == nil {
		t.Fatal("feedback changed task under active owner")
	}
	if task.Step != 0 || task.Version != 1 {
		t.Fatal("rejected feedback changed state")
	}
	s.mu.Unlock()
}
func TestWorkerReportsAdvanceOnceAndPersistBeforeForemanWake(t *testing.T) {
	s, _ := fixture(t)
	s.mu.Lock()
	task, e := s.create("project", "One", "Brief", "")
	if e != nil {
		t.Fatal(e)
	}
	worker := &Session{ID: "worker", ProjectID: task.ProjectID, TaskID: task.ID, Role: "planner", Status: "running", Step: 0, Directory: task.Directory}
	s.sessions[worker.ID] = worker
	s.active = worker.ID
	s.tokens["report-token"] = worker.ID
	s.mu.Unlock()
	body := `{"task_id":"` + task.ID + `","report_id":"one","summary":"Design ready","outcome":"complete","design":"Design and acceptance criteria"}`
	for i := 0; i < 2; i++ {
		w := call(s, "POST", "tools/report", body, "report-token")
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	s.mu.Lock()
	if task.Step != 1 || task.Status != "awaiting_approval" || task.Version != 2 {
		t.Fatal("duplicate report advanced pipeline")
	}
	foreman, _ := s.foreman(task.ProjectID)
	if foreman.Status != "queued" || len(s.queue) != 1 {
		t.Fatal("report missing or double wake")
	}
	worker.Status = "completed"
	s.active = ""
	s.next()
	s.mu.Unlock()
	idle(t, s)
}
func TestProviderIDPersistsDuringTurn(t *testing.T) {
	s, _ := fixture(t)
	seen := make(chan struct{})
	release := make(chan struct{})
	s.SetRunner(func(ctx context.Context, r RunRequest, emit func(Event), permission func(context.Context, string) (bool, error)) (string, error) {
		emit(Event{Kind: "session", ProviderID: "early-provider"})
		close(seen)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return "early-provider", nil
	})
	w := call(s, "POST", "projects/project/messages", `{"request_id":"one","message":"work"}`, "")
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	<-seen
	s.mu.Lock()
	v, _ := s.foreman("project")
	if v.ProviderID != "early-provider" {
		t.Fatal("provider id not persisted until completion")
	}
	s.mu.Unlock()
	close(release)
	idle(t, s)
}

func TestSuccessfulCheckRetryReplacesFailedAttempt(t *testing.T) {
	s, _ := fixture(t)
	s.mu.Lock()
	task, e := s.create("project", "One", "Brief", "")
	if e != nil {
		t.Fatal(e)
	}
	task.Step = 3
	task.Stage = "Build"
	task.Revision = task.BaseRevision
	task.Checks = []Check{{StepID: "checks", Name: "Previous attempt", Passed: false, Revision: task.Revision}}
	if _, e = s.start(task); e != nil {
		t.Fatal(e)
	}
	s.mu.Unlock()
	idle(t, s)
	s.mu.Lock()
	defer s.mu.Unlock()
	if task.Step != 4 || len(task.Checks) != 1 || !task.Checks[0].Passed {
		t.Fatalf("old failed check retained: %#v", task.Checks)
	}
}
func TestGitHubMergeRequiresApprovedCurrentRevision(t *testing.T) {
	s, _ := fixture(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	task, e := s.create("project", "One", "Brief", "")
	if e != nil {
		t.Fatal(e)
	}
	task.Revision = task.BaseRevision
	task.Step = len(task.Steps)
	merged := now()
	pr := githubPR{State: "MERGED", MergedAt: &merged, HeadRefOID: task.Revision}
	if e = s.applyPR(task, pr); e != nil {
		t.Fatal(e)
	}
	if task.Status == "done" {
		t.Fatal("unapproved merge counted done")
	}
	task.CodeApproved = task.Revision
	pr.StatusCheckRollup = append(pr.StatusCheckRollup, struct{ Status, Conclusion, State string }{Status: "COMPLETED", Conclusion: "FAILURE"})
	if e = s.applyPR(task, pr); e != nil {
		t.Fatal(e)
	}
	if task.Status == "done" {
		t.Fatal("failed checks counted done")
	}
	pr.StatusCheckRollup = nil
	if e = s.applyPR(task, pr); e != nil {
		t.Fatal(e)
	}
	if task.Status != "done" || task.Stage != "Done" {
		t.Fatal("verified approved merge not done")
	}
}
func TestConfigChangeCannotRetargetAnExistingTask(t *testing.T) {
	s, _ := fixture(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	task, e := s.create("project", "One", "Brief", "")
	if e != nil {
		t.Fatal(e)
	}
	p := s.cfg.Projects["project"]
	p.Path = "/another/repository"
	s.cfg.Projects["project"] = p
	if _, e = s.start(task); e == nil {
		t.Fatal("existing work retargeted to new path")
	}
}

func TestOnlyKnownFactoryToolsSkipProviderPermission(t *testing.T) {
	for _, name := range []string{"mcp__machinist__inspect_tasks", "mcp__machinist__create_task", "mcp__machinist__report"} {
		if !safeFactoryTool(name) {
			t.Fatal(name)
		}
	}
	for _, name := range []string{"Bash", "Write file", "mcp__machinist__approve", "mcp__machinist__merge", "mcp__machinist__report ; rm -rf /"} {
		if safeFactoryTool(name) {
			t.Fatal("unexpected permission bypass", name)
		}
	}
}
func TestHumanCanContinueAtRepairLimit(t *testing.T) {
	s, _ := fixture(t)
	s.mu.Lock()
	task, e := s.create("project", "One", "Brief", "")
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		_ = s.repair(task, "Fix it")
	}
	version := task.Version
	directory := task.Directory
	s.mu.Unlock()
	w := call(s, "POST", "tasks/"+task.ID+"/continue", `{"version":1}`, "")
	if w.Code != 409 {
		t.Fatal("stale human continuation accepted")
	}
	body := `{"version":` + strconv.Itoa(version) + `}`
	w = call(s, "POST", "tasks/"+task.ID+"/continue", body, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	idle(t, s)
	s.mu.Lock()
	defer s.mu.Unlock()
	if task.Repairs != 0 || task.Directory != directory || task.Status != "active" {
		t.Fatal("human continuation lost task workspace or failed to reset budget")
	}
}

func TestPublishingRequiresCleanHumanApprovedRevision(t *testing.T) {
	s, _ := fixture(t)
	s.mu.Lock()
	defer s.mu.Unlock()
	task, e := s.create("project", "One", "Brief", "")
	if e != nil {
		t.Fatal(e)
	}
	task.Revision = task.BaseRevision
	task.Step = len(task.Steps)
	if e = s.deliveryReady(task); e == nil {
		t.Fatal("unapproved publishing allowed")
	}
	task.CodeApproved = task.Revision
	if e = s.deliveryReady(task); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(task.Directory, "file.txt"), []byte("unapproved change"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = s.deliveryReady(task); e == nil {
		t.Fatal("dirty publishing allowed")
	}
}

func TestDeliveryWorkerLinksOnlyProjectApprovedHead(t *testing.T) {
	s, _ := fixture(t)
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	writeGH := func(head string) {
		t.Helper()
		script := "#!/bin/sh\nprintf '%s' '{\"state\":\"OPEN\",\"headRefOid\":\"" + head + "\",\"mergedAt\":null,\"statusCheckRollup\":[]}'\n"
		if e := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0700); e != nil {
			t.Fatal(e)
		}
	}
	s.mu.Lock()
	task, e := s.create("project", "One", "Brief", "")
	if e != nil {
		t.Fatal(e)
	}
	task.Revision = task.BaseRevision
	task.CodeApproved = task.Revision
	task.Step = len(task.Steps)
	worker := &Session{ID: "delivery", ProjectID: task.ProjectID, TaskID: task.ID, Role: "builder", Step: 2, Status: "running", Delivery: true}
	s.sessions[worker.ID] = worker
	s.active = worker.ID
	s.tokens["delivery-token"] = worker.ID
	s.mu.Unlock()
	body := `{"task_id":"` + task.ID + `","pr_url":"https://github.com/example/project/pull/1"}`
	writeGH("unapproved-head")
	w := call(s, "POST", "tools/link_pr", body, "delivery-token")
	if w.Code != 409 {
		t.Fatal("wrong GitHub revision linked", w.Body.String())
	}
	writeGH(task.Revision)
	w = call(s, "POST", "tools/link_pr", body, "delivery-token")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	s.mu.Lock()
	if task.PRURL == "" || task.Status == "done" {
		t.Fatal("link did not preserve unmerged Review")
	}
	worker.Status = "completed"
	s.active = ""
	s.next()
	s.mu.Unlock()
	idle(t, s)
}

func TestRebuildClearsPreviousDeliveryMode(t *testing.T) {
	s, _ := fixture(t)
	received := make(chan RunRequest, 1)
	s.SetRunner(func(ctx context.Context, r RunRequest, emit func(Event), permission func(context.Context, string) (bool, error)) (string, error) {
		if strings.Contains(r.Prompt, "Current step") {
			received <- r
		}
		return "builder-provider", nil
	})
	s.mu.Lock()
	task, e := s.create("project", "One", "Brief", "")
	if e != nil {
		t.Fatal(e)
	}
	task.Revision = task.BaseRevision
	task.CodeApproved = task.Revision
	task.Step = len(task.Steps)
	worker := &Session{ID: "builder", ProjectID: task.ProjectID, TaskID: task.ID, Role: "builder", Status: "completed", Step: 2, Directory: task.Directory, ProviderID: "existing-builder", Delivery: true}
	s.sessions[worker.ID] = worker
	if e = s.repair(task, "Fix the review finding"); e != nil {
		t.Fatal(e)
	}
	if _, e = s.start(task); e != nil {
		t.Fatal(e)
	}
	s.mu.Unlock()
	select {
	case r := <-received:
		if strings.Contains(r.Prompt, "Publish only") || r.SessionID != "existing-builder" {
			t.Fatal("rebuild publishes or creates another conversation")
		}
	case <-time.After(time.Second):
		t.Fatal("no rebuild prompt")
	}
	idle(t, s)
	s.mu.Lock()
	defer s.mu.Unlock()
	if worker.Delivery {
		t.Fatal("delivery mode survived repair")
	}
}

func TestMachineContextIsNotLabeledAsUserMessage(t *testing.T) {
	s, _ := fixture(t)
	w := call(s, "POST", "projects/project/messages", `{"request_id":"human","message":"Build a feature"}`, "")
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	idle(t, s)
	s.mu.Lock()
	v, _ := s.foreman("project")
	s.notify("project", "Worker finished the design")
	s.mu.Unlock()
	idle(t, s)
	s.mu.Lock()
	defer s.mu.Unlock()
	events, e := s.events(v.ID, 0)
	if e != nil {
		t.Fatal(e)
	}
	user, contextCount := 0, 0
	for _, event := range events {
		if event.Kind == "user" {
			user++
			if event.Text != "Build a feature" {
				t.Fatal("machine report impersonates the user")
			}
		}
		if event.Kind == "context" {
			contextCount++
			if event.Title != "Task update" {
				t.Fatal("missing context label")
			}
		}
	}
	if user != 1 || contextCount != 1 {
		t.Fatalf("expected one user and one context, got %d/%d", user, contextCount)
	}
}
