package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/adapters"
	"github.com/owainlewis/machinist/internal/config"
	"github.com/owainlewis/machinist/internal/issues"
)

// settingFactoryDefault holds the instance-wide default factory setup as JSON,
// used when a project has no override of its own.
const settingFactoryDefault = "factory.default"

// factorySettingResponse is the resolved setup for one project (or the global
// default), with the layer it resolved from.
type factorySettingResponse struct {
	Project string                  `json:"project,omitempty"`
	Setting adapters.FactorySetting `json:"setting"`
	Source  string                  `json:"source"`
}

// factoryCatalogResponse lists the adapters and roles the Settings editor may
// offer.
type factoryCatalogResponse struct {
	Trackers  []string `json:"trackers"`
	Agents    []string `json:"agents"`
	Roles     []string `json:"roles"`
	Endpoints []string `json:"endpoints"`
}

// workerFactorySettingResponse is what a worker needs to apply per-project
// choices it cannot see from a run spec: the rendered review executor command.
type workerFactorySettingResponse struct {
	Source         string `json:"source"`
	ReviewExecutor string `json:"review_executor"`
	CodingExecutor string `json:"coding_executor"`
	CodingModel    string `json:"coding_model"`
}

// ---------------------------------------------------------------------------
// Store

// ProjectFactorySetting reads one project's stored factory setup.
func (s *Store) ProjectFactorySetting(ctx context.Context, repository string) (adapters.FactorySetting, bool, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT setting FROM project_factory_settings WHERE repository = ?`, repository).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return adapters.FactorySetting{}, false, nil
	}
	if err != nil {
		return adapters.FactorySetting{}, false, err
	}
	setting, err := decodeFactorySetting(raw)
	if err != nil {
		return adapters.FactorySetting{}, false, err
	}
	return setting, true, nil
}

// SaveProjectFactorySetting stores one project's factory setup as JSON.
func (s *Store) SaveProjectFactorySetting(ctx context.Context, repository string, setting adapters.FactorySetting) error {
	body, err := json.Marshal(adapters.Normalize(setting))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO project_factory_settings(repository, setting, updated_at) VALUES(?,?,?)
ON CONFLICT(repository) DO UPDATE SET setting=excluded.setting, updated_at=excluded.updated_at`,
		repository, string(body), s.now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("save project factory setting: %w", err)
	}
	return nil
}

