package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/owainlewis/machinist/internal/skills"
)

// The Model Context Protocol endpoint exposes the documented API as tools
// over the streamable HTTP transport. Clients POST JSON-RPC messages to
// /mcp with `Authorization: Bearer <token>`; responses are single JSON
// objects (the JSON response mode of the transport). See docs/mcp.md.

const mcpProtocolVersion = "2025-03-26"

// mcpRequest is one incoming JSON-RPC message.
type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// mcpResponse is one outgoing JSON-RPC message.
type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// JSON-RPC and MCP error codes.
const (
	mcpParseError     = -32700
	mcpInvalidRequest = -32600
	mcpMethodNotFound = -32601
	mcpInvalidParams  = -32602
	mcpInternalError  = -32603
)

// mcpTool describes one tool for tools/list.
type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// mcpWriteTools names the tools that mutate state and therefore need a
// write-scope token. Everything else is read scope.
var mcpWriteTools = map[string]bool{
	"queue_job": true, "cancel_job": true, "requeue_job": true, "set_setting": true,
	"install_skill": true, "update_skill": true, "pin_skill": true, "uninstall_skill": true,
	"start_factory": true, "stop_factory": true, "set_job_cap": true, "set_lane_enabled": true,
}

func mcpSchema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required}
}

// mcpToolList is the documented tool catalogue served by tools/list. Keep in
// sync with docs/mcp.md.
var mcpToolList = []mcpTool{
	{Name: "list_jobs", Description: "List Machinist jobs newest first, with their runs. Optional state filter (queued, running, succeeded, failed, cancelled).", InputSchema: mcpSchema(map[string]any{"state": map[string]any{"type": "string", "description": "Only return jobs in this state"}})},
	{Name: "get_job", Description: "Get one job by id, including its runs and attempt metadata.", InputSchema: mcpSchema(map[string]any{"id": map[string]any{"type": "string"}}, "id")},
	{Name: "get_run", Description: "Get one run by id, including exit code, timing and final message.", InputSchema: mcpSchema(map[string]any{"id": map[string]any{"type": "string"}}, "id")},
	{Name: "get_transcript", Description: "Get the event transcript of a run (worker output log).", InputSchema: mcpSchema(map[string]any{"run_id": map[string]any{"type": "string"}}, "run_id")},
	{Name: "get_board", Description: "Get the full board state: jobs, workers and triggers.", InputSchema: mcpSchema(map[string]any{})},
	{Name: "list_review_profiles", Description: "List stored review profiles with their YAML bodies.", InputSchema: mcpSchema(map[string]any{})},
	{Name: "get_settings", Description: "Read all stored settings key/value pairs.", InputSchema: mcpSchema(map[string]any{})},
	{Name: "get_factory", Description: "Read the factory controls: dispatch state (running, paused or draining), the job cap and one slot per lane.", InputSchema: mcpSchema(map[string]any{})},
	{Name: "start_factory", Description: "Resume dispatch after a pause or drain.", InputSchema: mcpSchema(map[string]any{})},
	{Name: "stop_factory", Description: "Stop dispatching new jobs. mode=drain lets the running job finish; mode=cancel stops it now.", InputSchema: mcpSchema(map[string]any{
		"mode": map[string]any{"type": "string", "enum": []string{"drain", "cancel"}}, "reason": map[string]any{"type": "string"},
	})},
	{Name: "set_job_cap", Description: "Set how many jobs run at once. The value is clamped to the number of enabled connected lanes.", InputSchema: mcpSchema(map[string]any{"value": map[string]any{"type": "integer"}}, "value")},
	{Name: "set_lane_enabled", Description: "Enable or disable one lane by name. A disabled lane stays connected but gets no new jobs.", InputSchema: mcpSchema(map[string]any{
		"lane": map[string]any{"type": "string"}, "enabled": map[string]any{"type": "boolean"},
	}, "lane", "enabled")},
	{Name: "queue_job", Description: "Queue a job. Either command (single command) or workflow (task workflow) plus repository is required.", InputSchema: mcpSchema(map[string]any{
		"title": map[string]any{"type": "string"}, "spec": map[string]any{"type": "string"},
		"source_url": map[string]any{"type": "string"}, "workflow": map[string]any{"type": "string"},
		"command": map[string]any{"type": "string"}, "prompt": map[string]any{"type": "string"},
		"repository": map[string]any{"type": "string"}, "model": map[string]any{"type": "string"},
		"project": map[string]any{"type": "string"},
	}, "repository")},
	{Name: "cancel_job", Description: "Cancel a job and its latest run.", InputSchema: mcpSchema(map[string]any{"job_id": map[string]any{"type": "string"}}, "job_id")},
	{Name: "requeue_job", Description: "Requeue (retry) a failed, blocked, interrupted or cancelled job. Set previous_process_stopped=true to confirm the prior process is stopped.", InputSchema: mcpSchema(map[string]any{
		"job_id": map[string]any{"type": "string"}, "previous_process_stopped": map[string]any{"type": "boolean"},
	}, "job_id")},
	{Name: "get_setting", Description: "Read one stored setting by key.", InputSchema: mcpSchema(map[string]any{"key": map[string]any{"type": "string"}}, "key")},
	{Name: "set_setting", Description: "Write one settings key/value pair.", InputSchema: mcpSchema(map[string]any{
		"key": map[string]any{"type": "string"}, "value": map[string]any{"type": "string"},
	}, "key", "value")},
	{Name: "list_skills", Description: "List installed skills with their versions and the projects or review profiles that reference them.", InputSchema: mcpSchema(map[string]any{})},
	{Name: "get_skill", Description: "Get one installed skill with all of its versions.", InputSchema: mcpSchema(map[string]any{"name": map[string]any{"type": "string"}}, "name")},
	{Name: "install_skill", Description: "Install a skill from a GitHub URL, a local directory or a zip archive.", InputSchema: mcpSchema(map[string]any{"source": map[string]any{"type": "string", "description": "GitHub URL, local skill directory, or zip path"}}, "source")},
	{Name: "update_skill", Description: "Re-resolve a skill's recorded source and add the new version. Set activate=true to make it active.", InputSchema: mcpSchema(map[string]any{
		"name": map[string]any{"type": "string"}, "activate": map[string]any{"type": "boolean"},
	}, "name")},
	{Name: "pin_skill", Description: "Set the active version of an installed skill to any installed version.", InputSchema: mcpSchema(map[string]any{
		"name": map[string]any{"type": "string"}, "version": map[string]any{"type": "string"},
	}, "name", "version")},
	{Name: "uninstall_skill", Description: "Remove a skill and every installed version. Set force=true to remove a skill a review profile still references.", InputSchema: mcpSchema(map[string]any{
		"name": map[string]any{"type": "string"}, "force": map[string]any{"type": "boolean"},
	}, "name")},
}

