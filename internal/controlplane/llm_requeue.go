package controlplane

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"
)

// Wait for endpoint health, then requeue once (MACH-69): a scheduled pass
// watches llm_unavailable jobs, probes the configured model endpoint on a
// backoff, and — only when the endpoint is healthy and serves the expected
// model — requeues the job once with plain submit semantics (no healing
// instruction: the task was never the problem). The usual dedupe applies,
// the wait is bounded by the configured maximum, and after the automatic
// attempt (or the wait) fails, the job is alerted and left for a human.

// llmAlert spooled to the alert directory for the external poller.
type llmAlert struct {
	JobID  string `json:"job_id"`
	Ticket string `json:"ticket,omitempty"`
	Class  string `json:"class"`
	State  string `json:"state"`
	Detail string `json:"detail"`
	At     string `json:"at"`
}

// healLLMUnavailablePass is the scheduler entry point. It runs at the probe
// interval so a recovered endpoint is noticed quickly while the endpoint is
// down probes stay cheap.
func (s *Server) healLLMUnavailablePass(ctx context.Context) error {
	if !s.llmUnavailable.IsEnabled() {
		return nil
	}
	jobs, err := s.store.listJobs(ctx, "")
	if err != nil {
		return fmt.Errorf("list jobs for llm_unavailable: %w", err)
	}
	for _, job := range jobs {
		if job.State != "failed" || job.Metadata == nil || job.Metadata.ErrorClass == nil {
			continue
		}
		record := *job.Metadata.ErrorClass
		if record.Class != ErrorClassLLMUnavailable || record.Resolved {
			continue
		}
		s.processLLMUnavailableJob(ctx, job, record)
	}
	return nil
}

// processLLMUnavailableJob applies the wait-and-requeue policy to one failed
// job. Every branch either requeues once or resolves the record so the pass
// never requeues twice.
func (s *Server) processLLMUnavailableJob(ctx context.Context, job Job, record ErrorClassRecord) {
	allowance := s.llmUnavailableAutoRequeue()
	if allowance < 0 || record.Requeues >= allowance {
		s.stopLLMUnavailable(ctx, job, record, fmt.Sprintf("automatic requeue allowance used (%d); leaving the job for a human", record.Requeues))
		return
	}
	maxWait, err := s.llmUnavailable.MaxWaitDuration()
	if err != nil {
		maxWait = 60 * time.Minute
	}
	if failedAt := parseRecordTime(record.At); failedAt.IsZero() || s.now().UTC().After(failedAt.Add(maxWait)) {
		s.stopLLMUnavailable(ctx, job, record, fmt.Sprintf("endpoint not healthy within max wait (%s)", maxWait))
		return
	}
	active, err := s.store.ticketActiveAttemptExists(ctx, job)
	if err != nil {
		log.Printf("llm_unavailable: dedupe check for job %q: %v", job.ID, err)
		return
	}
	if active {
		// The usual dedupe: someone already requeued the ticket, so adding
		// another attempt would duplicate it.
		s.resolveLLMUnavailable(ctx, job, record, "another attempt of the ticket is queued or running; not requeuing", false)
		return
	}
	probe := s.probeModelEndpoint(ctx)
	if !probe.Healthy {
		return // keep waiting; the pass runs again at the probe interval.
	}
	if err := s.store.requeueJobAsIs(ctx, job.ID); err != nil {
		log.Printf("llm_unavailable: requeue job %q: %v", job.ID, err)
		return
	}
	record.Requeues++
	record.Stopped = "requeued once after the endpoint became healthy"
	record.Resolved = true
	if err := s.store.SaveErrorClassMetadata(ctx, job.ID, record); err != nil {
		log.Printf("llm_unavailable: record requeue on job %q: %v", job.ID, err)
	}
	log.Printf("llm_unavailable: requeued job %q after the endpoint became healthy (%s)", job.ID, probe.Detail)
	s.spoolLLMAlert(ctx, job, "requeued", fmt.Sprintf("endpoint healthy again (%s); job requeued once automatically", probe.Detail))
}

// stopLLMUnavailable ends the wait: alert, mark resolved, leave for a human.
func (s *Server) stopLLMUnavailable(ctx context.Context, job Job, record ErrorClassRecord, reason string) {
	s.resolveLLMUnavailable(ctx, job, record, reason, true)
}

