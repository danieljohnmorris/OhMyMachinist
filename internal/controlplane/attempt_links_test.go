package controlplane

import "testing"

func TestLinkTicketAttemptsChainsJobsByKeyInCreationOrder(t *testing.T) {
	_, store := newResolutionTestServer(t)
	first := seedJob(t, store, "MACH-23: Dedupe", "", "failed")
	other := seedJob(t, store, "MACH-24: Other", "", "failed")
	second := seedJob(t, store, "MACH-23: Dedupe", "", "succeeded")
	third := seedJob(t, store, "MACH-23: Dedupe", "", "running")
	created := map[string]string{first: "2026-10-13T11:37:16Z", second: "2026-10-13T12:41:15Z", third: "2026-10-13T13:00:00Z", other: "2026-10-13T11:38:00Z"}
	for id, at := range created {
		// The oldest attempt was touched last (e.g. cancelled late); order must still follow creation.
		if _, err := store.db.Exec(`UPDATE jobs SET created_at=?, updated_at='2026-10-13T14:00:00Z' WHERE id=?`, at, id); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 { // idempotent
		if err := store.linkTicketAttempts(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	want := []struct {
		id, previous, next string
		attempt            int
	}{{first, "", second, 1}, {second, first, third, 2}, {third, second, "", 3}, {other, "", "", 1}}
	for _, expected := range want {
		job := jobByID(t, store, expected.id)
		if job.PreviousJobID != expected.previous || job.SupersededBy != expected.next || job.Attempt != expected.attempt {
			t.Fatalf("job %s = prev %q next %q attempt %d, want %+v", expected.id, job.PreviousJobID, job.SupersededBy, job.Attempt, expected)
		}
		if job.IssueKey == "" {
			t.Fatalf("job %s issue key not backfilled", expected.id)
		}
	}
}