// handleMCP serves the streamable HTTP MCP transport.
func (s *Server) handleMCP(response http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(response, request.Body, maxRequestBytes))
	if err != nil {
		writeError(response, http.StatusBadRequest, errors.New("request body is too large"))
		return
	}
	var msg mcpRequest
	if err := json.Unmarshal(body, &msg); err != nil {
		s.writeMCP(response, nil, &mcpError{Code: mcpParseError, Message: "parse error"})
		return
	}
	// Notifications carry no id and expect no response body.
	if msg.ID == nil {
		response.WriteHeader(http.StatusAccepted)
		return
	}
	identity, scope, authenticated := s.authenticate(request)
	authorized := s.authOff() || authenticated
	if !authorized {
		writeUnauthorized(response)
		return
	}
	if !authenticated {
		// Auth-off servers accept calls without a credential; audit them as
		// the anonymous principal like the HTTP path instead of an empty name.
		identity = anonymous
	}
	switch msg.Method {
	case "initialize":
		s.writeMCP(response, msg.ID, map[string]any{
			"protocolVersion": mcpProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "machinist", "version": "1.0"},
		})
	case "ping":
		s.writeMCP(response, msg.ID, map[string]any{})
	case "tools/list":
		s.writeMCP(response, msg.ID, map[string]any{"tools": mcpToolList})
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			s.writeMCPError(response, msg.ID, mcpInvalidParams, "invalid tools/call params")
			return
		}
		if !s.authOff() {
			if mcpWriteTools[params.Name] && scope != TokenScopeWrite {
				s.writeMCPToolError(response, msg.ID, "this tool requires a write-scope token")
				return
			}
			if scope != TokenScopeRead && scope != TokenScopeWrite {
				s.writeMCPToolError(response, msg.ID, "token scope is not recognized")
				return
			}
		}
		s.callMCPTool(response, request, msg.ID, identity, params.Name, params.Arguments)
	default:
		s.writeMCPError(response, msg.ID, mcpMethodNotFound, "unknown method: "+msg.Method)
	}
}

