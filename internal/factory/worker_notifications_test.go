package factory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestReviewFeedbackReachesBusyForemanAtTurnBoundary(t *testing.T) {
	s, _ := fixture(t)
	s.mu.Lock()
	task, e := s.create("project", "Design", "Review this change", "")
	if e != nil {
		s.mu.Unlock()
		t.Fatal(e)
	}
	task.Design = "Initial design"
	s.advance(task)
	_ = s.saveTask(task)
	s.mu.Unlock()
	release := make(chan struct{})
	prompts := make(chan string, 3)
	s.SetRunner(func(ctx context.Context, r RunRequest, emit func(Event), permission func(context.Context, string) (bool, error)) (string, error) {
		prompts <- r.Prompt
		if r.Prompt == "Discuss current work" {
			select {
			case <-release:
			case <-ctx.Done():
				return "foreman", ctx.Err()
			}
		}
		return "foreman", nil
	})
	w := call(s, "POST", "projects/project/messages", `{"request_id":"discussion","message":"Discuss current work"}`, "")
	if w.Code != 202 {
		t.Fatal(w.Body.String())
	}
	select {
	case <-prompts:
	case <-time.After(time.Second):
		t.Fatal("foreman did not start")
	}
	w = call(s, "POST", "tasks/"+task.ID+"/changes", `{"version":1,"message":"Make the design smaller"}`, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	s.mu.Lock()
	foreman, _ := s.foreman("project")
	queued := append([]string(nil), foreman.ReportQueue...)
	s.mu.Unlock()
	if len(queued) != 1 || !strings.Contains(queued[0], "Make the design smaller") {
		t.Fatal("feedback lost while foreman was busy")
	}
	close(release)
	select {
	case prompt := <-prompts:
		if strings.Count(prompt, "Make the design smaller") != 1 {
			t.Fatal("feedback omitted or duplicated")
		}
	case <-time.After(time.Second):
		t.Fatal("busy foreman did not receive queued feedback")
	}
	idle(t, s)
}

func TestWorkerExitNotifiesForemanWithoutRepeatingAcceptedReport(t *testing.T) {
	for _, outcome := range []string{"missing", "error", "complete", "blocked", "blocked-error", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			s, _ := fixture(t)
			s.mu.Lock()
			task, e := s.create("project", "Worker result", "Report the result", "")
			s.mu.Unlock()
			if e != nil {
				t.Fatal(e)
			}
			foremanPrompts := make(chan string, 3)
			workerRuns := make(chan struct{}, 3)
			s.SetRunner(func(ctx context.Context, r RunRequest, emit func(Event), permission func(context.Context, string) (bool, error)) (string, error) {
				if strings.HasPrefix(r.SystemPrompt, "Foreman") {
					foremanPrompts <- r.Prompt
					return "foreman", nil
				}
				workerRuns <- struct{}{}
				if outcome == "cancelled" {
					w := call(s, "POST", "tasks/"+task.ID+"/cancel", `{}`, "")
					if w.Code != 200 {
						t.Error(w.Body.String())
					}
					return "worker", ctx.Err()
				}
				if outcome == "complete" || strings.HasPrefix(outcome, "blocked") {
					result := "blocked"
					if outcome == "complete" {
						result = "complete"
					}
					w := call(s, "POST", "tools/report", `{"task_id":"`+task.ID+`","report_id":"result","summary":"Accepted result","outcome":"`+result+`","design":"Design"}`, r.Token)
					if w.Code != 200 {
						t.Error(w.Body.String())
					}
				}
				if outcome == "error" || outcome == "blocked-error" {
					return "worker", errors.New("provider disconnected")
				}
				return "worker", nil
			})
			s.mu.Lock()
			worker, e := s.start(task)
			s.mu.Unlock()
			if e != nil {
				t.Fatal(e)
			}
			idle(t, s)
			if len(workerRuns) != 1 {
				t.Fatal("worker automatically retried")
			}
			if outcome == "cancelled" {
				if len(foremanPrompts) != 0 {
					t.Fatal("cancelled task notified foreman")
				}
				return
			}
			if len(foremanPrompts) != 1 {
				t.Fatalf("foreman received %d notifications", len(foremanPrompts))
			}
			prompt := <-foremanPrompts
			s.mu.Lock()
			status, activity, step, workerStatus := task.Status, task.Activity, task.Step, worker.Status
			s.mu.Unlock()
			if outcome == "missing" || outcome == "error" {
				if status != "interrupted" || !strings.Contains(prompt, "needs human attention") || !strings.Contains(prompt, "Do not automatically retry") {
					t.Fatalf("unreported exit not surfaced: status=%s prompt=%s", status, prompt)
				}
				if outcome == "missing" && workerStatus != "interrupted" {
					t.Fatal("clean exit mistaken for successful report")
				}
			} else {
				if strings.Contains(prompt, "needs human attention") {
					t.Fatal("accepted report was notified again as missing")
				}
				if outcome == "complete" && (step != 1 || status != "awaiting_approval") {
					t.Fatal("accepted completion changed")
				}
				if strings.HasPrefix(outcome, "blocked") && (step != 0 || !strings.HasPrefix(activity, "Blocked:")) {
					t.Fatal("accepted blocked report changed")
				}
			}
		})
	}
}

func TestBlockedWorkerFollowupWithoutReportNeedsExplicitRecovery(t *testing.T) {
	s, _ := fixture(t)
	s.mu.Lock()
	task, e := s.create("project", "Blocked worker", "Inspect blockage", "")
	s.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	workerRuns := make(chan struct{}, 3)
	notifications := make(chan string, 3)
	s.SetRunner(func(ctx context.Context, r RunRequest, emit func(Event), permission func(context.Context, string) (bool, error)) (string, error) {
		if strings.HasPrefix(r.SystemPrompt, "Foreman") {
			notifications <- r.Prompt
			if strings.Contains(r.Prompt, "(blocked)") {
				w := call(s, "POST", "tools/send_message", `{"request_id":"followup","task_id":"`+task.ID+`","message":"Look again"}`, r.Token)
				if w.Code != 200 {
					t.Error(w.Body.String())
				}
			}
			return "foreman", nil
		}
		workerRuns <- struct{}{}
		if !strings.Contains(r.Prompt, "Look again") {
			w := call(s, "POST", "tools/report", `{"task_id":"`+task.ID+`","report_id":"blocked","summary":"Need clarification","outcome":"blocked"}`, r.Token)
			if w.Code != 200 {
				t.Error(w.Body.String())
			}
		}
		return "worker", nil
	})
	s.mu.Lock()
	worker, e := s.start(task)
	s.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	idle(t, s)
	if len(workerRuns) != 2 || len(notifications) != 2 {
		t.Fatalf("unexpected turn counts: workers=%d notices=%d", len(workerRuns), len(notifications))
	}
	first, second := <-notifications, <-notifications
	if !strings.Contains(first, "(blocked)") || !strings.Contains(second, "needs human attention") {
		t.Fatal("unreported followup hidden by stale blocked activity")
	}
	s.mu.Lock()
	taskStatus, workerStatus := task.Status, worker.Status
	s.mu.Unlock()
	if taskStatus != "interrupted" || workerStatus != "interrupted" {
		t.Fatal("unreported followup can repeat without human recovery")
	}
}
