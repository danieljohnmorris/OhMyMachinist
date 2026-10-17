package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/plane"
	"github.com/owainlewis/machinist/internal/protocol"
)

// ErrPlaneSignature marks a webhook with a missing or invalid signature.
var ErrPlaneSignature = errors.New("plane webhook signature is invalid")

// defaultPlanePollQuiet is how long after the last verified webhook the poll
// fallback stays quiet.
const defaultPlanePollQuiet = 10 * time.Minute

// planeConfig is the resolved Plane intake configuration: config-file values
// overlaid with the runtime-editable "plane.*" settings.
type planeConfig struct {
	URL               string
	Workspace         string
	APIKey            string
	TriggerLabel      string
	TriggerLabelIDs   []string
	ApproveLabel      string
	FailedLabel       string
	ProgressState     string
	ReviewState       string
	Command           string
	AutoSubmit        bool
	StartComment      bool
	Shadow            bool
	UIURL             string
	PromptTemplate    string
	AutoMergeProjects map[string]bool
	Projects          map[string]bool
	RepoMap           plane.RepoMap
	Supervisors       plane.SupervisorMap
	PollEvery         time.Duration
	SyncEvery         time.Duration
	SyncSince         time.Time
	WebhookSecret     string
	AlertsDirectory   string
	WebhookLiveFile   string
	QuietWindow       time.Duration
	// ProjectPrompts overrides the prompt template per Machinist repository,
	// from the "plane.prompt_template.<repository>" settings.
	ProjectPrompts map[string]string
	// MergePolicies is the per-project auto-merge + deploy policy (MACH-24):
	// seed defaults layered with [plane.merge_policies] and the
	// "plane.merge_policy.<PROJECT>" / "plane.merge_policies" settings.
	MergePolicies map[string]config.MergePolicy
}

// PolicyFor returns the merge policy for a ticket key's project identifier.
func (c planeConfig) PolicyFor(key string) (config.MergePolicy, bool) {
	identifier := strings.ToUpper(strings.SplitN(key, "-", 2)[0])
	policy, ok := c.MergePolicies[identifier]
	return policy, ok
}

// promptTemplateFor returns the prompt template for a repository, preferring a
// per-project override over the global template. An empty result means the
// default template.
func (c planeConfig) promptTemplateFor(repo string) string {
	if template := strings.TrimSpace(c.ProjectPrompts[repo]); template != "" {
		return template
	}
	return c.PromptTemplate
}

// PlaneIntake evaluates Plane events and turns eligible tickets into jobs. It
// is safe for concurrent webhook deliveries.
type PlaneIntake struct {
	store           *Store
	definitionPath  string
	homeDirectory   string
	now             func() time.Time
	lastWebhook     atomic.Int64
	clientMu        sync.Mutex
	client          *plane.Client
	clientKey       string
	commandResolver commandResolver
	// autoMergeQueue is installed by the control plane: factory PRs for
	// auto-merge projects are queued for merge + deploy (MACH-24).
	autoMergeQueue func(context.Context, PlaneSyncCandidate, string) error
	// testPolicies, when non-nil, pins the resolved merge policies in tests.
	testPolicies map[string]config.MergePolicy
	logf         func(string, ...any)
}

// SetAutoMergeQueue installs the auto-merge enqueue hook.
func (p *PlaneIntake) SetAutoMergeQueue(hook func(context.Context, PlaneSyncCandidate, string) error) {
	p.autoMergeQueue = hook
}

// NewPlaneIntake builds an intake driver for a control-plane store.
func NewPlaneIntake(store *Store, definitionPath, homeDirectory string) (*PlaneIntake, error) {
	if store == nil {
		return nil, errors.New("plane intake requires a store")
	}
	if strings.TrimSpace(homeDirectory) == "" {
		if override := strings.TrimSpace(os.Getenv("MACHINIST_HOME")); override != "" {
			homeDirectory = override
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("resolve home directory: %w", err)
			}
			homeDirectory = filepath.Join(home, ".machinist")
		}
	}
	return &PlaneIntake{
		store: store, definitionPath: definitionPath, homeDirectory: homeDirectory,
		now: time.Now, logf: log.Printf,
	}, nil
}

// ImportState imports the retired script's JSON state files on first start.
func (p *PlaneIntake) ImportState(ctx context.Context) error {
	imported, err := p.store.ImportPlaneState(ctx, p.homeDirectory)
	if err != nil {
		return err
	}
	if imported {
		p.logf("plane intake: imported legacy state from %s", p.homeDirectory)
	}
	return nil
}