// callMCPTool dispatches one tool and answers with text content. Tool
// failures use the isError result flag so model clients can react.
func (s *Server) callMCPTool(response http.ResponseWriter, request *http.Request, id json.RawMessage, identity TokenIdentity, name string, arguments json.RawMessage) {
	result, toolErr := s.invokeMCPTool(request.Context(), identity, name, arguments)
	if toolErr != nil {
		s.writeMCPToolError(response, id, toolErr.Error())
		return
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		s.writeMCPError(response, id, mcpInternalError, "encode tool result")
		return
	}
	s.writeMCP(response, id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(encoded)}},
	})
}

// invokeMCPTool runs one tool against the store or submission core.
func (s *Server) invokeMCPTool(ctx context.Context, identity TokenIdentity, name string, arguments json.RawMessage) (any, error) {
	args := func(target any) error {
		if len(arguments) == 0 {
			return nil
		}
		return json.Unmarshal(arguments, target)
	}
	switch name {
	case "list_jobs":
		var input struct {
			State string `json:"state"`
		}
		if err := args(&input); err != nil {
			return nil, err
		}
		jobs, err := s.store.ListJobs(ctx)
		if err != nil {
			return nil, err
		}
		if input.State != "" {
			filtered := jobs[:0]
			for _, job := range jobs {
				if job.State == input.State {
					filtered = append(filtered, job)
				}
			}
			jobs = filtered
		}
		return map[string]any{"jobs": jobs}, nil
	case "get_job":
		var input struct {
			ID string `json:"id"`
		}
		if err := args(&input); err != nil {
			return nil, err
		}
		job, err := s.store.GetJob(ctx, input.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("job not found")
		}
		return job, err
	case "get_run":
		var input struct {
			ID string `json:"id"`
		}
		if err := args(&input); err != nil {
			return nil, err
		}
		run, err := s.store.GetRun(ctx, input.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("run not found")
		}
		return run, err
	case "get_transcript":
		var input struct {
			RunID string `json:"run_id"`
		}
		if err := args(&input); err != nil {
			return nil, err
		}
		events, err := s.store.RunEvents(ctx, input.RunID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("run not found")
		}
		return events, err
	case "get_board":
		return s.store.Snapshot(ctx)
	case "list_review_profiles":
		profiles, err := s.store.ReviewProfiles(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"profiles": profiles}, nil
	case "get_settings":
		values, err := s.store.Settings(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"settings": values}, nil
	case "get_setting":
		var input struct {
			Key string `json:"key"`
		}
		if err := args(&input); err != nil {
			return nil, err
		}
		value, ok, err := s.store.Setting(ctx, input.Key)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("setting %q not found", input.Key)
		}
		return map[string]string{"key": input.Key, "value": value}, nil
	case "get_factory":
		return s.store.FactoryStatus(ctx, s.maxConcurrentJobs)
	case "start_factory":
		if err := s.store.ResumeFactory(ctx); err != nil {
			return nil, err
		}
		s.recordFactoryAudit(ctx, identity, "factory.start", "resumed dispatch")
		return s.store.FactoryStatus(ctx, s.maxConcurrentJobs)
	case "stop_factory":
		var input struct {
			Mode   string `json:"mode"`
			Reason string `json:"reason"`
		}
		if err := args(&input); err != nil {
			return nil, err
		}
		mode := input.Mode
		if mode == "" {
			mode = "drain"
		}
		if mode != "drain" && mode != "cancel" {
			return nil, errors.New(`mode must be "drain" or "cancel"`)
		}
		if err := s.store.PauseFactory(ctx, identity.Name, input.Reason, mode); err != nil {
			return nil, err
		}
		action := "factory.drain"
		if mode == "cancel" {
			action = "factory.pause"
		}
		s.recordFactoryAudit(ctx, identity, action, fmt.Sprintf("mode=%s reason=%s", mode, input.Reason))
		return s.store.FactoryStatus(ctx, s.maxConcurrentJobs)
	case "set_job_cap":
		var input struct {
			Value int `json:"value"`
		}
		if err := args(&input); err != nil {
			return nil, err
		}
		before, err := s.store.FactoryStatus(ctx, s.maxConcurrentJobs)
		if err != nil {
			return nil, err
		}
		clamped, err := s.store.SetFactoryJobCap(ctx, input.Value)
		if err != nil {
			return nil, err
		}
		s.recordFactoryAudit(ctx, identity, "factory.job_cap", fmt.Sprintf("old=%d new=%d requested=%d", before.MaxConcurrentJobs, clamped, input.Value))
		return s.store.FactoryStatus(ctx, s.maxConcurrentJobs)
	case "set_lane_enabled":
		var input struct {
			Lane    string `json:"lane"`
			Enabled bool   `json:"enabled"`
		}
		if err := args(&input); err != nil {
			return nil, err
		}
		if err := s.store.SetLaneEnabled(ctx, input.Lane, input.Enabled); err != nil {
			return nil, err
		}
		action, value := "factory.lane_disable", "disabled"
		if input.Enabled {
			action, value = "factory.lane_enable", "enabled"
		}
		s.recordFactoryAudit(ctx, identity, action, fmt.Sprintf("lane=%s new=%s", input.Lane, value))
		return s.store.FactoryStatus(ctx, s.maxConcurrentJobs)
	case "set_setting":
		var input struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		if err := args(&input); err != nil {
			return nil, err
		}
		if !validSettingKey(input.Key) {
			return nil, errors.New("setting key must be letters, digits, dash or underscore")
		}
		if err := s.store.PutSetting(ctx, input.Key, input.Value); err != nil {
			return nil, err
		}
		if err := s.store.RecordAudit(ctx, identity, "settings.write", input.Key); err != nil {
			log.Printf("audit settings.write: %v", err)
		}
		return map[string]string{"key": input.Key, "value": input.Value}, nil
	case "queue_job":
		var input submitRequest
		if err := args(&input); err != nil {
			return nil, err
		}
		id, err := s.queueJob(ctx, input, identity)
		if err != nil {
			return nil, err
		}
		return map[string]string{"id": id}, nil
	case "cancel_job", "requeue_job":
		var input struct {
			JobID                  string `json:"job_id"`
			PreviousProcessStopped bool   `json:"previous_process_stopped"`
		}
		if err := args(&input); err != nil {
			return nil, err
		}
		action := "cancel"
		if name == "requeue_job" {
			action = "retry"
		}
		runID, err := s.store.LatestRunID(ctx, input.JobID)
		switch {
		case errors.Is(err, sql.ErrNoRows) && action == "cancel":
			// Plain jobs can sit queued without ever having run. Like the
			// HTTP path, pass the empty run token so WorkflowAction routes
			// to cancelPlainJob.
			runID = ""
		case errors.Is(err, sql.ErrNoRows):
			return nil, errors.New("job not found")
		case err != nil:
			return nil, err
		}
		if err := s.store.WorkflowAction(ctx, input.JobID, runID, action, input.PreviousProcessStopped); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, errors.New("job not found")
			}
			return nil, err
		}
		if err := s.store.RecordAudit(ctx, identity, "jobs."+action, input.JobID); err != nil {
			log.Printf("audit %s: %v", action, err)
		}
		return map[string]string{"status": "accepted", "job_id": input.JobID, "run_id": runID}, nil
	case "list_skills":
		views, err := s.listSkills(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"skills": views}, nil
	case "get_skill":
		var input struct {
			Name string `json:"name"`
		}
		if err := args(&input); err != nil {
			return nil, err
		}
		if _, err := skills.Key(input.Name); err != nil {
			return nil, errors.New("skill name is not usable")
		}
		view, err := s.getSkill(ctx, input.Name)
		if err != nil {
			return nil, err
		}
		return view, nil
	case "install_skill":
		var input struct {
			Source string `json:"source"`
		}
		if err := args(&input); err != nil {
			return nil, err
		}
		if strings.TrimSpace(input.Source) == "" {
			return nil, errors.New("source is required")
		}
		result, err := s.installSkill(ctx, input.Source)
		if err != nil {
			return nil, err
		}
		s.recordSkillAudit(ctx, identity, "skills.install", result.Name)
		return result, nil
	case "update_skill":
		var input struct {
			Name     string `json:"name"`
			Activate bool   `json:"activate"`
		}
		if err := args(&input); err != nil {
			return nil, err
		}
		if _, err := skills.Key(input.Name); err != nil {
			return nil, errors.New("skill name is not usable")
		}
		result, err := s.updateSkill(ctx, input.Name, input.Activate)
		if err != nil {
			return nil, err
		}
		s.recordSkillAudit(ctx, identity, "skills.update", result.Name)
		return result, nil
	case "pin_skill":
		var input struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if err := args(&input); err != nil {
			return nil, err
		}
		if _, err := skills.Key(input.Name); err != nil {
			return nil, errors.New("skill name is not usable")
		}
		if strings.TrimSpace(input.Version) == "" {
			return nil, errors.New("version is required")
		}
		if err := s.pinSkill(input.Name, input.Version); err != nil {
			return nil, err
		}
		s.recordSkillAudit(ctx, identity, "skills.pin", input.Name+" "+input.Version)
		view, err := s.getSkill(ctx, input.Name)
		if err != nil {
			return nil, err
		}
		return view, nil
	case "uninstall_skill":
		var input struct {
			Name  string `json:"name"`
			Force bool   `json:"force"`
		}
		if err := args(&input); err != nil {
			return nil, err
		}
		if _, err := skills.Key(input.Name); err != nil {
			return nil, errors.New("skill name is not usable")
		}
		if err := s.uninstallSkill(input.Name, input.Force); err != nil {
			return nil, err
		}
		s.recordSkillAudit(ctx, identity, "skills.uninstall", input.Name)
		return map[string]string{"status": "removed", "name": input.Name}, nil
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}

func (s *Server) writeMCP(response http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(response, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) writeMCPError(response http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeJSON(response, http.StatusOK, mcpResponse{JSONRPC: "2.0", ID: id, Error: &mcpError{Code: code, Message: message}})
}

// writeMCPToolError answers tools/call with a tool error result, keeping the
// JSON-RPC layer successful so the model sees the failure text.
func (s *Server) writeMCPToolError(response http.ResponseWriter, id json.RawMessage, message string) {
	s.writeMCP(response, id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}},
		"isError": true,
	})
}
