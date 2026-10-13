package controlplane

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/issues"
	"github.com/owainlewis/machinist/internal/protocol"
)

// The runner records up to 64 MiB before base64 encoding. The JSON envelope remains
// below this limit, including per-event and string-escaping overhead.
const maxCompletionBytes = 96 << 20

const maxRequestBytes = 1 << 20

const workerAvailabilityWindow = 15 * time.Second

//go:embed web/dist/* web/dist/assets/*
var webAssets embed.FS

type Server struct {
	store                *Store
	definitionPath       string
	triggers             []config.ResolvedTrigger
	github               githubTriggerClient
	issueSources         map[string]issuePoller
	planeClients         map[string]*issues.PlaneClient
	githubRepositories   map[string]string
	webhookSecret        string
	resolutionEvery      time.Duration
	planePause           time.Duration
	schedulerEvery       time.Duration
	now                  func() time.Time
	schedulerError       func(error)
	shutdownTimeout      time.Duration
	maxConcurrentJobs    int
	workerToken          string
	authMode             string
	reviewDefaultProfile string
	failAlerts           *failAlertWriter
	csrfToken            string
	handler              http.Handler
}

// defaultResolutionEvery is the ticket reconciliation cadence: fast enough to
// close cards shortly after external work lands, slow enough to stay cheap.
const defaultResolutionEvery = 10 * time.Minute

type statusResponse struct {
	Workflows []string `json:"workflows"`
	Snapshot
	Commands          []string `json:"commands"`
	Repositories      []string `json:"repositories"`
	MaxConcurrentJobs *int     `json:"max_concurrent_jobs"`
	RunningJobs       int      `json:"running_jobs"`
	CSRFToken         string   `json:"csrf_token"`
}

type submitRequest struct {
	Title      string `json:"title"`
	SourceURL  string `json:"source_url"`
	Spec       string `json:"spec"`
	Workflow   string `json:"workflow"`
	Prompt     string `json:"prompt"`
	Repository string `json:"repository"`
	Command    string `json:"command"`
	Model      string `json:"model"`
	Project    string `json:"project"`
}

type commandDefinitionResponse struct {
	Name     string `json:"name"`
	Executor string `json:"executor"`
	Timeout  string `json:"timeout"`
	Hash     string `json:"hash"`
	Prompt   string `json:"prompt"`
}

type workflowStepDefinition struct {
	Name     string `json:"name"`
	Approval bool   `json:"approval"`
}

type definitionsResponse struct {
	Workflows map[string][]workflowStepDefinition `json:"workflows"`
	Commands  []commandDefinitionResponse         `json:"commands"`
}

type catalogResponse struct {
	Workflows    []string `json:"workflows"`
	Commands     []string `json:"commands"`
	Repositories []string `json:"repositories"`
}