// config resolves the effective configuration.
func (p *PlaneIntake) config(ctx context.Context) (planeConfig, error) {
	definition, err := config.LoadDefinitions(p.definitionPath)
	if err != nil {
		return planeConfig{}, err
	}
	settings, err := p.store.PlaneSettings(ctx)
	if err != nil {
		return planeConfig{}, err
	}
	alertsDirectory, err := definition.ResolveAlerts()
	if err != nil {
		return planeConfig{}, err
	}
	if alertsDirectory == "" {
		alertsDirectory = filepath.Join(p.homeDirectory, "alerts")
	}
	base := definition.Plane
	resolved := planeConfig{
		URL:             firstNonEmptyString(base.URL, os.Getenv("PLANE_URL"), plane.DefaultURL),
		Workspace:       firstNonEmptyString(base.Workspace, os.Getenv("PLANE_WORKSPACE"), plane.DefaultWorkspace),
		APIKey:          os.Getenv("PLANE_API_KEY"),
		TriggerLabel:    firstNonEmptyString(setting(settings, "plane.trigger_label"), base.TriggerLabel, plane.DefaultTriggerLabel),
		ApproveLabel:    firstNonEmptyString(setting(settings, "plane.approve_label"), base.ApproveLabel, plane.DefaultApproveLabel),
		FailedLabel:     firstNonEmptyString(setting(settings, "plane.failed_label"), base.FailedLabel, plane.DefaultFailedLabel),
		ProgressState:   firstNonEmptyString(setting(settings, "plane.progress_state"), base.ProgressState, plane.DefaultProgressState),
		ReviewState:     firstNonEmptyString(setting(settings, "plane.review_state"), base.ReviewState, plane.DefaultReviewState),
		Command:         firstNonEmptyString(setting(settings, "plane.command"), base.Command, "build-pr"),
		UIURL:           firstNonEmptyString(setting(settings, "plane.ui_url"), base.UIURL),
		PromptTemplate:  firstNonEmptyString(setting(settings, "plane.prompt_template"), base.PromptTemplate),
		StartComment:    boolSetting(settings, "plane.start_comment", base.StartComment, true),
		AutoSubmit:      boolSetting(settings, "plane.auto_submit", base.AutoSubmit, true),
		Shadow:          boolSetting(settings, "plane.shadow", base.Shadow, true),
		WebhookSecret:   firstNonEmptyString(setting(settings, "plane.webhook_secret"), base.WebhookSecret, os.Getenv("PLANE_WEBHOOK_SECRET"), os.Getenv("MACHINIST_ISSUE_WEBHOOK_SECRET")),
		AlertsDirectory: alertsDirectory,
		WebhookLiveFile: filepath.Join(p.homeDirectory, "plane-webhook-live"),
		QuietWindow:     defaultPlanePollQuiet,
	}
	resolved.TriggerLabelIDs = stringList(setting(settings, "plane.trigger_label_ids"), base.TriggerLabelIDs, []string{plane.DefaultTriggerLabelID})
	resolved.SyncSince, err = parseSyncSince(firstNonEmptyString(setting(settings, "plane.sync_since"), base.SyncSince))
	if err != nil {
		return planeConfig{}, err
	}
	resolved.PollEvery, err = parsePlaneDuration(firstNonEmptyString(setting(settings, "plane.poll_every"), base.PollEvery), 2*time.Minute)
	if err != nil {
		return planeConfig{}, err
	}
	resolved.SyncEvery, err = parsePlaneDuration(firstNonEmptyString(setting(settings, "plane.sync_every"), base.SyncEvery), 20*time.Second)
	if err != nil {
		return planeConfig{}, err
	}
	resolved.RepoMap = plane.RepoMap{
		Repositories: mergeStringMaps(base.Repositories, jsonStringMap(setting(settings, "plane.repositories"))),
		Identifiers:  mergeStringMaps(base.Identifiers, jsonStringMap(setting(settings, "plane.identifiers"))),
		Default:      firstNonEmptyString(setting(settings, "plane.default_repo"), base.DefaultRepo),
	}
	resolved.Supervisors = plane.SupervisorMap(mergeStringMaps(map[string]string(base.Supervisors), jsonStringMap(setting(settings, "plane.supervisors"))))
	resolved.Projects = planeStringSet(setting(settings, "plane.projects"), base.Projects)
	resolved.AutoMergeProjects = planeStringSet(setting(settings, "plane.auto_merge_projects"), base.AutoMergeProjects)
	for key, value := range settings {
		if repository, ok := strings.CutPrefix(key, "plane.prompt_template."); ok && strings.TrimSpace(value) != "" {
			if resolved.ProjectPrompts == nil {
				resolved.ProjectPrompts = map[string]string{}
			}
			resolved.ProjectPrompts[repository] = value
		}
	}
	configured, err := config.MergePoliciesFromSettings(setting(settings, "plane.merge_policies"), settings)
	if err != nil {
		return planeConfig{}, err
	}
	for project, policy := range base.MergePolicies {
		configured[project] = policy
	}
	if p.testPolicies != nil {
		resolved.MergePolicies = p.testPolicies
		return resolved, nil
	}
	resolved.MergePolicies = config.ResolveMergePolicies(configured)
	return resolved, nil
}

// WebhookSecretValue is the configured signature secret, for the HTTP handler.
func (p *PlaneIntake) WebhookSecret(ctx context.Context) string {
	cfg, err := p.config(ctx)
	if err != nil {
		return ""
	}
	return cfg.WebhookSecret
}

