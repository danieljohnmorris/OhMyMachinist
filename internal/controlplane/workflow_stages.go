package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/protocol"
)

// Configurable workflow stages. A project defines one or more job types and
// each job type carries an ordered list of stages. The stage shown on a card is
// derived from the job state, the stage the executor last reported, and the
// ticket or pull request resolution. Configuration lives in the database (the
// Settings UI, HTTP API and MCP are views of it); the shipped templates below
// are only defaults for a project that has never saved its own config.

// Stage kinds name the behaviour of a stage. The kind drives derivation and the
// stage strip; the id and name are free-form labels the project chooses.
const (
	StageKindQueued   = "queued"
	StageKindPlanning = "planning"
	StageKindCoding   = "coding"
	StageKindReview   = "review"
	StageKindUAT      = "uat"
	StageKindApply    = "apply"
	StageKindVerify   = "verify"
	StageKindApprove  = "approve"
	StageKindFinished = "finished"
)

// Session modes select how a stage's agent session relates to the previous
// stage: "continue" resumes the same session, "fresh" starts an independent
// one. An empty value is normalized to the kind's default.
const (
	StageSessionContinue = "continue"
	StageSessionFresh    = "fresh"
)

// maxReviewRounds is the built-in review budget: after this many fix rounds a
// job in review needs attention instead of looping forever.
const maxReviewRounds = 3

var stageKinds = []string{
	StageKindQueued, StageKindPlanning, StageKindCoding, StageKindReview,
	StageKindUAT, StageKindApply, StageKindVerify, StageKindApprove, StageKindFinished,
}

// StageDefinition is one ordered step of a job type.
type StageDefinition struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Agent         string `json:"agent,omitempty"`
	Model         string `json:"model,omitempty"`
	Session       string `json:"session,omitempty"`
	HumanApproval bool   `json:"human_approval,omitempty"`
	ReviewProfile string `json:"review_profile,omitempty"`
}

// JobTypeDefinition is a named, ordered stage list.
type JobTypeDefinition struct {
	Name   string            `json:"name"`
	Stages []StageDefinition `json:"stages"`
}

// WorkflowConfig is one project's job types, default type and Plane label
// mapping.
type WorkflowConfig struct {
	DefaultType   string              `json:"default_type"`
	LabelMappings map[string]string   `json:"label_mappings,omitempty"`
	Types         []JobTypeDefinition `json:"types"`
}

// webPRJobType is the shipped standard delivery workflow: a PR opens and its
// built-in review runs, then a human checks the change before it finishes.
func webPRJobType() JobTypeDefinition {
	return JobTypeDefinition{Name: "web_pr", Stages: []StageDefinition{
		{ID: "queued", Name: "Queued", Kind: StageKindQueued},
		{ID: "planning", Name: "Planning", Kind: StageKindPlanning, Session: StageSessionContinue},
		{ID: "coding", Name: "Coding", Kind: StageKindCoding, Session: StageSessionContinue},
		{ID: "review", Name: "PR review", Kind: StageKindReview, Session: StageSessionFresh},
		{ID: "uat", Name: "UAT", Kind: StageKindUAT, HumanApproval: true},
		{ID: "finished", Name: "Finished", Kind: StageKindFinished},
	}}
}

// configOpsJobType is the shipped non-standard workflow for configuration and
// operational changes that need no pull request.
func configOpsJobType() JobTypeDefinition {
	return JobTypeDefinition{Name: "config_ops", Stages: []StageDefinition{
		{ID: "queued", Name: "Queued", Kind: StageKindQueued},
		{ID: "planning", Name: "Planning", Kind: StageKindPlanning, Session: StageSessionContinue},
		{ID: "apply", Name: "Apply", Kind: StageKindApply, Session: StageSessionContinue},
		{ID: "verify", Name: "Verify", Kind: StageKindVerify, Session: StageSessionFresh},
		{ID: "approve", Name: "Approve", Kind: StageKindApprove, HumanApproval: true},
		{ID: "finished", Name: "Finished", Kind: StageKindFinished},
	}}
}