func NewServer(store *Store, definitionPath, workerToken string, maxConcurrentJobs int, authMode string) (*Server, error) {
	if maxConcurrentJobs < 0 {
		return nil, errors.New("max concurrent jobs cannot be negative")
	}
	if authMode != config.AuthTokens && authMode != config.AuthNone {
		return nil, fmt.Errorf("auth mode must be %q or %q", config.AuthTokens, config.AuthNone)
	}
	csrfToken, err := randomID("csrf", 24)
	if err != nil {
		return nil, err
	}
	managedTriggers, err := config.LoadTriggers(definitionPath)
	if err != nil {
		return nil, err
	}
	definition, e := config.LoadDefinitions(definitionPath)
	if e != nil {
		return nil, e
	}
	storage, e := definition.ResolveStorage(store.storageConfig.Path)
	if e != nil {
		return nil, e
	}
	if e = store.configureStorage(storage); e != nil {
		return nil, e
	}
	alertsDirectory, e := definition.ResolveAlerts()
	if e != nil {
		return nil, e
	}
	startup := time.Now().UTC()
	definitions := make([]TriggerDefinition, 0, len(managedTriggers))
	for _, trigger := range managedTriggers {
		definitions = append(definitions, TriggerDefinition{
			Identity: trigger.Identity, Family: trigger.Family,
			ConfigSignature: trigger.Signature, NextDueAt: trigger.FirstDue(startup),
		})
	}
	if err := store.SyncTriggers(context.Background(), definitions); err != nil {
		return nil, fmt.Errorf("restore managed triggers: %w", err)
	}
	issueSources, planeClients, e := loadIssueSources(definitionPath)
	if e != nil {
		return nil, e
	}
	if len(planeClients) == 0 {
		// Jobs submitted by an external Plane intake have no issue trigger.
		// A workspace client from PLANE_URL/PLANE_WORKSPACE/PLANE_API_KEY
		// still lets the reconciler resolve their tickets by key.
		if client, err := issues.NewPlaneWorkspaceClient(issues.PlaneConfig{
			URL: os.Getenv("PLANE_URL"), Workspace: os.Getenv("PLANE_WORKSPACE"), APIKey: os.Getenv("PLANE_API_KEY"),
		}); err == nil {
			planeClients = map[string]*issues.PlaneClient{"": client}
		}
	}
	server := &Server{
		store: store, definitionPath: definitionPath, triggers: managedTriggers,
		github: NewGitHubCLI("gh", 30*time.Second), now: time.Now,
		issueSources:       issueSources,
		planeClients:       planeClients,
		githubRepositories: definition.GitHub.Repositories,
		webhookSecret:      definition.IssueSources.WebhookSecretValue(),
		resolutionEvery:    defaultResolutionEvery,
		planePause:         2500 * time.Millisecond, // ~2 Plane calls per ticket; stays under 60 req/min
		schedulerEvery:     30 * time.Second, shutdownTimeout: 5 * time.Second,
		schedulerError:    func(err error) { log.Printf("scheduler: %v", err) },
		failAlerts:        &failAlertWriter{directory: alertsDirectory},
		maxConcurrentJobs: maxConcurrentJobs, workerToken: workerToken, csrfToken: csrfToken,
		authMode:             authMode,
		reviewDefaultProfile: definition.Review.DefaultProfile,
	}
	server.handler, err = server.routes()
	if err != nil {
		return nil, err
	}
	return server, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) Serve(ctx context.Context, listen string, onListening func(net.Addr)) error {
	if err := validateListenAddress(listen); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listen, err)
	}
	defer listener.Close()
	if onListening != nil {
		onListening(listener.Addr())
	}
	if s.authOff() {
		warnOnAuthDisabled(listener.Addr())
	}
	httpServer := &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	done := make(chan error, 1)
	go func() {
		err := httpServer.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()
	schedulerCtx, stopScheduler := context.WithCancel(ctx)
	defer stopScheduler()
	schedulerDone := make(chan error, 1)
	go func() { schedulerDone <- s.runScheduler(schedulerCtx) }()
	stopHTTP := func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
		defer cancel()
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		var forceCloseErr error
		if shutdownErr != nil {
			forceCloseErr = httpServer.Close()
		}
		// Shutdown can run before Serve registers the listener when the
		// callback cancels the context. Close it explicitly and wait for the
		// serving goroutine so every cancellation path releases the socket.
		closeErr := listener.Close()
		<-done
		var shutdownFailure error
		if shutdownErr != nil {
			shutdownFailure = fmt.Errorf("stop control plane: %w", shutdownErr)
		}
		if forceCloseErr != nil && !errors.Is(forceCloseErr, http.ErrServerClosed) {
			shutdownFailure = errors.Join(shutdownFailure, fmt.Errorf("force close control plane: %w", forceCloseErr))
		}
		if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
			shutdownFailure = errors.Join(shutdownFailure, fmt.Errorf("stop control plane listener: %w", closeErr))
		}
		return shutdownFailure
	}
	select {
	case err := <-done:
		stopScheduler()
		return errors.Join(err, <-schedulerDone)
	case err := <-schedulerDone:
		if shutdownErr := stopHTTP(); shutdownErr != nil && err == nil {
			return shutdownErr
		}
		return err
	case <-ctx.Done():
		stopScheduler()
		shutdownErr := stopHTTP()
		return errors.Join(shutdownErr, <-schedulerDone)
	}
}