// PlaneBase returns the configured Plane base URL and workspace so the web UI
// can build run links from server-provided values instead of a client-side
// constant. Falls back to the neutral placeholder defaults on a config error.
func (p *PlaneIntake) PlaneBase(ctx context.Context) (string, string) {
	cfg, err := p.config(ctx)
	if err != nil {
		return plane.DefaultURL, plane.DefaultWorkspace
	}
	return cfg.URL, cfg.Workspace
}

// clientFor returns a cached Plane REST client for the resolved config.
func (p *PlaneIntake) clientFor(cfg planeConfig) (*plane.Client, error) {
	p.clientMu.Lock()
	defer p.clientMu.Unlock()
	key := cfg.URL + "\x00" + cfg.Workspace + "\x00" + cfg.APIKey
	if p.client != nil && p.clientKey == key {
		return p.client, nil
	}
	client, err := plane.NewClient(plane.ClientConfig{URL: cfg.URL, Workspace: cfg.Workspace, APIKey: cfg.APIKey})
	if err != nil {
		return nil, err
	}
	p.client, p.clientKey = client, key
	return client, nil
}

// HandleWebhook verifies and processes one Plane delivery. It reports whether
// the delivery was accepted (signature valid) and an error when processing
// failed. A valid delivery always writes the webhook-live marker.
func (p *PlaneIntake) HandleWebhook(ctx context.Context, body []byte, signature, delivery string) (bool, error) {
	cfg, err := p.config(ctx)
	if err != nil {
		return false, err
	}
	if !plane.VerifySignature(cfg.WebhookSecret, body, signature) {
		return false, ErrPlaneSignature
	}
	p.lastWebhook.Store(p.now().UnixNano())
	p.markWebhookLive(cfg)
	_, issue, ok, err := plane.ParseWebhook(body, delivery)
	if err != nil {
		return true, err
	}
	if !ok {
		return true, nil
	}
	return true, p.processIssue(ctx, cfg, issue, "webhook:"+delivery, delivery, false)
}

// markWebhookLive writes the marker that told the legacy poll to stop.
func (p *PlaneIntake) markWebhookLive(cfg planeConfig) {
	if cfg.WebhookLiveFile == "" {
		return
	}
	if _, err := os.Stat(cfg.WebhookLiveFile); err == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(cfg.WebhookLiveFile), 0o755); err != nil {
		p.logf("plane intake: create webhook-live directory: %v", err)
		return
	}
	if err := os.WriteFile(cfg.WebhookLiveFile, []byte(p.now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		p.logf("plane intake: write webhook-live marker: %v", err)
	}
}

