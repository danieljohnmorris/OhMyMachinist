package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/issues"
)

// resolutionPullRequestLimit bounds the merged-PR window scanned per job on
// each reconcile pass.
const resolutionPullRequestLimit = 50

// Trackers a factory job can belong to. Jobs created before ticket identity
// was stored carry no tracker; they are classified by their trigger.
const (
	trackerBuiltin = "builtin"
	trackerPlane   = "plane"
	trackerLinear  = "linear"
)

// ticketKeyPattern matches ticket identifiers such as M-1, MACH-5 or SPARK-10,
// matching config tracker.key_prefix (1-10 characters).
var ticketKeyPattern = regexp.MustCompile(`\b([A-Z][A-Z0-9]{0,9}-\d+)\b`)

// ticketKey returns the ticket identifier a job belongs to, preferring the
// stored ticket identity and falling back to the trigger subject and title.
// Jobs created before resolution tracking carry no stored key, so the
// fallbacks keep the first-run backfill working.
func ticketKey(job Job) string {
	candidates := []string{job.IssueKey, job.TriggerSubject, job.Title, job.GitHubIssueTitle}
	if job.Metadata != nil {
		candidates = append([]string{job.Metadata.TicketKey}, candidates...)
	}
	for _, candidate := range candidates {
		if match := ticketKeyPattern.FindString(strings.TrimSpace(candidate)); match != "" {
			return match
		}
	}
	return ""
}

// jobTracker reports which tracker owns a job's ticket: the stored tracker,
// else Linear for jobs admitted by a Linear trigger, else Plane, which is what
// every legacy ticket job came from.
func (s *Server) jobTracker(job Job) string {
	if job.Metadata != nil {
		if tracker := strings.ToLower(strings.TrimSpace(job.Metadata.Tracker)); tracker != "" {
			return tracker
		}
	}
	if _, ok := s.issueSources[job.TriggerID].(linearPoller); ok {
		return trackerLinear
	}
	return trackerPlane
}

// linearClientFor returns the Linear client of the trigger that admitted the
// job. Linear jobs always come from a trigger, so there is no fallback.
func (s *Server) linearClientFor(job Job) *issues.LinearClient {
	if poller, ok := s.issueSources[job.TriggerID].(linearPoller); ok {
		return poller.client
	}
	return nil
}

// ticketKeyPatternFor builds a boundary-aware matcher so MACH-5 does not
// match MACH-51.
func ticketKeyPatternFor(key string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(key) + `\b`)
}

// pullRequestMatchesTicket reports whether a merged pull request is linked to
// the ticket: the key appears in the title or branch name, or the body links
// to one of the ticket's tracker URLs.
func pullRequestMatchesTicket(pullRequest GitHubPullRequest, key string, issueURLs ...string) bool {
	pattern := ticketKeyPatternFor(key)
	if pattern.MatchString(pullRequest.Title) || pattern.MatchString(pullRequest.HeadRefName) {
		return true
	}
	for _, issueURL := range issueURLs {
		if containsIssueLink(pullRequest.Body, issueURL) {
			return true
		}
	}
	return false
}