// DefaultWorkflowConfig returns the shipped templates for a project that has
// not saved its own configuration.
func DefaultWorkflowConfig() WorkflowConfig {
	return WorkflowConfig{
		DefaultType:   "web_pr",
		LabelMappings: map[string]string{"type:config": "config_ops"},
		Types:         []JobTypeDefinition{webPRJobType(), configOpsJobType()},
	}
}

// defaultSessionForKind picks a stage's session mode when none is configured:
// review always runs a fresh, independent agent; everything else continues.
func defaultSessionForKind(kind string) string {
	if kind == StageKindReview {
		return StageSessionFresh
	}
	return StageSessionContinue
}

func validStageKind(kind string) bool { return slices.Contains(stageKinds, kind) }

// NormalizeWorkflowConfig fills in derived names and session defaults. An empty
// config normalizes to the shipped templates so a fresh project always has a
// usable definition.
func NormalizeWorkflowConfig(input WorkflowConfig) WorkflowConfig {
	if len(input.Types) == 0 {
		return DefaultWorkflowConfig()
	}
	config := WorkflowConfig{
		DefaultType:   strings.TrimSpace(input.DefaultType),
		LabelMappings: map[string]string{},
		Types:         make([]JobTypeDefinition, 0, len(input.Types)),
	}
	for label, target := range input.LabelMappings {
		label = strings.TrimSpace(label)
		target = strings.TrimSpace(target)
		if label != "" && target != "" {
			config.LabelMappings[label] = target
		}
	}
	for _, jobType := range input.Types {
		normalized := JobTypeDefinition{Name: strings.TrimSpace(jobType.Name)}
		for _, stage := range jobType.Stages {
			stage.ID = strings.TrimSpace(stage.ID)
			stage.Name = strings.TrimSpace(stage.Name)
			stage.Kind = strings.TrimSpace(stage.Kind)
			stage.Agent = strings.TrimSpace(stage.Agent)
			stage.Model = strings.TrimSpace(stage.Model)
			stage.Session = strings.TrimSpace(stage.Session)
			stage.ReviewProfile = strings.TrimSpace(stage.ReviewProfile)
			if stage.Name == "" {
				stage.Name = stage.ID
			}
			if stage.ID == "" {
				stage.ID = stage.Kind
			}
			if stage.Session == "" && stage.Kind != "" {
				stage.Session = defaultSessionForKind(stage.Kind)
			}
			normalized.Stages = append(normalized.Stages, stage)
		}
		config.Types = append(config.Types, normalized)
	}
	if config.DefaultType == "" && len(config.Types) > 0 {
		config.DefaultType = config.Types[0].Name
	}
	return config
}