// processIssue decides and (unless shadowing) applies what an issue event
// requires: the approve bridge, the job submission and the state alert.
func (p *PlaneIntake) processIssue(ctx context.Context, cfg planeConfig, issue plane.Issue, source, delivery string, dryRun bool) error {
	identifier := firstNonEmptyString(issue.ProjectIdentifier, cfg.RepoMap.IdentifierFor(issue.ProjectID))
	key := plane.Key(identifier, issue.SequenceID)

	bridge := plane.NeedsApproveBridge(issue, cfg.ApproveLabel, nil, cfg.TriggerLabel, cfg.TriggerLabelIDs)
	if bridge {
		if dryRun || cfg.Shadow {
			p.recordShadow(ctx, "approve-bridge", key, map[string]any{"issue_id": issue.ID, "source": source})
		} else {
			if err := p.applyApproveBridge(ctx, cfg, issue, key); err != nil {
				return err
			}
		}
		if !triggeredWithBridge(issue, bridge, cfg) {
			return nil
		}
	}
	if !plane.Triggered(issue, cfg.TriggerLabel, cfg.TriggerLabelIDs) && !bridge {
		return nil
	}
	if issue.IsDraft || issue.DeletedAt != "" || issue.ArchivedAt != "" {
		return nil
	}

	p.emitStateAlert(ctx, cfg, issue, source, delivery, dryRun)

	spec := plane.Spec(issue)
	if strings.TrimSpace(spec) == "" {
		p.logf("plane intake: skip %s: empty description (no spec, no job)", key)
		return nil
	}
	hash := plane.SpecHash(issue.Name, spec)
	url := plane.BrowseURL(cfg.URL, cfg.Workspace, key)

	record, found, err := p.store.PlaneIntake(ctx, issue.ID)
	if err != nil {
		return err
	}
	if found && record.SpecHash == hash {
		return nil
	}
	repo, mapped := cfg.RepoMap.RepoFor(issue.ProjectID, identifier)
	if !mapped {
		p.logf("plane intake: %s has no machinist repo mapping; job not queued", key)
		if !dryRun && !cfg.Shadow {
			record.SpecHash, record.Key, record.ProjectID = hash, key, issue.ProjectID
			record.Source, record.DeliveryID = source, delivery
			return p.store.PutPlaneIntake(ctx, issue.ID, record)
		}
		return nil
	}

	jobID, activeState, active, err := p.store.ActiveTriggerJob(ctx, url, key)
	if err != nil {
		return err
	}
	if active && activeState == "running" {
		p.logf("plane intake: %s job %s is still running; ignoring ticket edit", key, jobID)
		return nil
	}

	task := protocol.Task{Title: plane.Title(key, issue.Name), SourceURL: url, Spec: spec}
	admission := PlaneAdmission{
		Prompt:   plane.RenderPrompt(cfg.promptTemplateFor(repo), url, key, issue.Name, spec),
		Title:    plane.Title(key, issue.Name),
		Task:     &task,
		Project:  identifier,
		IssueKey: key, Subject: url, Repository: repo, Source: source, DeliveryID: delivery, SpecHash: hash,
	}
	commandName := cfg.Command
	if active && activeState == "queued" {
		// MACH-23 dedupe: rewrite the queued job's spec in place.
		if dryRun || cfg.Shadow {
			p.recordShadow(ctx, "update", key, map[string]any{"job_id": jobID, "hash": hash, "repo": repo, "source": source})
			return nil
		}
		if err := p.store.UpdateQueuedTriggerJob(ctx, jobID, TriggerAdmission{
			Prompt: admission.Prompt, Title: admission.Title, Task: admission.Task,
			SelectionName: commandName, Command: p.queuedCommand(commandName),
		}); err != nil {
			return err
		}
		record.SpecHash, record.Key, record.ProjectID, record.JobID = hash, key, issue.ProjectID, jobID
		record.Source, record.DeliveryID = source, delivery
		if err := p.store.PutPlaneIntake(ctx, issue.ID, record); err != nil {
			return err
		}
		p.logf("plane intake: updated queued job %s for %s", jobID, key)
		return nil
	}

	if dryRun || cfg.Shadow {
		p.recordShadow(ctx, "job", key, map[string]any{"hash": hash, "repo": repo, "source": source, "delivery": delivery, "prompt": admission.Prompt})
		return nil
	}
	if p.commandResolver == nil {
		return errors.New("plane intake: command resolver is not configured")
	}
	command, err := p.commandResolver(commandName)
	if err != nil {
		return fmt.Errorf("plane intake: resolve command %q: %w", commandName, err)
	}
	admission.Command = command
	if !cfg.AutoSubmit {
		p.logf("plane intake: auto-submit disabled; would queue %s for %s", key, repo)
		return nil
	}
	created, err := p.store.CreatePlaneJob(ctx, admission)
	if err != nil {
		return err
	}
	record.SpecHash, record.Key, record.ProjectID, record.JobID = hash, key, issue.ProjectID, created
	record.Source, record.DeliveryID = source, delivery
	if err := p.store.PutPlaneIntake(ctx, issue.ID, record); err != nil {
		return err
	}
	p.postStartComment(ctx, cfg, issue.ProjectID, issue.ID, created, key)
	p.logf("plane intake: queued %s job %s repo=%s source=%s", key, created, repo, source)
	return nil
}

// triggeredWithBridge reports whether an issue is eligible, treating a pending
// approve bridge as if the trigger label were already added.
func triggeredWithBridge(issue plane.Issue, bridge bool, cfg planeConfig) bool {
	if plane.Triggered(issue, cfg.TriggerLabel, cfg.TriggerLabelIDs) {
		return true
	}
	return bridge
}

// applyApproveBridge adds the trigger label to an approved ticket, never
// removing labels, then advertises the ticket as triggered for this pass.
func (p *PlaneIntake) applyApproveBridge(ctx context.Context, cfg planeConfig, issue plane.Issue, key string) error {
	client, err := p.clientFor(cfg)
	if err != nil {
		return err
	}
	labels, err := client.Labels(ctx, issue.ProjectID)
	if err != nil {
		return err
	}
	triggerID := ""
	for _, label := range labels {
		if strings.EqualFold(label.Name, cfg.TriggerLabel) {
			triggerID = label.ID
			break
		}
	}
	if triggerID == "" {
		p.logf("plane intake: approve bridge %s: project has no %q label", key, cfg.TriggerLabel)
		return nil
	}
	fresh, err := client.Issue(ctx, issue.ProjectID, issue.ID)
	if err != nil {
		return err
	}
	current := labelIDs(fresh)
	for _, id := range current {
		if id == triggerID {
			return nil
		}
	}
	body := map[string]any{"labels": append(append([]string{}, current...), triggerID)}
	if err := client.PatchIssue(ctx, issue.ProjectID, issue.ID, body); err != nil {
		return err
	}
	p.logf("plane intake: approve bridge %s: %q present -> added %q label", key, cfg.ApproveLabel, cfg.TriggerLabel)
	return nil
}