// DeleteProjectFactorySetting removes a project's override so it falls back to
// the global default.
func (s *Store) DeleteProjectFactorySetting(ctx context.Context, repository string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM project_factory_settings WHERE repository = ?`, repository); err != nil {
		return fmt.Errorf("delete project factory setting: %w", err)
	}
	return nil
}

func decodeFactorySetting(raw string) (adapters.FactorySetting, error) {
	if strings.TrimSpace(raw) == "" {
		return adapters.FactorySetting{}, nil
	}
	var setting adapters.FactorySetting
	if err := json.Unmarshal([]byte(raw), &setting); err != nil {
		return adapters.FactorySetting{}, fmt.Errorf("decode factory setting: %w", err)
	}
	return adapters.Normalize(setting), nil
}

// ---------------------------------------------------------------------------
// Resolution

// resolveFactorySetting computes the effective setup for a repository. First
// match wins: project setting in Settings > global default in Settings >
// per-project config > config default > built-in (empty, meaning the trigger
// and command keep their own agent and model).
func (s *Server) resolveFactorySetting(ctx context.Context, repository string) (adapters.FactorySetting, string, error) {
	if strings.TrimSpace(repository) != "" {
		if setting, ok, err := s.store.ProjectFactorySetting(ctx, repository); err != nil {
			return adapters.FactorySetting{}, "", err
		} else if ok {
			return setting, "project", nil
		}
	}
	if value, ok, err := s.store.Setting(ctx, settingFactoryDefault); err != nil {
		return adapters.FactorySetting{}, "", err
	} else if ok {
		setting, err := decodeFactorySetting(value)
		if err != nil {
			return adapters.FactorySetting{}, "", err
		}
		return setting, "global", nil
	}
	definition, err := s.loadDefinition()
	if err != nil {
		return adapters.FactorySetting{}, "", err
	}
	if strings.TrimSpace(repository) != "" {
		if setting, ok := definition.Factory.Projects[repository]; ok {
			return adapters.Normalize(setting), "config project", nil
		}
	}
	if !definition.Factory.Default.IsZero() {
		return adapters.Normalize(definition.Factory.Default), "config default", nil
	}
	return adapters.FactorySetting{}, "built-in", nil
}

// loadDefinition reads the control-plane config, or returns an empty definition
// when no path is configured (as in unit tests and external intakes).
func (s *Server) loadDefinition() (config.Config, error) {
	if strings.TrimSpace(s.definitionPath) == "" {
		return config.Config{}, nil
	}
	return config.LoadDefinitions(s.definitionPath)
}

// applyFactoryToCommand overrides a resolved command's executor and model with
// the repository's factory setup. Unset fields leave the trigger's own choice
// untouched, so an empty setup preserves the wrapped current behaviour.
func (s *Server) applyFactoryToCommand(ctx context.Context, repository string, command *config.ResolvedCommand) error {
	setting, _, err := s.resolveFactorySetting(ctx, repository)
	if err != nil {
		return err
	}
	if setting.CodingAgent.Executor != "" {
		command.Executor = setting.CodingAgent.Executor
	}
	if model := setting.Model(adapters.RoleCoding).Model; model != "" {
		command.Model = model
	}
	return nil
}

// issuePollerAdapter gives a Tracker the poll/comment shape the scheduler uses.
type issuePollerAdapter struct {
	tracker adapters.Tracker
}

func (a issuePollerAdapter) Poll(ctx context.Context, label string) ([]issues.Issue, error) {
	return a.tracker.Intake(ctx, label)
}

func (a issuePollerAdapter) PostComment(ctx context.Context, key, body string) error {
	return a.tracker.Comment(ctx, key, body)
}

// trackerSourceFor returns the per-project tracker override when the project's
// factory setup names a tracker. The second result is false when the project
// keeps the trigger's configured source.
func (s *Server) trackerSourceFor(ctx context.Context, repository string) (issuePoller, bool, error) {
	setting, _, err := s.resolveFactorySetting(ctx, repository)
	if err != nil {
		return nil, false, err
	}
	if setting.Tracker == "" {
		return nil, false, nil
	}
	build := s.trackerFactory
	if build == nil {
		build = s.trackerFor
	}
	tracker, err := build(ctx, repository, setting.Tracker)
	if err != nil {
		return nil, false, err
	}
	return issuePollerAdapter{tracker: tracker}, true, nil
}

// trackerFor builds the tracker adapter a project's setup names, reusing the
// configured issue-source connections and their environment-held secrets.
func (s *Server) trackerFor(ctx context.Context, repository, kind string) (adapters.Tracker, error) {
	definition, err := s.loadDefinition()
	if err != nil {
		return nil, err
	}
	switch kind {
	case adapters.TrackerPlane:
		connection, ok := definition.IssueSources.PlaneProjects()[repository]
		if !ok {
			return nil, fmt.Errorf("project %q selects the Plane tracker but has no [issue_sources.plane] entry", repository)
		}
		client, err := issues.NewPlaneConnection(connection)
		if err != nil {
			return nil, err
		}
		return adapters.PlaneTracker{Client: client}, nil
	case adapters.TrackerLinear:
		connection, ok := definition.IssueSources.LinearProjects()[repository]
		if !ok {
			return nil, fmt.Errorf("project %q selects the Linear tracker but has no [issue_sources.linear] entry", repository)
		}
		client, err := issues.NewLinearConnection(connection)
		if err != nil {
			return nil, err
		}
		return adapters.LinearTracker{Client: client}, nil
	default:
		return nil, fmt.Errorf("project %q selects unknown tracker %q", repository, kind)
	}
}

// workerFactorySetting renders what a worker needs for one repository: the
// review executor command (with {{env.NAME}} placeholders intact so secrets
// resolve from the worker's environment) and the coding executor and model.
func (s *Server) workerFactorySetting(ctx context.Context, repository string) (workerFactorySettingResponse, error) {
	setting, source, err := s.resolveFactorySetting(ctx, repository)
	if err != nil {
		return workerFactorySettingResponse{}, err
	}
	response := workerFactorySettingResponse{Source: source}
	if agent, ok := adapters.AgentFor(setting.ReviewAgent.Kind); ok {
		response.ReviewExecutor = strings.TrimSpace(agent.ReviewCommand(setting.ReviewAgent, setting.Model(adapters.RoleReview)))
	}
	response.CodingExecutor = setting.CodingAgent.Executor
	response.CodingModel = setting.Model(adapters.RoleCoding).Model
	return response, nil
}

// ---------------------------------------------------------------------------
// HTTP handlers

func (s *Server) getFactorySetting(response http.ResponseWriter, request *http.Request) {
	s.writeFactorySetting(response, request, strings.TrimSpace(request.URL.Query().Get("project")))
}

// writeFactorySetting resolves and writes the setup for one project.
func (s *Server) writeFactorySetting(response http.ResponseWriter, request *http.Request, project string) {
	setting, source, err := s.resolveFactorySetting(request.Context(), project)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, factorySettingResponse{Project: project, Setting: setting, Source: source})
}

type saveFactorySettingRequest struct {
	Project string                  `json:"project"`
	Setting adapters.FactorySetting `json:"setting"`
}

func (s *Server) saveFactorySetting(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input saveFactorySettingRequest
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	input.Project = strings.TrimSpace(input.Project)
	if input.Project == "" {
		writeError(response, http.StatusBadRequest, errors.New("project is required"))
		return
	}
	setting := adapters.Normalize(input.Setting)
	if err := adapters.Validate(setting); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	if setting.IsZero() {
		if err := s.store.DeleteProjectFactorySetting(request.Context(), input.Project); err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
	} else if err := s.store.SaveProjectFactorySetting(request.Context(), input.Project, setting); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	s.recordAudit(request, "factory.write", input.Project)
	s.writeFactorySetting(response, request, input.Project)
}

func (s *Server) deleteFactorySetting(response http.ResponseWriter, request *http.Request) {
	project := strings.TrimSpace(request.URL.Query().Get("project"))
	if project == "" {
		writeError(response, http.StatusBadRequest, errors.New("project is required"))
		return
	}
	if err := s.store.DeleteProjectFactorySetting(request.Context(), project); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	s.recordAudit(request, "factory.reset", project)
	s.getFactorySetting(response, request)
}

func (s *Server) getFactoryDefault(response http.ResponseWriter, request *http.Request) {
	value, ok, err := s.store.Setting(request.Context(), settingFactoryDefault)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	setting := adapters.FactorySetting{}
	source := "built-in"
	if ok {
		if setting, err = decodeFactorySetting(value); err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
		source = "global"
	} else {
		definition, err := s.loadDefinition()
		if err != nil {
			writeError(response, http.StatusInternalServerError, err)
			return
		}
		if !definition.Factory.Default.IsZero() {
			setting, source = adapters.Normalize(definition.Factory.Default), "config default"
		}
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, factorySettingResponse{Setting: setting, Source: source})
}

func (s *Server) saveFactoryDefault(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input saveFactorySettingRequest
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	setting := adapters.Normalize(input.Setting)
	if err := adapters.Validate(setting); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	body, err := json.Marshal(setting)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	if err := s.store.SaveSetting(request.Context(), settingFactoryDefault, string(body)); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	s.recordAudit(request, "factory.default.write", "")
	s.getFactoryDefault(response, request)
}

func (s *Server) getFactoryCatalog(response http.ResponseWriter, request *http.Request) {
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, factoryCatalogResponse{
		Trackers:  adapters.TrackerKinds(),
		Agents:    adapters.AgentKinds(),
		Roles:     adapters.Roles(),
		Endpoints: adapters.Endpoints(),
	})
}

// getWorkerFactorySetting serves GET /api/v1/worker/factory-setting?project=...
// so a worker can apply a project's review agent to the runs it executes.
func (s *Server) getWorkerFactorySetting(response http.ResponseWriter, request *http.Request) {
	project := strings.TrimSpace(request.URL.Query().Get("project"))
	result, err := s.workerFactorySetting(request.Context(), project)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, result)
}