// resolveLLMUnavailable marks the record resolved and, when alerting, spools
// an alert for the external poller.
func (s *Server) resolveLLMUnavailable(ctx context.Context, job Job, record ErrorClassRecord, reason string, alert bool) {
	record.Stopped = reason
	record.Resolved = true
	if err := s.store.SaveErrorClassMetadata(ctx, job.ID, record); err != nil {
		log.Printf("llm_unavailable: record stop on job %q: %v", job.ID, err)
	}
	log.Printf("llm_unavailable: job %q: %s", job.ID, reason)
	if alert {
		s.spoolLLMAlert(ctx, job, "left_for_human", reason)
	}
}

// spoolLLMAlert writes one llm_unavailable alert; the spool overwrites per
// job so the poller always sees the latest state.
func (s *Server) spoolLLMAlert(ctx context.Context, job Job, state, detail string) {
	if err := s.failAlerts.writeNamed("llm-unavailable-"+job.ID+".json", llmAlert{
		JobID:  job.ID,
		Ticket: ticketKey(job),
		Class:  string(ErrorClassLLMUnavailable),
		State:  state,
		Detail: detail,
		At:     s.now().UTC().Format(time.RFC3339),
	}); err != nil {
		log.Printf("llm_unavailable: spool alert for job %q: %v", job.ID, err)
	}
}

// handleLLMUnavailable records the classification: provider, model, endpoint
// and time on the job metadata (shown on the board card, run page and failure
// alert), and a Plane comment saying the run failed because the LLM was
// unavailable. It consumes no retry budget, adds no failed label and triggers
// no review or fix-up flow; the scheduled pass owns the wait and the
// once-only requeue.
func (s *Server) handleLLMUnavailable(ctx context.Context, job Job, run Run, reason string) {
	record := ErrorClassRecord{
		Class:    ErrorClassLLMUnavailable,
		Reason:   reason,
		Provider: run.Executor,
		Model:    run.Model,
		Endpoint: s.llmUnavailable.Endpoint,
		At:       s.now().UTC().Format(time.RFC3339),
	}
	// A repeat llm_unavailable failure with no successful run in between is
	// the automatic attempt failing again: keep the requeue count so the
	// allowance is not reset by reclassification (MACH-69 "stop, alert and
	// leave the job for a human").
	if job.Metadata != nil && job.Metadata.ErrorClass != nil && job.Metadata.ErrorClass.Class == ErrorClassLLMUnavailable {
		existing := job.Metadata.ErrorClass
		if succeeded, err := s.store.jobHasSucceededRun(ctx, job.ID); err == nil && !succeeded {
			record.Requeues = existing.Requeues
		}
	}
	if err := s.store.SaveErrorClassMetadata(ctx, job.ID, record); err != nil {
		log.Printf("llm_unavailable: record classification on job %q: %v", job.ID, err)
	}
	client := s.planeClientFor(job)
	key := ticketKey(job)
	if client == nil || key == "" {
		return
	}
	var body strings.Builder
	fmt.Fprintf(&body, "<p><strong>Run failed — LLM unavailable</strong></p>")
	fmt.Fprintf(&body, "<p>Cause: <code>%s</code> — %s</p>", record.Class, reason)
	if record.Model != "" || record.Endpoint != "" {
		fmt.Fprintf(&body, "<p>Model <code>%s</code> on <code>%s</code>.</p>", record.Model, record.Endpoint)
	}
	if allowance := s.llmUnavailableAutoRequeue(); s.llmUnavailable.Endpoint == "" || allowance < 0 {
		body.WriteString("<p>No automatic requeue is configured; the job is left for a human.</p>")
	} else {
		maxWait := "60m"
		if parsed, err := s.llmUnavailable.MaxWaitDuration(); err == nil {
			maxWait = parsed.String()
		}
		fmt.Fprintf(&body, "<p>Machinist will wait for the endpoint to become healthy and requeue the job once automatically (max wait %s). No retry budget consumed, no failed label.</p>", maxWait)
	}
	if err := client.PostComment(ctx, key, body.String()); err != nil {
		log.Printf("llm_unavailable: comment on ticket %s: %v", key, err)
	}
}