// emitStateAlert records the observed state and writes a state-change alert
// when it changed.
func (p *PlaneIntake) emitStateAlert(ctx context.Context, cfg planeConfig, issue plane.Issue, source, delivery string, dryRun bool) {
	states, err := p.store.PlaneAlertStates(ctx)
	if err != nil {
		p.logf("plane intake: read alert state: %v", err)
		return
	}
	previous, hadPrevious := states[issue.ID]
	if hadPrevious && previous.StateID == issue.StateID {
		return
	}
	identifier := firstNonEmptyString(issue.ProjectIdentifier, cfg.RepoMap.IdentifierFor(issue.ProjectID))
	key := plane.Key(identifier, issue.SequenceID)
	repo, _ := cfg.RepoMap.RepoFor(issue.ProjectID, identifier)
	alert := plane.StateAlert{
		Type: "plane.workitem.state_changed", DetectedAt: p.now().UTC().Format(time.RFC3339),
		Event: source, Action: "state", Delivery: delivery,
		Plane: plane.StateAlertItem{
			Key: key, IssueID: issue.ID, ProjectID: issue.ProjectID, Repo: repo, URL: plane.BrowseURL(cfg.URL, cfg.Workspace, key), Name: issue.Name,
		},
		StateTo: plane.StateSnapshot{ID: issue.StateID, Name: issue.StateName, Group: issue.StateGroup},
	}
	if hadPrevious {
		alert.StateFrom = &plane.StateSnapshot{ID: previous.StateID, Name: previous.Name, Group: previous.Group}
	}
	if dryRun || cfg.Shadow {
		p.recordShadow(ctx, "state-alert", key, alert)
		return
	}
	if err := writeAlertFile(cfg.AlertsDirectory, fmt.Sprintf("plane-%s-%d.json", key, p.now().UnixMilli()), alert); err != nil {
		p.logf("plane intake: write state alert: %v", err)
	}
	if err := p.store.PutPlaneAlertState(ctx, PlaneAlertState{IssueID: issue.ID, StateID: issue.StateID, Name: issue.StateName, Group: issue.StateGroup}); err != nil {
		p.logf("plane intake: store alert state: %v", err)
	}
}