func (s *Server) runScheduler(ctx context.Context) error {
	var schedulers sync.WaitGroup
	loop := func(interval time.Duration, delayFirst bool, work func(context.Context) error) {
		if interval <= 0 {
			return
		}
		schedulers.Add(1)
		go func() {
			defer schedulers.Done()
			if delayFirst && !sleep(ctx, interval) {
				return
			}
			for {
				s.reportSchedulerError(work(ctx))
				if !sleep(ctx, interval) {
					return
				}
			}
		}()
	}
	for _, trigger := range s.triggers {
		loop(s.schedulerEvery, false, func(ctx context.Context) error {
			if err := s.processManagedTrigger(ctx, trigger); err != nil {
				return fmt.Errorf("trigger %q: %w", trigger.Identity, err)
			}
			return nil
		})
	}
	loop(s.schedulerEvery, true, s.maintainState)
	loop(s.schedulerEvery, true, s.store.CleanupArtifacts)
	loop(s.resolutionEvery, false, s.reconcileTicketOutcomes)
	<-ctx.Done()
	schedulers.Wait()
	return nil
}

// sleep waits for the duration and reports false when ctx ends first.
func sleep(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (s *Server) maintainState(ctx context.Context) error {
	_, reclaimErr := s.store.ReclaimExpiredLeases(ctx)
	_, pruneErr := s.store.PruneSupersededWorkers(ctx, s.store.now().UTC().Add(-workerAvailabilityWindow))
	return errors.Join(reclaimErr, pruneErr)
}

func (s *Server) reportSchedulerError(err error) {
	if err != nil && !errors.Is(err, context.Canceled) && s.schedulerError != nil {
		s.schedulerError(err)
	}
}

func (s *Server) routes() (http.Handler, error) {
	dist, err := fs.Sub(webAssets, "web/dist")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/runs/{id}/artifacts", s.authorizeWorker(s.uploadArtifact))
	mux.HandleFunc("GET /api/v1/jobs/{id}/artifacts", s.authorizeArtifact(s.listArtifacts))
	mux.HandleFunc("GET /api/v1/artifacts/{id}/content", s.authorizeArtifact(s.artifactContent))
	mux.HandleFunc("GET /api/v1/status", s.status)
	mux.HandleFunc("GET /api/v1/catalog", s.catalog)
	mux.HandleFunc("GET /api/v1/definitions", s.definitions)
	mux.HandleFunc("POST /api/v1/jobs/{id}/{action}", s.authorizeSubmission(s.workflowAction))
	mux.HandleFunc("POST /api/v1/jobs", s.authorizeSubmission(s.linkAttemptsAfter(s.submit)))
	mux.HandleFunc("DELETE /api/v1/jobs/{id}", s.authorizeSubmission(s.deleteJob))
	mux.HandleFunc("POST /api/v1/workers/poll", s.authorizeWorker(s.poll))
	mux.HandleFunc("POST /api/v1/webhooks/issues", s.issueWebhook)
	mux.HandleFunc("GET /api/v1/review-profile", s.getReviewProfile)
	mux.HandleFunc("GET /api/v1/review-profiles", s.listReviewProfiles)
	mux.HandleFunc("PUT /api/v1/review-profiles", s.authorizeSubmission(s.saveReviewProfile))
	mux.HandleFunc("PUT /api/v1/review-profile", s.authorizeSubmission(s.saveProjectReviewProfile))
	mux.HandleFunc("POST /api/v1/review-profile/preview", s.previewReviewProfile)
	mux.HandleFunc("GET /api/v1/review-default-profile", s.getReviewDefaultProfile)
	mux.HandleFunc("PUT /api/v1/review-default-profile", s.authorizeSubmission(s.saveReviewDefaultProfile))
	mux.HandleFunc("POST /api/v1/runs/{id}/heartbeat", s.authorizeWorker(s.heartbeat))
	mux.HandleFunc("POST /api/v1/runs/{id}/complete", s.authorizeWorker(s.complete))
	mux.HandleFunc("POST /api/v1/runs/{id}/events", s.authorizeWorker(s.liveEvents))
	mux.HandleFunc("GET /api/v1/runs/{id}/events", s.authorizeArtifact(s.runEvents))
	// Documented machine API (docs/api.md): scoped bearer tokens required.
	mux.HandleFunc("GET /api/v1/jobs", s.authorizeRead(s.listJobsHandler))
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.authorizeRead(s.getJobHandler))
	mux.HandleFunc("GET /api/v1/runs/{id}", s.authorizeRead(s.getRunHandler))
	mux.HandleFunc("GET /api/v1/settings", s.authorizeRead(s.getSettingsHandler))
	mux.HandleFunc("PUT /api/v1/settings/{key}", s.authorizeWrite(s.putSettingHandler))
	mux.HandleFunc("GET /api/v1/tokens", s.authorizeWrite(s.listTokensHandler))
	mux.HandleFunc("POST /api/v1/tokens", s.authorizeWrite(s.createTokenHandler))
	mux.HandleFunc("DELETE /api/v1/tokens/{id}", s.authorizeWrite(s.revokeTokenHandler))
	mux.HandleFunc("GET /api/v1/audit-log", s.authorizeRead(s.auditLogHandler))
	// Model Context Protocol endpoint (docs/mcp.md), streamable HTTP.
	mux.HandleFunc("POST /mcp", s.handleMCP)
	mux.HandleFunc("GET /mcp", methodNotAllowed)
	mux.HandleFunc("DELETE /mcp", methodNotAllowed)
	mux.Handle("/", http.FileServer(http.FS(dist)))
	return securityHeaders(mux), nil
}