// ValidateWorkflowConfig checks a normalized config before it is stored. It
// returns the first problem it finds so the API and MCP can reject bad input.
func ValidateWorkflowConfig(config WorkflowConfig) error {
	if len(config.Types) == 0 {
		return errors.New("a project needs at least one job type")
	}
	if len(config.Types) > 32 {
		return errors.New("a project supports at most 32 job types")
	}
	names := map[string]bool{}
	for _, jobType := range config.Types {
		if jobType.Name == "" {
			return errors.New("job type names must not be empty")
		}
		if !validSettingKey(jobType.Name) {
			return fmt.Errorf("job type %q must be letters, digits, dash or underscore", jobType.Name)
		}
		if names[jobType.Name] {
			return fmt.Errorf("duplicate job type %q", jobType.Name)
		}
		names[jobType.Name] = true
		if len(jobType.Stages) == 0 {
			return fmt.Errorf("job type %q needs at least one stage", jobType.Name)
		}
		if len(jobType.Stages) > 32 {
			return fmt.Errorf("job type %q has more than 32 stages", jobType.Name)
		}
		ids := map[string]bool{}
		for _, stage := range jobType.Stages {
			if stage.ID == "" {
				return fmt.Errorf("job type %q has a stage with no id", jobType.Name)
			}
			if ids[stage.ID] {
				return fmt.Errorf("job type %q has duplicate stage %q", jobType.Name, stage.ID)
			}
			ids[stage.ID] = true
			if !validStageKind(stage.Kind) {
				return fmt.Errorf("stage %q has unknown kind %q", stage.ID, stage.Kind)
			}
			if stage.Session != StageSessionContinue && stage.Session != StageSessionFresh {
				return fmt.Errorf("stage %q session must be %q or %q", stage.ID, StageSessionContinue, StageSessionFresh)
			}
			if len(stage.Name) > 64 || len(stage.Model) > 128 {
				return fmt.Errorf("stage %q name or model is too long", stage.ID)
			}
		}
	}
	if config.DefaultType == "" || !names[config.DefaultType] {
		return fmt.Errorf("default job type %q is not defined", config.DefaultType)
	}
	if len(config.LabelMappings) > 64 {
		return errors.New("a project supports at most 64 label mappings")
	}
	for label, target := range config.LabelMappings {
		if label == "" || target == "" {
			return errors.New("label mappings need a label and a job type")
		}
		if !names[target] {
			return fmt.Errorf("label %q maps to unknown job type %q", label, target)
		}
	}
	return nil
}

// Type returns the job type with the given name.
func (c WorkflowConfig) Type(name string) (JobTypeDefinition, bool) {
	for _, jobType := range c.Types {
		if jobType.Name == name {
			return jobType, true
		}
	}
	return JobTypeDefinition{}, false
}

// replaceJobType adds or replaces one job type, preserving order.
func replaceJobType(config WorkflowConfig, jobType JobTypeDefinition) WorkflowConfig {
	for i := range config.Types {
		if config.Types[i].Name == jobType.Name {
			config.Types[i] = jobType
			return config
		}
	}
	config.Types = append(config.Types, jobType)
	return config
}

// removeJobType deletes a job type. It refuses to remove the last type and
// repoints the default and any label mappings at the first remaining type.
func removeJobType(config WorkflowConfig, name string) (WorkflowConfig, error) {
	types := make([]JobTypeDefinition, 0, len(config.Types))
	for _, jobType := range config.Types {
		if jobType.Name != name {
			types = append(types, jobType)
		}
	}
	if len(types) == len(config.Types) {
		return WorkflowConfig{}, fmt.Errorf("job type %q not found", name)
	}
	if len(types) == 0 {
		return WorkflowConfig{}, errors.New("a project needs at least one job type")
	}
	config.Types = types
	if config.DefaultType == name {
		config.DefaultType = types[0].Name
	}
	for label, target := range config.LabelMappings {
		if target == name {
			delete(config.LabelMappings, label)
		}
	}
	return config, nil
}

// Default returns the default job type, falling back to the first defined type.
func (c WorkflowConfig) Default() JobTypeDefinition {
	if jobType, ok := c.Type(c.DefaultType); ok {
		return jobType
	}
	if len(c.Types) > 0 {
		return c.Types[0]
	}
	return JobTypeDefinition{}
}

// StageLabels returns the label questions a job carries, in order. Plane labels
// arrive as "type:config"; GitHub labels are the same free-form strings.
func (c WorkflowConfig) resolvedTypeName(labels []string, explicit string) string {
	if explicit = strings.TrimSpace(explicit); explicit != "" {
		if _, ok := c.Type(explicit); ok {
			return explicit
		}
	}
	for _, label := range labels {
		if target, ok := c.LabelMappings[strings.TrimSpace(label)]; ok {
			if _, ok := c.Type(target); ok {
				return target
			}
		}
	}
	return c.Default().Name
}