// Poll scans every configured project for eligible tickets.
func (p *PlaneIntake) Poll(ctx context.Context, dryRun bool) (int, error) {
	cfg, err := p.config(ctx)
	if err != nil {
		return 0, err
	}
	if !dryRun && p.pollQuiet(cfg) {
		p.logf("plane intake: poll quiet (webhooks arriving)")
		return 0, nil
	}
	client, err := p.clientFor(cfg)
	if err != nil {
		return 0, err
	}
	projects, err := client.Projects(ctx)
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, project := range projects {
		if !projectSelected(cfg, project) {
			continue
		}
		labels, err := client.Labels(ctx, project.ID)
		if err != nil {
			p.logf("plane intake: list labels for %s: %v", project.Identifier, err)
			continue
		}
		hasTrigger, hasApprove := false, false
		for _, label := range labels {
			switch {
			case strings.EqualFold(label.Name, cfg.TriggerLabel):
				hasTrigger = true
			case strings.EqualFold(label.Name, cfg.ApproveLabel):
				hasApprove = true
			}
		}
		if !hasTrigger {
			continue
		}
		if hasApprove && !dryRun {
			if err := p.approveScan(ctx, cfg, client, project); err != nil {
				p.logf("plane intake: approve scan %s: %v", project.Identifier, err)
			}
		}
		cursor := ""
		for {
			page, err := client.Issues(ctx, project.ID, cursor)
			if err != nil {
				return queued, err
			}
			for _, issue := range page.Results {
				if issue.ProjectIdentifier == "" {
					issue.ProjectIdentifier = project.Identifier
				}
				before, _, _ := p.store.PlaneIntake(ctx, issue.ID)
				if err := p.processIssue(ctx, cfg, issue, "poll", "", dryRun); err != nil {
					p.logf("plane intake: poll %s: %v", issue.ID, err)
				}
				after, _, _ := p.store.PlaneIntake(ctx, issue.ID)
				if after.JobID != "" && after.JobID != before.JobID {
					queued++
				}
			}
			if !page.Next || page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
	}
	p.logf("plane intake: poll done: %d new job(s)", queued)
	return queued, nil
}

// approveScan runs the approve bridge over a project's first page of issues.
func (p *PlaneIntake) approveScan(ctx context.Context, cfg planeConfig, client *plane.Client, project plane.Project) error {
	page, err := client.Issues(ctx, project.ID, "")
	if err != nil {
		return err
	}
	for _, issue := range page.Results {
		if issue.ProjectIdentifier == "" {
			issue.ProjectIdentifier = project.Identifier
		}
		if !plane.NeedsApproveBridge(issue, cfg.ApproveLabel, nil, cfg.TriggerLabel, cfg.TriggerLabelIDs) {
			continue
		}
		key := plane.Key(issue.ProjectIdentifier, issue.SequenceID)
		if err := p.applyApproveBridge(ctx, cfg, issue, key); err != nil {
			p.logf("plane intake: approve bridge %s: %v", key, err)
		}
	}
	return nil
}

// pollQuiet reports whether the poll fallback should stand down because
// webhooks are arriving (recent delivery or the legacy live marker).
func (p *PlaneIntake) pollQuiet(cfg planeConfig) bool {
	if last := p.lastWebhook.Load(); last > 0 {
		if p.now().Sub(time.Unix(0, last)) < cfg.QuietWindow {
			return true
		}
	}
	if cfg.WebhookLiveFile != "" {
		if _, err := os.Stat(cfg.WebhookLiveFile); err == nil {
			return true
		}
	}
	return false
}

// Sync applies Machinist run outcomes to the linked Plane tickets.
func (p *PlaneIntake) Sync(ctx context.Context) error {
	cfg, err := p.config(ctx)
	if err != nil {
		return err
	}
	if cfg.APIKey == "" {
		return nil
	}
	client, err := p.clientFor(cfg)
	if err != nil {
		return err
	}
	candidates, err := p.store.PlaneSyncCandidates(ctx, cfg.SyncSince)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		return nil
	}
	progress, err := p.store.PlaneSyncAll(ctx)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		record := progress[candidate.JobID]
		phase, ok := plane.PhaseFor(candidate.RunState)
		if !ok {
			continue
		}
		if record.Started == "" && phase != "" {
			if cfg.Shadow {
				p.recordShadow(ctx, "state-sync", candidate.IssueKey, map[string]any{"job_id": candidate.JobID, "phase": plane.PhaseStart, "run_state": candidate.RunState})
			} else if err := p.applySync(ctx, cfg, client, candidate, plane.PhaseStart, ""); err != nil {
				p.logf("plane intake: state sync %s: %v", candidate.IssueKey, err)
				continue
			}
			record.Started = plane.PhaseStart
		}
		if record.Ended == "" {
			switch candidate.RunState {
			case "succeeded":
				pr, hasPR := plane.ParsePullRequest(candidate.Events)
				if hasPR {
					if cfg.Shadow {
						p.recordShadow(ctx, "state-sync", candidate.IssueKey, map[string]any{"job_id": candidate.JobID, "phase": plane.PhaseReview, "pr": pr})
					} else if err := p.applySync(ctx, cfg, client, candidate, plane.PhaseReview, pr); err != nil {
						p.logf("plane intake: state sync %s: %v", candidate.IssueKey, err)
						continue
					}
					record.Ended = plane.PhaseReview + " pr=" + pr
				} else {
					record.Ended = "succeeded: no PR opened, state left"
				}
			case "failed":
				if cfg.Shadow {
					p.recordShadow(ctx, "state-sync", candidate.IssueKey, map[string]any{"job_id": candidate.JobID, "phase": plane.PhaseFailed})
				} else if err := p.applySync(ctx, cfg, client, candidate, plane.PhaseFailed, ""); err != nil {
					p.logf("plane intake: state sync %s: %v", candidate.IssueKey, err)
					continue
				}
				record.Ended = plane.PhaseFailed
			}
		}
		if record.Notified == "" && strings.Contains(record.Ended, "pr=") {
			pr := strings.TrimSpace(strings.SplitN(record.Ended, "pr=", 2)[1])
			if cfg.Shadow {
				p.recordShadow(ctx, "pr-notify", candidate.IssueKey, map[string]any{"job_id": candidate.JobID, "pr": pr})
			} else {
				p.notifyPROpened(ctx, cfg, candidate, pr)
				if p.autoMergeQueue != nil {
					if err := p.autoMergeQueue(ctx, candidate, pr); err != nil {
						p.logf("plane intake: queue auto-merge %s: %v", candidate.IssueKey, err)
					}
				}
			}
			record.Notified = "queued"
		}
		if record.JobID == "" {
			record.JobID = candidate.JobID
		}
		if err := p.store.PutPlaneSync(ctx, record); err != nil {
			return err
		}
	}
	return nil
}

// applySync performs one ticket state/label patch.
func (p *PlaneIntake) applySync(ctx context.Context, cfg planeConfig, client *plane.Client, candidate PlaneSyncCandidate, phase, pr string) error {
	issue, err := client.Issue(ctx, candidate.ProjectID, candidate.IssueID)
	if err != nil {
		return err
	}
	labels := labelIDs(issue)
	body := map[string]any{}
	switch phase {
	case plane.PhaseStart:
		if plane.StartMovesState(issue.StateGroup) {
			stateID, err := p.ensureState(ctx, client, cfg, candidate.ProjectID, cfg.ProgressState, "started", false)
			if err != nil {
				return err
			}
			if stateID != "" {
				body["state"] = stateID
			}
		}
		if failedID := p.failedLabelID(ctx, client, cfg, candidate.ProjectID, false); failedID != "" {
			if patched, changed := plane.DropFailedLabelPatch(labels, failedID); changed {
				body["labels"] = patched
			}
		}
	case plane.PhaseReview:
		if plane.ReviewMovesState(issue.StateGroup) {
			stateID, err := p.ensureState(ctx, client, cfg, candidate.ProjectID, cfg.ReviewState, "started", true)
			if err != nil {
				return err
			}
			if stateID != "" {
				body["state"] = stateID
			}
		}
	case plane.PhaseFailed:
		failedID := p.failedLabelID(ctx, client, cfg, candidate.ProjectID, true)
		if patched, changed := plane.FailedLabelPatch(labels, failedID); changed {
			body["labels"] = patched
		}
	}
	if len(body) == 0 {
		return nil
	}
	if err := client.PatchIssue(ctx, candidate.ProjectID, candidate.IssueID, body); err != nil {
		return err
	}
	p.logf("plane intake: state sync %s %s -> %v", candidate.IssueKey, phase, bodyKeys(body))
	_ = pr
	return nil
}

