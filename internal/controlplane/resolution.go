package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/issues"
)

// resolutionPullRequestLimit bounds the merged-PR window scanned per job on
// each reconcile pass.
const resolutionPullRequestLimit = 50

// ticketKeyPattern matches ticket identifiers such as MACH-5 or SPARK-10.
var ticketKeyPattern = regexp.MustCompile(`\b([A-Z][A-Z0-9]{1,9}-\d+)\b`)

// ticketKey returns the ticket identifier a job belongs to, preferring the
// stored issue key and falling back to the trigger subject and title. Jobs
// created before resolution tracking carry no stored key, so the fallbacks
// keep the first-run backfill working.
func ticketKey(job Job) string {
	for _, candidate := range []string{job.IssueKey, job.TriggerSubject, job.Title, job.GitHubIssueTitle} {
		if match := ticketKeyPattern.FindString(strings.TrimSpace(candidate)); match != "" {
			return match
		}
	}
	return ""
}

// ticketKeyPatternFor builds a boundary-aware matcher so MACH-5 does not
// match MACH-51.
func ticketKeyPatternFor(key string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(key) + `\b`)
}

// pullRequestMatchesTicket reports whether a merged pull request is linked to
// the ticket: the key appears in the title or branch name, or the body links
// to the Plane issue.
func pullRequestMatchesTicket(pullRequest GitHubPullRequest, key, planeIssueURL string) bool {
	pattern := ticketKeyPatternFor(key)
	if pattern.MatchString(pullRequest.Title) || pattern.MatchString(pullRequest.HeadRefName) {
		return true
	}
	return containsIssueLink(pullRequest.Body, planeIssueURL)
}

// containsIssueLink reports whether body links to the issue URL, tolerating
// trailing paths and fragments but never a longer issue key.
func containsIssueLink(body, issueURL string) bool {
	issueURL = strings.TrimRight(issueURL, "/")
	if issueURL == "" {
		return false
	}
	for index := 0; index < len(body); {
		found := strings.Index(body[index:], issueURL)
		if found < 0 {
			return false
		}
		index += found
		end := index + len(issueURL)
		if end >= len(body) || strings.ContainsRune("/#?)> \t\r\n", rune(body[end])) {
			return true
		}
		index++
	}
	return false
}

// resolutionComment is posted to the ticket when a delivered PR merges.
func resolutionComment(pullRequest GitHubPullRequest) string {
	return fmt.Sprintf(`<p>Resolved by machinist: <a href="%s">PR #%d</a> merged by @%s.</p>`,
		pullRequest.URL, pullRequest.Number, pullRequest.MergedBy.Login)
}

// resolutionDetail describes the resolution shown on the card and run page.
func resolutionDetail(pullRequest GitHubPullRequest) string {
	return fmt.Sprintf("PR #%d (merged by %s)", pullRequest.Number, pullRequest.MergedBy.Login)
}

// planeClientFor returns the Plane client for a job's issue trigger, falling
// back to the only configured client so single-project factories reconcile
// jobs submitted without a trigger.
func (s *Server) planeClientFor(job Job) *issues.PlaneClient {
	if client, ok := s.planeClients[job.TriggerID]; ok {
		return client
	}
	if len(s.planeClients) == 1 {
		for _, client := range s.planeClients {
			return client
		}
	}
	return nil
}

// mergedTicketPullRequest returns the most recent merged pull request linked
// to the ticket, searching the job's GitHub repository. Repositories without
// a configured GitHub slug have no searchable PRs. Listings are cached per
// repository for one reconcile pass.
func (s *Server) mergedTicketPullRequest(ctx context.Context, job Job, key string, client *issues.PlaneClient, cache map[string][]GitHubPullRequest) (GitHubPullRequest, bool, error) {
	slug := s.githubRepositories[job.Repository]
	if slug == "" {
		return GitHubPullRequest{}, false, nil
	}
	pullRequests, ok := cache[slug]
	if !ok {
		var err error
		pullRequests, err = s.github.MergedPullRequests(ctx, slug, resolutionPullRequestLimit)
		if err != nil {
			return GitHubPullRequest{}, false, err
		}
		cache[slug] = pullRequests
	}
	planeIssueURL := ""
	if client != nil {
		planeIssueURL = client.BrowseURL(key)
	}
	for _, pullRequest := range pullRequests {
		if pullRequestMatchesTicket(pullRequest, key, planeIssueURL) {
			return pullRequest, true, nil
		}
	}
	return GitHubPullRequest{}, false, nil
}