// containsIssueLink reports whether body links to the issue URL, tolerating
// trailing paths and fragments but never a longer issue key. Only absolute
// HTTP(S) URLs count; builtin fragment links are too short to be evidence.
func containsIssueLink(body, issueURL string) bool {
	issueURL = strings.TrimRight(issueURL, "/")
	if !strings.HasPrefix(issueURL, "http://") && !strings.HasPrefix(issueURL, "https://") {
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

// resolutionComment is posted to a Plane ticket when a delivered PR merges.
func resolutionComment(pullRequest GitHubPullRequest) string {
	return fmt.Sprintf(`<p>Resolved by machinist: <a href="%s">PR #%d</a> merged by @%s.</p>`,
		pullRequest.URL, pullRequest.Number, pullRequest.MergedBy.Login)
}

// linearResolutionComment is the Markdown form posted to Linear tickets.
func linearResolutionComment(pullRequest GitHubPullRequest) string {
	return fmt.Sprintf("Resolved by machinist: [PR #%d](%s) merged by @%s.",
		pullRequest.Number, pullRequest.URL, pullRequest.MergedBy.Login)
}

// builtinResolutionComment is the plain-text form posted to builtin tickets.
func builtinResolutionComment(pullRequest GitHubPullRequest) string {
	return fmt.Sprintf("Resolved by merged pull request #%d (merged by @%s): %s",
		pullRequest.Number, pullRequest.MergedBy.Login, pullRequest.URL)
}

// resolutionDetail describes the resolution shown on the card and run page.
func resolutionDetail(pullRequest GitHubPullRequest) string {
	return fmt.Sprintf("PR #%d (merged by %s)", pullRequest.Number, pullRequest.MergedBy.Login)
}

// planeSource returns the Plane client for a job's issue trigger and the
// identity it is registered under, falling back to the only configured client
// so single-project factories reconcile jobs submitted without a trigger.
func (s *Server) planeSource(job Job) (string, *issues.PlaneClient) {
	if client, ok := s.planeClients[job.TriggerID]; ok {
		return job.TriggerID, client
	}
	if len(s.planeClients) == 1 {
		for identity, client := range s.planeClients {
			return identity, client
		}
	}
	return "", nil
}

// planeClientFor returns the Plane client for a Plane job. Builtin and
// Linear jobs never get one, so the single-client fallback cannot move or
// comment on a Plane ticket that happens to share their key.
func (s *Server) planeClientFor(job Job) *issues.PlaneClient {
	if s.jobTracker(job) != trackerPlane {
		return nil
	}
	_, client := s.planeSource(job)
	return client
}

// ticketGroup is every unresolved finished job of one ticket in one tracker
// source. Plane and Linear groups carry the client that owns the ticket.
type ticketGroup struct {
	tracker string
	source  string
	key     string
	plane   *issues.PlaneClient
	linear  *issues.LinearClient
	jobs    []Job
}

// ticketGroupFor classifies a job into its tracker source. Jobs without a
// ticket key, Linear jobs whose trigger is gone and unknown trackers have
// nothing to resolve against.
func (s *Server) ticketGroupFor(job Job) (ticketGroup, bool) {
	key := ticketKey(job)
	if key == "" {
		return ticketGroup{}, false
	}
	group := ticketGroup{tracker: s.jobTracker(job), key: key}
	switch group.tracker {
	case trackerBuiltin:
	case trackerLinear:
		group.source, group.linear = job.TriggerID, s.linearClientFor(job)
		if group.linear == nil {
			return ticketGroup{}, false
		}
	case trackerPlane:
		group.source, group.plane = s.planeSource(job)
	default:
		return ticketGroup{}, false
	}
	return group, true
}

// mergedTicketPullRequest returns the most recent merged pull request linked
// to the ticket, searching the job's GitHub repository. A PR recorded on the
// job itself always counts. Repositories without a configured GitHub slug
// have no searchable PRs. Listings are cached per repository for one pass.
func (s *Server) mergedTicketPullRequest(ctx context.Context, job Job, key string, issueURLs []string, cache map[string][]GitHubPullRequest) (GitHubPullRequest, bool, error) {
	slug := s.githubRepositories[job.Repository]
	if slug == "" || s.github == nil {
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
	deliveredURL := ""
	if job.Metadata != nil {
		deliveredURL = strings.TrimRight(job.Metadata.PRURL, "/")
		issueURLs = append(issueURLs, job.Metadata.IssueURL)
	}
	for _, pullRequest := range pullRequests {
		if (deliveredURL != "" && strings.TrimRight(pullRequest.URL, "/") == deliveredURL) || pullRequestMatchesTicket(pullRequest, key, issueURLs...) {
			return pullRequest, true, nil
		}
	}
	return GitHubPullRequest{}, false, nil
}

// groupPullRequest searches every job of the group for a merged linked PR.
func (s *Server) groupPullRequest(ctx context.Context, group ticketGroup, issueURLs []string, cache map[string][]GitHubPullRequest) (GitHubPullRequest, bool, error) {
	for _, job := range group.jobs {
		pullRequest, ok, err := s.mergedTicketPullRequest(ctx, job, group.key, issueURLs, cache)
		if err != nil {
			return GitHubPullRequest{}, false, fmt.Errorf("search merged pull requests for %s: %w", group.key, err)
		}
		if ok {
			return pullRequest, true, nil
		}
	}
	return GitHubPullRequest{}, false, nil
}

// markGroupResolved records one resolution on every job of the group.
func (s *Server) markGroupResolved(ctx context.Context, group ticketGroup, kind, detail string) error {
	var failures []error
	for _, job := range group.jobs {
		failures = append(failures, s.store.MarkJobResolved(ctx, job.ID, kind, detail, s.now().UTC()))
	}
	return errors.Join(failures...)
}

// resolvePlaneGroup resolves a Plane ticket's jobs when the ticket is done. A
// merged pull request also moves the ticket to Done and posts one comment,
// but only when the ticket is not already completed, so backfills and repeat
// passes do not spam the ticket. Plane is touched before jobs are marked, so
// a failed attempt retries on the next pass.
func (s *Server) resolvePlaneGroup(ctx context.Context, group ticketGroup, cache map[string][]GitHubPullRequest) error {
	client, key := group.plane, group.key
	var issueURLs []string
	if client != nil {
		issueURLs = append(issueURLs, client.BrowseURL(key))
	}
	merged, found, err := s.groupPullRequest(ctx, group, issueURLs, cache)
	if err != nil {
		return err
	}
	var state issues.PlaneIssueState
	if client != nil {
		if state, err = client.IssueState(ctx, key); err != nil {
			return fmt.Errorf("read ticket %s state: %w", key, err)
		}
	}
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
		return s.markGroupResolved(ctx, group, "pr", resolutionDetail(merged))
	case client != nil && issues.IsCompletedPlaneState(state):
		return s.markGroupResolved(ctx, group, trackerPlane, "Plane "+firstNonEmptyString(state.Name, issues.CompletedPlaneState))
	}
	return nil
}

// resolveLinearGroup mirrors resolvePlaneGroup for Linear: a merged PR moves
// an open ticket to its completed state with one comment; a ticket completed
// in Linear resolves its jobs.
func (s *Server) resolveLinearGroup(ctx context.Context, group ticketGroup, cache map[string][]GitHubPullRequest) error {
	client, key := group.linear, group.key
	state, err := client.IssueState(ctx, key)
	if err != nil {
		return fmt.Errorf("read ticket %s state: %w", key, err)
	}
	merged, found, err := s.groupPullRequest(ctx, group, []string{state.URL}, cache)
	if err != nil {
		return err
	}
	switch {
	case found:
		if !issues.IsCompletedLinearState(state) {
			if err := client.SetState(ctx, key, issues.CompletedLinearState); err != nil {
				return fmt.Errorf("move ticket %s to %s: %w", key, issues.CompletedLinearState, err)
			}
			if err := client.PostComment(ctx, key, linearResolutionComment(merged)); err != nil {
				return fmt.Errorf("comment on ticket %s: %w", key, err)
			}
		}
		return s.markGroupResolved(ctx, group, "pr", resolutionDetail(merged))
	case issues.IsCompletedLinearState(state):
		return s.markGroupResolved(ctx, group, trackerLinear, "Linear "+firstNonEmptyString(state.Name, issues.CompletedLinearState))
	}
	return nil
}

// resolveBuiltinGroup resolves a builtin ticket. A merged PR marks the
// ticket done, comments on it and resolves its finished jobs in one
// transaction; a ticket already done resolves any job still finished.
func (s *Server) resolveBuiltinGroup(ctx context.Context, group ticketGroup, cache map[string][]GitHubPullRequest) error {
	merged, found, err := s.groupPullRequest(ctx, group, nil, cache)
	if err != nil {
		return err
	}
	var pullRequest *GitHubPullRequest
	if found {
		pullRequest = &merged
	}
	if _, err := s.store.resolveBuiltinTicket(ctx, group.key, pullRequest); err != nil {
		return fmt.Errorf("resolve ticket %s: %w", group.key, err)
	}
	return nil
}

// reconcileTicketOutcomes resolves finished jobs whose tickets completed
// outside the factory. It runs once at startup to backfill existing cards and
// then roughly every ten minutes. Jobs are grouped by tracker, source and
// ticket so each ticket costs one lookup against the client that owns it, and
// external lookups are paced to stay under tracker rate limits.
func (s *Server) reconcileTicketOutcomes(ctx context.Context) error {
	if err := s.store.linkTicketAttempts(ctx); err != nil {
		log.Printf("link ticket attempts: %v", err)
	}
	jobs, err := s.store.listJobs(ctx, "")
	if err != nil {
		return fmt.Errorf("list jobs for resolution: %w", err)
	}
	var order []string
	groups := map[string]*ticketGroup{}
	for _, job := range jobs {
		if !terminalJobState(job.State) || job.State == "resolved" {
			continue
		}
		group, ok := s.ticketGroupFor(job)
		if !ok {
			continue
		}
		id := group.tracker + "\x00" + group.source + "\x00" + group.key
		existing, seen := groups[id]
		if !seen {
			order = append(order, id)
			existing = &group
			groups[id] = existing
		}
		existing.jobs = append(existing.jobs, job)
	}
	cache := map[string][]GitHubPullRequest{}
	var failures []error
	lookups := 0
	for _, id := range order {
		group := *groups[id]
		if group.tracker == trackerBuiltin {
			failures = append(failures, s.resolveBuiltinGroup(ctx, group, cache))
			continue
		}
		if lookups > 0 && s.planePause > 0 {
			select {
			case <-ctx.Done():
				return errors.Join(append(failures, ctx.Err())...)
			case <-time.After(s.planePause):
			}
		}
		lookups++
		if group.tracker == trackerLinear {
			failures = append(failures, s.resolveLinearGroup(ctx, group, cache))
		} else {
			failures = append(failures, s.resolvePlaneGroup(ctx, group, cache))
		}
	}
	return errors.Join(failures...)
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