// ensureState returns a project state id by name, creating it when allowed.
func (p *PlaneIntake) ensureState(ctx context.Context, client *plane.Client, cfg planeConfig, projectID, name, group string, create bool) (string, error) {
	states, err := client.States(ctx, projectID)
	if err != nil {
		return "", err
	}
	for _, state := range states {
		if strings.EqualFold(state.Name, name) {
			return state.ID, nil
		}
	}
	if !create {
		return "", nil
	}
	created, err := client.CreateState(ctx, projectID, name, group, "#8B5CF6")
	if err != nil {
		return "", err
	}
	p.logf("plane intake: created state %q in %s -> %s", name, projectID, created.ID)
	return created.ID, nil
}

// failedLabelID returns the failed label id, creating it when allowed.
func (p *PlaneIntake) failedLabelID(ctx context.Context, client *plane.Client, cfg planeConfig, projectID string, create bool) string {
	labels, err := client.Labels(ctx, projectID)
	if err != nil {
		return ""
	}
	for _, label := range labels {
		if strings.EqualFold(label.Name, cfg.FailedLabel) {
			return label.ID
		}
	}
	if !create {
		return ""
	}
	created, err := client.CreateLabel(ctx, projectID, cfg.FailedLabel, "#DC2626")
	if err != nil {
		p.logf("plane intake: create failed label: %v", err)
		return ""
	}
	return created.ID
}

// notifyPROpened writes the PR-opened alert for the ticket supervisor.
func (p *PlaneIntake) notifyPROpened(ctx context.Context, cfg planeConfig, candidate PlaneSyncCandidate, pr string) {
	autoMerge := cfg.AutoMergeProjects[strings.ToUpper(strings.SplitN(candidate.IssueKey, "-", 2)[0])]
	repo := candidate.Repo
	if repo == "" {
		identifier := strings.SplitN(candidate.IssueKey, "-", 2)[0]
		repo, _ = cfg.RepoMap.RepoFor(candidate.ProjectID, identifier)
	}
	alert := plane.NewPROpenedAlert(
		cfg.Supervisors.SupervisorFor(candidate.IssueKey), candidate.IssueKey,
		plane.BrowseURL(cfg.URL, cfg.Workspace, candidate.IssueKey), pr, candidate.JobID, repo,
		plane.RunURL(cfg.UIURL, candidate.JobID), p.now().UTC().Format(time.RFC3339), autoMerge,
	)
	if err := writeAlertFile(cfg.AlertsDirectory, fmt.Sprintf("pr-%s-%s.json", candidate.IssueKey, candidate.JobID), alert); err != nil {
		p.logf("plane intake: write PR alert: %v", err)
	}
}

// postStartComment posts the run-started comment on the ticket.
func (p *PlaneIntake) postStartComment(ctx context.Context, cfg planeConfig, projectID, issueID, jobID, key string) {
	if !cfg.StartComment || cfg.UIURL == "" || cfg.APIKey == "" {
		return
	}
	client, err := p.clientFor(cfg)
	if err != nil {
		return
	}
	comment := plane.StartCommentHTML(key, jobID, plane.RunURL(cfg.UIURL, jobID))
	if err := client.PostComment(ctx, projectID, issueID, comment); err != nil {
		p.logf("plane intake: start comment %s: %v", key, err)
		return
	}
	p.logf("plane intake: start comment %s posted for %s", key, jobID)
}

// recordShadow stores a decision that shadow mode did not act on.
func (p *PlaneIntake) recordShadow(ctx context.Context, kind, key string, decision any) {
	body, err := json.Marshal(decision)
	if err != nil {
		return
	}
	if err := p.store.RecordPlaneShadow(ctx, p.now().UTC().Format(time.RFC3339), kind, key, string(body)); err != nil {
		p.logf("plane intake: record shadow decision: %v", err)
	}
}

// commandResolver resolves a configured command into a runnable command.
type commandResolver func(name string) (config.ResolvedCommand, error)

// queuedCommand resolves just enough command identity to rewrite a queued run;
// the caller overwrites it with the fully resolved command before creating a
// new job.
func (p *PlaneIntake) queuedCommand(name string) config.ResolvedCommand {
	if p.commandResolver == nil {
		return config.ResolvedCommand{Name: name}
	}
	command, err := p.commandResolver(name)
	if err != nil {
		return config.ResolvedCommand{Name: name}
	}
	return command
}