// ResolveJobType returns the job type for a card: an explicit type wins, then
// the first matching label mapping, then the project default.
func ResolveJobType(config WorkflowConfig, labels []string, explicit string) (JobTypeDefinition, error) {
	name := config.resolvedTypeName(labels, explicit)
	jobType, ok := config.Type(name)
	if !ok {
		return JobTypeDefinition{}, fmt.Errorf("no job type %q is configured", name)
	}
	return jobType, nil
}

// StageInput is the observable state a stage is derived from.
type StageInput struct {
	State          string
	Stage          string
	ReviewRound    int
	ResolutionKind string
}

// StageStatus is one entry of a card's stage strip.
type StageStatus struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
	Round int    `json:"round,omitempty"`
}

// StageProgress is the derived stage strip for one job.
type StageProgress struct {
	JobType   string        `json:"job_type"`
	Current   string        `json:"current,omitempty"`
	Round     int           `json:"round,omitempty"`
	Attention bool          `json:"attention,omitempty"`
	Stages    []StageStatus `json:"stages"`
}

// DeriveStage maps job state, the executor's last reported stage and the
// terminal resolution to a position in the job type's ordered stage list. The
// finished stage is only reached on a merged PR or a Done ticket: a succeeded
// PR job waits in UAT, not Finished.
func DeriveStage(jobType JobTypeDefinition, in StageInput) StageProgress {
	progress := StageProgress{JobType: jobType.Name}
	for _, stage := range jobType.Stages {
		progress.Stages = append(progress.Stages, StageStatus{ID: stage.ID, Name: stage.Name})
	}
	if len(progress.Stages) == 0 {
		return progress
	}
	index := deriveStageIndex(jobType, in)
	if index < 0 {
		index = 0
	}
	progress.Current = jobType.Stages[index].ID
	for i := range progress.Stages {
		switch {
		case i < index:
			progress.Stages[i].State = "complete"
		case i == index:
			if jobType.Stages[i].Kind == StageKindFinished {
				progress.Stages[i].State = "complete"
			} else {
				progress.Stages[i].State = "current"
			}
		default:
			progress.Stages[i].State = "pending"
		}
	}
	if jobType.Stages[index].Kind == StageKindReview {
		round := in.ReviewRound
		if round < 1 {
			round = 1
		}
		progress.Round = round
		progress.Stages[index].Round = round
		if round > maxReviewRounds {
			progress.Attention = true
			progress.Stages[index].State = "attention"
		}
	}
	switch in.State {
	case "blocked", "awaiting_approval", "interrupted", "failed", "timed_out", "cancelled":
		progress.Attention = true
		if jobType.Stages[index].Kind != StageKindFinished {
			progress.Stages[index].State = "attention"
		}
	}
	return progress
}

func deriveStageIndex(jobType JobTypeDefinition, in StageInput) int {
	indexOfKind := func(kind string) int {
		for i, stage := range jobType.Stages {
			if stage.Kind == kind {
				return i
			}
		}
		return -1
	}
	indexOf := func(stage string) int {
		stage = strings.TrimSpace(stage)
		if stage == "" {
			return -1
		}
		for i, candidate := range jobType.Stages {
			if candidate.ID == stage || candidate.Kind == stage {
				return i
			}
		}
		return -1
	}
	// defaultRunning mirrors the missing-stage fallback: prefer the build stage,
	// otherwise the first stage that is neither queued nor finished.
	defaultRunning := func() int {
		for _, kind := range []string{StageKindCoding, StageKindApply, StageKindReview, StageKindVerify} {
			if index := indexOfKind(kind); index >= 0 {
				return index
			}
		}
		for i, stage := range jobType.Stages {
			if stage.Kind != StageKindQueued && stage.Kind != StageKindFinished {
				return i
			}
		}
		return 0
	}
	switch in.State {
	case "queued":
		if index := indexOfKind(StageKindQueued); index >= 0 {
			return index
		}
		return 0
	case "resolved":
		if index := indexOfKind(StageKindFinished); index >= 0 {
			return index
		}
		return len(jobType.Stages) - 1
	case "succeeded":
		// Review passed. A job with a human stage waits there until the ticket
		// or PR resolves; only then does it reach Finished.
		for _, kind := range []string{StageKindUAT, StageKindApprove} {
			if index := indexOfKind(kind); index >= 0 {
				return index
			}
		}
		if index := indexOfKind(StageKindFinished); index >= 0 {
			return index
		}
		return len(jobType.Stages) - 1
	case "running":
		if index := indexOf(in.Stage); index >= 0 {
			return index
		}
		return defaultRunning()
	default:
		// blocked, awaiting_approval, interrupted, failed, timed_out, cancelled.
		if index := indexOf(in.Stage); index >= 0 {
			return index
		}
		return defaultRunning()
	}
}

