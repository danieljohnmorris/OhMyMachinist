package controlplane

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/owainlewis/machinist/internal/protocol"
)

func TestBuiltinTicketConcurrentApprovalAndCompletion(t *testing.T) {
	for _, doneFirst := range []bool{false, true} {
		name := "completion_before_done"
		if doneFirst {
			name = "done_before_completion"
		}
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t, filepath.Join(t.TempDir(), "db"))
			ticket, err := store.createTicket(t.Context(), Ticket{Title: "Preserve delivery links", Description: "Implement the requested change", Repository: "machinist"}, "MM")
			if err != nil {
				t.Fatal(err)
			}
			const requests = 8
			ids := make([]string, requests)
			created := make([]bool, requests)
			errs := make([]error, requests)
			var wg sync.WaitGroup
			for i := range requests {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ids[i], created[i], errs[i] = store.approveTicket(t.Context(), ticket.Key, testAgent("build", "{{machinist.prompt}}"))
				}()
			}
			wg.Wait()
			count := 0
			for i := range requests {
				if errs[i] != nil {
					t.Fatal(errs[i])
				}
				if ids[i] != ids[0] {
					t.Fatalf("approval duplicated jobs: %v", ids)
				}
				if created[i] {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("created %d jobs, want 1", count)
			}
			run, err := store.Poll(t.Context(), pollRequest("worker-a", []string{"codex"}, []string{"machinist"}))
			if err != nil {
				t.Fatal(err)
			}
			if run == nil {
				t.Fatal("approved ticket was not dispatched")
			}
			ticket, err = store.ticket(t.Context(), ticket.Key)
			if err != nil {
				t.Fatal(err)
			}
			if ticket.State != "in_progress" {
				t.Fatalf("dispatched state = %s", ticket.State)
			}
			if doneFirst {
				if _, err = store.updateTicket(t.Context(), ticket.Key, map[string]string{"state": "done"}); err != nil {
					t.Fatal(err)
				}
			}
			result := json.RawMessage(`{"metadata":{"pr_url":"https://github.com/example/repo/pull/42","tracker":"plane","ticket_key":"OTHER-1","issue_url":"https://other.example/ticket"}}`)
			completion := protocol.Completion{InstanceID: "worker-a", LeaseToken: run.LeaseToken, State: "succeeded", ExitCode: 0, Result: result}
			if err = store.Complete(t.Context(), run.ID, completion); err != nil {
				t.Fatal(err)
			}
			// Worker completion is idempotent: retries cannot post duplicate feedback.
			if err = store.Complete(t.Context(), run.ID, completion); err != nil {
				t.Fatal(err)
			}
			ticket, err = store.ticket(t.Context(), ticket.Key)
			if err != nil {
				t.Fatal(err)
			}
			if len(ticket.Comments) != 2 {
				t.Fatalf("comments = %#v", ticket.Comments)
			}
			if !doneFirst {
				if ticket.State != "in_review" {
					t.Fatalf("completed state = %s", ticket.State)
				}
				if _, err = store.updateTicket(t.Context(), ticket.Key, map[string]string{"state": "done"}); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := store.Snapshot(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			job := snapshot.Jobs[0]
			if job.State != "resolved" {
				t.Fatalf("job state = %s", job.State)
			}
			if job.Metadata == nil || job.Metadata.Tracker != "builtin" || job.Metadata.TicketKey != ticket.Key || job.Metadata.IssueURL != "#/tickets/"+ticket.Key || job.Metadata.PRURL != "https://github.com/example/repo/pull/42" {
				t.Fatalf("metadata identity or PR lost: %#v", job.Metadata)
			}
		})
	}
}

func TestBuiltinTicketApprovalValidationAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	store := openTestStore(t, path)
	ticket, err := store.createTicket(t.Context(), Ticket{Title: "Unassigned ticket"}, "MM")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = store.approveTicket(t.Context(), ticket.Key, testAgent("build", "{{machinist.prompt}}")); !errors.Is(err, errTicketInvalid) {
		t.Fatalf("unassigned approval: %v", err)
	}
	if _, err = store.updateTicket(t.Context(), ticket.Key, map[string]string{"state": "approved"}); !errors.Is(err, errTicketInvalid) {
		t.Fatalf("state bypass: %v", err)
	}
	if _, err = store.updateTicket(t.Context(), ticket.Key, map[string]string{"repository": "machinist", "state": "todo"}); err != nil {
		t.Fatal(err)
	}
	job, _, err := store.approveTicket(t.Context(), ticket.Key, testAgent("build", "{{machinist.prompt}}"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.updateTicket(t.Context(), ticket.Key, map[string]string{"repository": "another-repo"}); !errors.Is(err, errTicketConflict) {
		t.Fatalf("repository changed after dispatch: %v", err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, path)
	again, created, err := reopened.approveTicket(t.Context(), ticket.Key, testAgent("build", "{{machinist.prompt}}"))
	if err != nil {
		t.Fatal(err)
	}
	if again != job || created {
		t.Fatalf("restart duplicated approval: %s %v", again, created)
	}
}

func TestBuiltinTicketDeletionKeepsDeliveryHistory(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "db"))
	draft, err := store.createTicket(t.Context(), Ticket{Title: "Discard draft"}, "MM")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.deleteTicket(t.Context(), draft.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ticket(t.Context(), draft.Key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted ticket lookup: %v", err)
	}
	approved, err := store.createTicket(t.Context(), Ticket{Title: "Retain delivery history", Repository: "machinist"}, "MM")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.approveTicket(t.Context(), approved.Key, testAgent("build", "{{machinist.prompt}}")); err != nil {
		t.Fatal(err)
	}
	if err := store.deleteTicket(t.Context(), approved.Key); !errors.Is(err, errTicketConflict) {
		t.Fatalf("approved deletion: %v", err)
	}
	ticket, err := store.ticket(t.Context(), approved.Key)
	if err != nil || ticket.State != "approved" || ticket.JobID == "" {
		t.Fatalf("delivery history lost: %#v, %v", ticket, err)
	}
}

func TestDeleteWorkerSeparatesMissingFromActive(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "db"))
	if err := store.DeleteWorker(t.Context(), "nobody"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing worker error = %v, want sql.ErrNoRows", err)
	}
	if _, err := store.CreateJob(t.Context(), "request", "machinist", "plan", testAgent("plan", "Plan request")); err != nil {
		t.Fatal(err)
	}
	run, err := store.Poll(t.Context(), pollRequest("worker-busy", []string{"codex"}, []string{"machinist"}))
	if err != nil || run == nil {
		t.Fatalf("poll = %v, %v", run, err)
	}
	if err := store.DeleteWorker(t.Context(), "worker-busy"); !errors.Is(err, ErrWorkerActive) {
		t.Fatalf("running worker error = %v, want ErrWorkerActive", err)
	}
	if _, err := store.Poll(t.Context(), pollRequest("worker-idle", []string{"codex"}, []string{"machinist"})); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteWorker(t.Context(), "worker-idle"); err != nil {
		t.Fatalf("idle worker delete = %v", err)
	}
	if err := store.DeleteWorker(t.Context(), "worker-idle"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second delete error = %v, want sql.ErrNoRows", err)
	}
}