// SetCommandResolver installs the resolver used to build jobs.
func (p *PlaneIntake) SetCommandResolver(resolver commandResolver) { p.commandResolver = resolver }

// ProcessPayload processes a saved webhook payload, as `plane file` does. It
// skips signature verification and reports the event through the given source.
func (p *PlaneIntake) ProcessPayload(ctx context.Context, body []byte, source string, dryRun bool) error {
	cfg, err := p.config(ctx)
	if err != nil {
		return err
	}
	_, issue, ok, err := plane.ParseWebhook(body, "")
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("payload does not describe a Plane issue event")
	}
	return p.processIssue(ctx, cfg, issue, source, "", dryRun)
}

// planeWebhook handles a Plane delivery at POST /api/v1/webhooks/issues.
func (s *Server) planeWebhook(response http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(response, request.Body, plane.MaxBodyBytes))
	if err != nil {
		writeError(response, http.StatusBadRequest, errors.New("read plane webhook body"))
		return
	}
	accepted, err := s.planeIntake.HandleWebhook(request.Context(), body, request.Header.Get("X-Plane-Signature"), request.Header.Get("X-Plane-Delivery"))
	if errors.Is(err, ErrPlaneSignature) || !accepted {
		writeUnauthorized(response)
		return
	}
	if err != nil {
		log.Printf("plane intake: webhook: %v", err)
		writeError(response, http.StatusInternalServerError, errors.New("plane intake failed"))
		return
	}
	response.WriteHeader(http.StatusAccepted)
}

// planeIntakeReportHandler serves GET /api/v1/plane/intake: the runtime-editable
// settings plus the shadow decisions and imported script records used for the
// cutover comparison report.
func (s *Server) planeIntakeReportHandler(response http.ResponseWriter, request *http.Request) {
	settings, err := s.store.PlaneSettings(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	shadow, err := s.store.PlaneShadowDecisions(request.Context(), 200)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	scriptJobs, err := s.store.PlaneScriptJobs(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, map[string]any{
		"settings": settings, "shadow_decisions": shadow, "script_jobs": scriptJobs,
	})
}

// projectSelected reports whether a project passes the optional filter.
func projectSelected(cfg planeConfig, project plane.Project) bool {
	if len(cfg.Projects) == 0 {
		return true
	}
	return cfg.Projects[project.ID] || cfg.Projects[project.Identifier]
}

// labelIDs flattens an issue's labels into their UUIDs.
func labelIDs(issue plane.Issue) []string {
	ids := []string{}
	for _, label := range issue.Labels {
		if label.ID != "" {
			ids = append(ids, label.ID)
		}
	}
	return append(ids, issue.LabelIDs...)
}

// writeAlertFile writes one alert JSON file atomically into the pending spool.
func writeAlertFile(directory, name string, payload any) error {
	if directory == "" {
		return errors.New("alert directory is not configured")
	}
	pending := filepath.Join(directory, "pending")
	if err := os.MkdirAll(pending, 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(pending, name)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	temporary := filepath.Join(pending, "."+name+".tmp")
	if err := os.WriteFile(temporary, append(body, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func bodyKeys(body map[string]any) []string {
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	return keys
}

func setting(settings map[string]string, key string) string { return strings.TrimSpace(settings[key]) }

func boolSetting(settings map[string]string, key string, fallback *bool, defaultValue bool) bool {
	if value := setting(settings, key); value != "" {
		return value == "1" || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes")
	}
	if fallback != nil {
		return *fallback
	}
	return defaultValue
}

func jsonStringMap(value string) map[string]string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	result := map[string]string{}
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		return nil
	}
	return result
}

func mergeStringMaps(base map[string]string, overrides map[string]string) map[string]string {
	merged := map[string]string{}
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range overrides {
		merged[key] = value
	}
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func planeStringSet(value string, fallback []string) map[string]bool {
	set := map[string]bool{}
	for _, entry := range fallback {
		if entry = strings.TrimSpace(entry); entry != "" {
			set[entry] = true
		}
	}
	for _, entry := range strings.Split(value, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			set[entry] = true
		}
	}
	return set
}

func stringList(value string, fallback, defaults []string) []string {
	list := []string{}
	for _, entry := range fallback {
		if entry = strings.TrimSpace(entry); entry != "" {
			list = append(list, entry)
		}
	}
	for _, entry := range strings.Split(value, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			list = append(list, entry)
		}
	}
	if len(list) == 0 {
		return defaults
	}
	return list
}

func parsePlaneDuration(value string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return fallback, fmt.Errorf("invalid plane duration %q", value)
	}
	return duration, nil
}

func parseSyncSince(value string) (time.Time, error) {
	if strings.TrimSpace(value) == "" {
		// The legacy sync only considered runs started at or after this date.
		return time.Date(2026, 10, 10, 10, 48, 0, 0, time.UTC), nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid plane sync_since %q: %w", value, err)
	}
	return parsed, nil
}