func (s *Server) definitions(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	definition, err := config.LoadDefinitions(s.definitionPath)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	commands := make([]commandDefinitionResponse, 0, len(definition.Commands))
	for _, name := range definition.CommandNames() {
		command, err := definition.ResolveCommand(name)
		if err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
		commands = append(commands, commandDefinitionResponse{Name: command.Name, Executor: command.Executor, Timeout: command.Timeout.String(), Hash: command.Hash, Prompt: command.Prompt})
	}
	workflows := map[string][]workflowStepDefinition{}
	for _, name := range definition.WorkflowNames() {
		steps, err := definition.ResolveTaskWorkflow(name, "")
		if err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
		for _, step := range steps {
			workflows[name] = append(workflows[name], workflowStepDefinition{Name: step.Command.Name, Approval: step.Approval})
		}
	}
	writeJSON(response, http.StatusOK, definitionsResponse{Commands: commands, Workflows: workflows})
}

func (s *Server) status(response http.ResponseWriter, request *http.Request) {
	if err := s.maintainState(request.Context()); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	snapshot, err := s.store.Snapshot(request.Context())
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	definition, err := config.LoadDefinitions(s.definitionPath)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	now := s.store.now().UTC()
	for index := range snapshot.Workers {
		snapshot.Workers[index].Connected = !snapshot.Workers[index].LastSeenAt.Before(now.Add(-workerAvailabilityWindow))
	}
	repositories, repositoryErr := s.store.AvailableRepositories(request.Context(), now.Add(-workerAvailabilityWindow))
	if repositoryErr != nil {
		writeError(response, http.StatusInternalServerError, repositoryErr)
		return
	}
	var maxConcurrentJobs *int
	if s.maxConcurrentJobs > 0 {
		limit := s.maxConcurrentJobs
		maxConcurrentJobs = &limit
	}
	var runningJobs int
	for _, job := range snapshot.Jobs {
		if job.State == "running" {
			runningJobs++
		}
	}
	writeJSON(response, http.StatusOK, statusResponse{
		Snapshot:          snapshot,
		Commands:          definition.CommandNames(),
		Workflows:         definition.WorkflowNames(),
		Repositories:      repositories,
		MaxConcurrentJobs: maxConcurrentJobs,
		RunningJobs:       runningJobs,
		CSRFToken:         s.csrfToken,
	})
}