const projectWorkflowSchema = `
CREATE TABLE IF NOT EXISTS project_workflows (
 project TEXT PRIMARY KEY,
 config TEXT NOT NULL,
 updated_at TEXT NOT NULL);`

// ProjectWorkflowConfig returns a project's saved workflow config, or the
// shipped defaults when it has none. The result is always normalized.
func (s *Store) ProjectWorkflowConfig(ctx context.Context, project string) (WorkflowConfig, error) {
	configs, err := s.projectWorkflowConfigs(ctx)
	if err != nil {
		return WorkflowConfig{}, err
	}
	if config, ok := configs[strings.ToLower(strings.TrimSpace(project))]; ok {
		return config, nil
	}
	return DefaultWorkflowConfig(), nil
}

// projectWorkflowConfigs reads every saved project config, keyed case-folded.
func (s *Store) projectWorkflowConfigs(ctx context.Context) (map[string]WorkflowConfig, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT project, config FROM project_workflows`)
	if err != nil {
		return nil, fmt.Errorf("read project workflows: %w", err)
	}
	defer rows.Close()
	configs := map[string]WorkflowConfig{}
	for rows.Next() {
		var project, body string
		if err := rows.Scan(&project, &body); err != nil {
			return nil, fmt.Errorf("read project workflow: %w", err)
		}
		var config WorkflowConfig
		if err := json.Unmarshal([]byte(body), &config); err != nil {
			return nil, fmt.Errorf("decode project workflow %q: %w", project, err)
		}
		configs[project] = NormalizeWorkflowConfig(config)
	}
	return configs, rows.Err()
}

// SaveProjectWorkflowConfig validates and stores a project's config. The
// config is normalized first so derived fields are stable across readers.
func (s *Store) SaveProjectWorkflowConfig(ctx context.Context, project string, config WorkflowConfig) (WorkflowConfig, error) {
	project = strings.ToLower(strings.TrimSpace(project))
	if project == "" {
		return WorkflowConfig{}, errors.New("project is required")
	}
	normalized := NormalizeWorkflowConfig(config)
	if err := ValidateWorkflowConfig(normalized); err != nil {
		return WorkflowConfig{}, err
	}
	body, err := json.Marshal(normalized)
	if err != nil {
		return WorkflowConfig{}, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO project_workflows(project,config,updated_at) VALUES(?,?,?)
ON CONFLICT(project) DO UPDATE SET config=excluded.config,updated_at=excluded.updated_at`,
		project, string(body), s.now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return WorkflowConfig{}, fmt.Errorf("save project workflow: %w", err)
	}
	return normalized, nil
}