// resolveTicket moves every finished job of one ticket to resolved when the
// ticket is done. A merged pull request also moves the Plane ticket to Done
// and posts one comment, but only when the ticket is not already completed,
// so backfills and repeat passes do not spam the ticket. Plane is touched
// before jobs are marked, so a failed attempt retries on the next pass.
func (s *Server) resolveTicket(ctx context.Context, jobs []Job, key string, cache map[string][]GitHubPullRequest) error {
	client := s.planeClientFor(jobs[0])
	var merged GitHubPullRequest
	found := false
	for _, job := range jobs {
		pullRequest, ok, err := s.mergedTicketPullRequest(ctx, job, key, client, cache)
		if err != nil {
			return fmt.Errorf("search merged pull requests for %s: %w", key, err)
		}
		if ok {
			merged, found = pullRequest, true
			break
		}
	}
	var state issues.PlaneIssueState
	if client != nil {
		var err error
		if state, err = client.IssueState(ctx, key); err != nil {
			return fmt.Errorf("read ticket %s state: %w", key, err)
		}
	}
	kind, detail := "", ""
	switch {
	case found:
		if client != nil && !issues.IsCompletedPlaneState(state) {
			if err := client.SetState(ctx, key, issues.CompletedPlaneState); err != nil {
				return fmt.Errorf("move ticket %s to %s: %w", key, issues.CompletedPlaneState, err)
			}
			if err := client.PostComment(ctx, key, resolutionComment(merged)); err != nil {
				return fmt.Errorf("comment on ticket %s: %w", key, err)
			}
		}
		kind, detail = "pr", resolutionDetail(merged)
	case client != nil && issues.IsCompletedPlaneState(state):
		kind, detail = "plane", "Plane "+firstNonEmptyString(state.Name, issues.CompletedPlaneState)
	default:
		return nil
	}
	var failures []error
	for _, job := range jobs {
		failures = append(failures, s.store.MarkJobResolved(ctx, job.ID, kind, detail, s.now().UTC()))
	}
	return errors.Join(failures...)
}

// reconcileTicketOutcomes resolves finished jobs whose tickets completed
// outside the factory. It runs once at startup to backfill existing cards and
// then roughly every ten minutes. Jobs are grouped by ticket so each ticket
// costs one lookup, and lookups are paced to stay under Plane's rate limit.
func (s *Server) reconcileTicketOutcomes(ctx context.Context) error {
	if err := s.store.linkTicketAttempts(ctx); err != nil {
		log.Printf("link ticket attempts: %v", err)
	}
	jobs, err := s.store.listJobs(ctx, "")
	if err != nil {
		return fmt.Errorf("list jobs for resolution: %w", err)
	}
	var keys []string
	byKey := map[string][]Job{}
	for _, job := range jobs {
		if !terminalJobState(job.State) || job.State == "resolved" {
			continue
		}
		key := ticketKey(job)
		if key == "" {
			continue
		}
		if _, seen := byKey[key]; !seen {
			keys = append(keys, key)
		}
		byKey[key] = append(byKey[key], job)
	}
	cache := map[string][]GitHubPullRequest{}
	var failures []error
	for index, key := range keys {
		if index > 0 && s.planePause > 0 {
			select {
			case <-ctx.Done():
				return errors.Join(append(failures, ctx.Err())...)
			case <-time.After(s.planePause):
			}
		}
		failures = append(failures, s.resolveTicket(ctx, byKey[key], key, cache))
	}
	return errors.Join(failures...)
}

// issueWebhook accepts Plane issue events. When a ticket moves into a
// completed state, every finished job tied to the ticket is resolved. The
// request must carry a valid HMAC signature.
func (s *Server) issueWebhook(response http.ResponseWriter, request *http.Request) {
	if s.webhookSecret == "" {
		writeError(response, http.StatusServiceUnavailable, errors.New("issue webhook secret is not configured"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(response, request.Body, maxRequestBytes))
	if err != nil {
		writeError(response, http.StatusBadRequest, errors.New("read issue webhook body"))
		return
	}
	if !issues.VerifyHMAC(s.webhookSecret, body, request.Header.Get("X-Signature")) {
		writeUnauthorized(response)
		return
	}
	issue, state, err := issues.ParseIssueWebhookState(body)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if !issues.IsCompletedPlaneState(state) {
		response.WriteHeader(http.StatusAccepted)
		return
	}
	name := firstNonEmptyString(state.Name, issues.CompletedPlaneState)
	resolved, err := s.store.ResolveTicketsForIssue(request.Context(), issue.Key, "plane", "Plane "+name, s.now().UTC())
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if resolved > 0 {
		log.Printf("issue webhook: ticket %s completed, resolved %d job(s)", issue.Key, resolved)
	}
	response.WriteHeader(http.StatusAccepted)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