func (s *Server) catalog(response http.ResponseWriter, request *http.Request) {
	definition, err := config.LoadDefinitions(s.definitionPath)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	repositories, repositoryErr := s.store.KnownRepositories(request.Context())
	if repositoryErr != nil {
		writeError(response, http.StatusInternalServerError, repositoryErr)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, catalogResponse{
		Commands:     definition.CommandNames(),
		Workflows:    definition.WorkflowNames(),
		Repositories: repositories,
	})
}

// badRequestError marks submit validation failures that map to HTTP 400.
type badRequestError struct{ err error }

func (e badRequestError) Error() string { return e.err.Error() }

func (s *Server) submit(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input submitRequest
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	jobID, err := s.queueJob(request.Context(), input, identityFrom(request))
	if err != nil {
		if bad, ok := err.(badRequestError); ok {
			writeError(response, http.StatusBadRequest, bad.err)
			return
		}
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusCreated, map[string]string{"id": jobID})
}

// queueJob validates and stores one submitted job. It is shared by the HTTP
// API and the MCP queue_job tool; validation failures are badRequestError.
func (s *Server) queueJob(ctx context.Context, input submitRequest, identity TokenIdentity) (string, error) {
	bad := func(err error) (string, error) { return "", badRequestError{err} }
	if strings.TrimSpace(input.Repository) == "" {
		return bad(errors.New("repository is required"))
	}
	repositories, repositoryErr := s.store.KnownRepositories(ctx)
	if repositoryErr != nil {
		return "", repositoryErr
	}
	if !slices.Contains(repositories, input.Repository) {
		return bad(fmt.Errorf("repository %q is not defined in the control plane", input.Repository))
	}
	input.Model = strings.TrimSpace(input.Model)
	if len(input.Model) > 128 || strings.ContainsAny(input.Model, "\x00\r\n") {
		return bad(errors.New("model must be at most 128 characters on one line"))
	}
	input.Project = strings.TrimSpace(input.Project)
	project, projectErr := ResolveProjectKey(input.Project, input.Title)
	if projectErr != nil {
		return bad(projectErr)
	}
	if input.Workflow != "" {
		if input.Command != "" {
			return bad(errors.New("choose either workflow or command"))
		}
		definition, err := config.LoadDefinitions(s.definitionPath)
		if err != nil {
			return bad(err)
		}
		if input.Prompt != "" && (input.SourceURL != "" || input.Spec != "") {
			return bad(errors.New("use spec instead of prompt for a task"))
		}
		task := protocol.Task{Title: input.Title, SourceURL: input.SourceURL, Spec: input.Spec}
		// Normalize older clients at the boundary; every new workflow is a task.
		if input.Prompt != "" {
			task.Spec = input.Prompt
		}
		if err := task.Validate(); err != nil {
			return bad(err)
		}
		steps, err := definition.ResolveTaskWorkflow(input.Workflow, input.Model)
		if err != nil {
			return bad(err)
		}
		id, err := s.store.CreateTaskJob(ctx, task, input.Repository, input.Workflow, project, steps)
		if err != nil {
			return "", err
		}
		if err := s.store.RecordAudit(ctx, identity, "jobs.create", input.Repository+" "+input.Workflow); err != nil {
			log.Printf("audit jobs.create: %v", err)
		}
		return id, nil
	}
	if input.SourceURL != "" || input.Spec != "" {
		return bad(errors.New("source_url and spec require a workflow"))
	}
	if strings.TrimSpace(input.Command) == "" {
		return bad(errors.New("command is required"))
	}
	command, err := config.LoadCommand(s.definitionPath, input.Command)
	if err != nil {
		return bad(err)
	}
	command, err = config.RenderPrompt(command, input.Prompt)
	if err != nil {
		return bad(err)
	}
	command.Model = input.Model
	jobID, err := s.store.CreateJobWithOptions(ctx, input.Prompt, input.Repository, input.Command, command, CreateJobOptions{Title: input.Title, Project: project})
	if err != nil {
		return "", err
	}
	if err := s.store.RecordAudit(ctx, identity, "jobs.create", input.Repository+" "+input.Command); err != nil {
		log.Printf("audit jobs.create: %v", err)
	}
	return jobID, nil
}