// DeleteProjectWorkflowConfig resets a project to the shipped templates.
func (s *Store) DeleteProjectWorkflowConfig(ctx context.Context, project string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM project_workflows WHERE project=?`, strings.ToLower(strings.TrimSpace(project)))
	return err
}

// recordStageMetadata persists the workflow stage an executor reported in its
// step result. Best-effort: a malformed or invalid stage never fails a run that
// has already completed. It only writes the stage fields, so unrelated metadata
// from a plain command is not introduced.
func recordStageMetadata(ctx context.Context, tx *sql.Tx, jobID string, result json.RawMessage) {
	if len(result) == 0 {
		return
	}
	var envelope struct {
		StepResult json.RawMessage `json:"step_result"`
	}
	if json.Unmarshal(result, &envelope) != nil || len(envelope.StepResult) == 0 {
		return
	}
	stage, err := protocol.ParseStepResult(envelope.StepResult)
	if err != nil {
		return
	}
	metadata := map[string]any{}
	for key, value := range stage.StageMetadata() {
		if text, ok := value.(string); ok {
			if !validSettingKey(text) {
				continue
			}
			metadata[key] = text
			continue
		}
		metadata[key] = value
	}
	if len(metadata) == 0 {
		return
	}
	_ = saveJobMetadata(ctx, tx, jobID, metadata)
}

// SetJobType records the job type chosen at intake on a job's metadata. The
// executor's later stage results preserve it.
func (s *Store) SetJobType(ctx context.Context, jobID, jobType string) error {
	jobType = strings.TrimSpace(jobType)
	if jobType == "" {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := saveJobMetadata(ctx, tx, jobID, map[string]any{"job_type": jobType}); err != nil {
		return err
	}
	return tx.Commit()
}

// loadJobStages attaches the derived stage strip and job type to each job. The
// per-project configs are read once per listing, not per job.
func (s *Store) loadJobStages(ctx context.Context, jobs []Job) error {
	configs, err := s.projectWorkflowConfigs(ctx)
	if err != nil {
		return err
	}
	for i := range jobs {
		project := strings.ToLower(strings.TrimSpace(jobs[i].Repository))
		config, ok := configs[project]
		if !ok {
			config = DefaultWorkflowConfig()
		}
		explicit := ""
		stage := ""
		round := 0
		if metadata := jobs[i].Metadata; metadata != nil {
			explicit = metadata.JobType
			stage = metadata.Stage
			round = metadata.ReviewRound
		}
		jobType, err := ResolveJobType(config, nil, explicit)
		if err != nil {
			continue
		}
		jobs[i].JobType = jobType.Name
		progress := DeriveStage(jobType, StageInput{
			State:          jobs[i].State,
			Stage:          stage,
			ReviewRound:    round,
			ResolutionKind: jobs[i].ResolutionKind,
		})
		jobs[i].Stage = &progress
	}
	return nil
}

var errWorkflowProject = errors.New("project must be a repository name or owner/name")

// workflowConfigResponse is the GET/PUT body for a project's workflow config.
type workflowConfigResponse struct {
	Project string         `json:"project"`
	Config  WorkflowConfig `json:"config"`
	Default bool           `json:"default"`
}

// getWorkflowHandler serves GET /api/v1/workflows?project=X.
func (s *Server) getWorkflowHandler(response http.ResponseWriter, request *http.Request) {
	project := strings.TrimSpace(request.URL.Query().Get("project"))
	if project != "" && !reviewProjectPattern.MatchString(project) {
		writeError(response, http.StatusBadRequest, errWorkflowProject)
		return
	}
	key := s.resolveReviewProject(project)
	stored, err := s.store.ProjectWorkflowConfig(request.Context(), key)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	_, saved, err := s.store.hasProjectWorkflow(request.Context(), key)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, workflowConfigResponse{Project: key, Config: stored, Default: !saved})
}

// putWorkflowHandler serves PUT /api/v1/workflows?project=X with a workflow
// config body. It validates, stores and audits the change.
func (s *Server) putWorkflowHandler(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	project := strings.TrimSpace(request.URL.Query().Get("project"))
	if project == "" || !reviewProjectPattern.MatchString(project) {
		writeError(response, http.StatusBadRequest, errWorkflowProject)
		return
	}
	var input WorkflowConfig
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	key := s.resolveReviewProject(project)
	saved, err := s.store.SaveProjectWorkflowConfig(request.Context(), key, input)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	s.recordAudit(request, "workflows.write", key)
	writeJSON(response, http.StatusOK, workflowConfigResponse{Project: key, Config: saved})
}

// hasProjectWorkflow reports whether a project has saved its own config.
func (s *Store) hasProjectWorkflow(ctx context.Context, project string) (string, bool, error) {
	var stored string
	err := s.db.QueryRowContext(ctx, `SELECT project FROM project_workflows WHERE project=?`, strings.ToLower(strings.TrimSpace(project))).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return stored, true, nil
}

// jobTypesResponse is the GET /api/v1/job-types body.
type jobTypesResponse struct {
	Project     string              `json:"project"`
	JobTypes    []JobTypeDefinition `json:"job_types"`
	DefaultType string              `json:"default_type"`
}

// getJobTypesHandler serves GET /api/v1/job-types?project=X.
func (s *Server) getJobTypesHandler(response http.ResponseWriter, request *http.Request) {
	project := strings.TrimSpace(request.URL.Query().Get("project"))
	if project != "" && !reviewProjectPattern.MatchString(project) {
		writeError(response, http.StatusBadRequest, errWorkflowProject)
		return
	}
	key := s.resolveReviewProject(project)
	config, err := s.store.ProjectWorkflowConfig(request.Context(), key)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, jobTypesResponse{Project: key, JobTypes: config.Types, DefaultType: config.DefaultType})
}

// putJobTypeHandler serves PUT /api/v1/job-types?project=X with one job type
// body {name, stages}, adding or replacing that type.
func (s *Server) putJobTypeHandler(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	project := strings.TrimSpace(request.URL.Query().Get("project"))
	if project == "" || !reviewProjectPattern.MatchString(project) {
		writeError(response, http.StatusBadRequest, errWorkflowProject)
		return
	}
	var input JobTypeDefinition
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	key := s.resolveReviewProject(project)
	config, err := s.store.ProjectWorkflowConfig(request.Context(), key)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		writeError(response, http.StatusBadRequest, errors.New("job type name is required"))
		return
	}
	input.Name = name
	config = replaceJobType(config, input)
	saved, err := s.store.SaveProjectWorkflowConfig(request.Context(), key, config)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	s.recordAudit(request, "job_types.write", key+"/"+name)
	writeJSON(response, http.StatusOK, jobTypesResponse{Project: key, JobTypes: saved.Types, DefaultType: saved.DefaultType})
}

// deleteJobTypeHandler serves DELETE /api/v1/job-types?project=X&name=Y. A job
// type in use as the default cannot be removed.
func (s *Server) deleteJobTypeHandler(response http.ResponseWriter, request *http.Request) {
	project := strings.TrimSpace(request.URL.Query().Get("project"))
	if project == "" || !reviewProjectPattern.MatchString(project) {
		writeError(response, http.StatusBadRequest, errWorkflowProject)
		return
	}
	name := strings.TrimSpace(request.URL.Query().Get("name"))
	if name == "" {
		writeError(response, http.StatusBadRequest, errors.New("job type name is required"))
		return
	}
	key := s.resolveReviewProject(project)
	config, err := s.store.ProjectWorkflowConfig(request.Context(), key)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	updated, err := removeJobType(config, name)
	if err != nil {
		writeError(response, http.StatusNotFound, err)
		return
	}
	saved, err := s.store.SaveProjectWorkflowConfig(request.Context(), key, updated)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	s.recordAudit(request, "job_types.delete", key+"/"+name)
	writeJSON(response, http.StatusOK, jobTypesResponse{Project: key, JobTypes: saved.Types, DefaultType: saved.DefaultType})
}