func (s *Server) deleteJob(response http.ResponseWriter, request *http.Request) {
	err := s.store.DeleteJob(request.Context(), request.PathValue("id"))
	if errors.Is(err, ErrJobActive) {
		writeError(response, http.StatusConflict, err)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(response, http.StatusNotFound, errors.New("job not found"))
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	s.recordAudit(request, "jobs.delete", request.PathValue("id"))
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) poll(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input protocol.PollRequest
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	if strings.TrimSpace(input.InstanceID) == "" || strings.TrimSpace(input.Name) == "" {
		writeError(response, http.StatusBadRequest, errors.New("worker instance_id and name are required"))
		return
	}
	run, err := s.store.poll(request.Context(), input, s.maxConcurrentJobs)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, protocol.PollResponse{Run: run})
}

func (s *Server) complete(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxCompletionBytes) {
		return
	}
	var input protocol.Completion
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	if input.InstanceID == "" || input.LeaseToken == "" {
		writeError(response, http.StatusBadRequest, errors.New("instance_id and lease_token are required"))
		return
	}
	err := s.store.Complete(request.Context(), request.PathValue("id"), input)
	if errors.Is(err, ErrLeaseConflict) || errors.Is(err, ErrRunState) {
		writeError(response, http.StatusConflict, err)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(response, http.StatusNotFound, errors.New("run not found"))
		return
	}
	if errors.Is(err, ErrInvalidCompletion) {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if err != nil {
		log.Printf("complete run %q: %v", request.PathValue("id"), err)
		writeError(response, http.StatusInternalServerError, errors.New("complete run"))
		return
	}
	if alertingState(input.State) {
		s.writeFailAlert(request.Context(), request.PathValue("id"))
	}
	response.WriteHeader(http.StatusNoContent)
}

// writeFailAlert spools the pending fail alert for a failed or timed-out run.
// The run is already terminal when this runs, so alerting is best effort: a
// failed alert write is logged and never fails the completion delivery.
func (s *Server) writeFailAlert(ctx context.Context, runID string) {
	alert, ok, err := s.store.pendingFailAlert(ctx, runID)
	if err != nil {
		log.Printf("collect fail alert for run %q: %v", runID, err)
		return
	}
	if !ok {
		return
	}
	if err := s.failAlerts.write(alert); err != nil {
		log.Printf("write fail alert for run %q: %v", runID, err)
	}
}

func (s *Server) heartbeat(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input protocol.Heartbeat
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	if input.InstanceID == "" || input.LeaseToken == "" {
		writeError(response, http.StatusBadRequest, errors.New("instance_id and lease_token are required"))
		return
	}
	err := s.store.Heartbeat(request.Context(), request.PathValue("id"), input)
	if errors.Is(err, ErrLeaseConflict) || errors.Is(err, ErrRunState) {
		writeError(response, http.StatusConflict, err)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		writeError(response, http.StatusNotFound, errors.New("run not found"))
		return
	}
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	response.WriteHeader(http.StatusNoContent)
}

func (s *Server) authorizeWorker(next http.HandlerFunc) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		if !s.authOff() && !s.validBearerRequest(request) {
			writeUnauthorized(response)
			return
		}
		next(response, request)
	}
}

// authorizeSubmission authenticates job-changing requests. Machine clients
// present a bearer credential: the shared worker token or a write-scope API
// token. Browsers use the same-origin CSRF defense. Handlers see the resolved
// principal for audit logging.
func (s *Server) authorizeSubmission(next http.HandlerFunc) http.HandlerFunc {
	return s.authorizeWrite(next)
}

func (s *Server) validBearerRequest(request *http.Request) bool {
	provided := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
	return provided != request.Header.Get("Authorization") && subtle.ConstantTimeCompare([]byte(provided), []byte(s.workerToken)) == 1
}

// methodNotAllowed answers unsupported MCP transport methods; the streamable
// HTTP transport is served via POST only.
func methodNotAllowed(response http.ResponseWriter, _ *http.Request) {
	response.Header().Set("Allow", "POST")
	writeError(response, http.StatusMethodNotAllowed, errors.New("MCP endpoint supports POST only"))
}

func writeUnauthorized(response http.ResponseWriter) {
	response.Header().Set("WWW-Authenticate", "Bearer")
	writeError(response, http.StatusUnauthorized, errors.New("invalid or missing bearer token"))
}

func (s *Server) validBrowserRequest(request *http.Request) bool {
	if subtle.ConstantTimeCompare([]byte(request.Header.Get("X-Machinist-CSRF")), []byte(s.csrfToken)) != 1 {
		return false
	}
	origin, err := url.Parse(request.Header.Get("Origin"))
	if err != nil || origin.Scheme != "http" || !strings.EqualFold(origin.Host, request.Host) {
		return false
	}
	hostname := origin.Hostname()
	return hostname == "localhost" || net.ParseIP(hostname) != nil && net.ParseIP(hostname).IsLoopback()
}

func decodeJSON(request *http.Request, target any) error {
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err != nil {
			return fmt.Errorf("decode trailing JSON: %w", err)
		}
		return errors.New("request contains multiple JSON values")
	}
	return nil
}

func limitRequestBody(response http.ResponseWriter, request *http.Request, limit int64) bool {
	if request.ContentLength > limit {
		writeError(response, http.StatusRequestEntityTooLarge, errors.New("request body is too large"))
		return false
	}
	request.Body = http.MaxBytesReader(response, request.Body, limit)
	return true
}

func writeJSON(response http.ResponseWriter, status int, body any) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(body)
}

func writeError(response http.ResponseWriter, status int, err error) {
	writeJSON(response, status, map[string]string{"error": err.Error()})
}

func writeDecodeError(response http.ResponseWriter, err error) {
	var maxBytesError *http.MaxBytesError
	if errors.As(err, &maxBytesError) {
		writeError(response, http.StatusRequestEntityTooLarge, errors.New("request body is too large"))
		return
	}
	writeError(response, http.StatusBadRequest, err)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' blob:; media-src 'self' blob:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		response.Header().Set("Referrer-Policy", "no-referrer")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(response, request)
	})
}

func (s *Server) workflowAction(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input struct {
		Feedback               string `json:"feedback"`
		RunID                  string `json:"run_id"`
		PreviousProcessStopped bool   `json:"previous_process_stopped"`
	}
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	if err := s.store.WorkflowAction(request.Context(), request.PathValue("id"), input.RunID, request.PathValue("action"), input.PreviousProcessStopped, input.Feedback); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(response, http.StatusNotFound, err)
			return
		}
		if errors.Is(err, ErrWorkflowAction) {
			writeError(response, http.StatusConflict, err)
			return
		}
		if err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
	}
	s.recordAudit(request, "jobs."+request.PathValue("action"), request.PathValue("id"))
	writeJSON(response, http.StatusOK, map[string]string{"status": "accepted"})
}

// linkAttemptsAfter relinks ticket attempts once a submission is stored, so a
// requeue shows up in the attempt picker immediately.
func (s *Server) linkAttemptsAfter(next http.HandlerFunc) http.HandlerFunc {
	return func(response http.ResponseWriter, request *http.Request) {
		next(response, request)
		if err := s.store.linkTicketAttempts(context.WithoutCancel(request.Context())); err != nil {
			log.Printf("link ticket attempts: %v", err)
		}
	}
}
