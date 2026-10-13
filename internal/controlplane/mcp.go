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
	JSONRPC string        `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any           `json:"result,omitempty"`
	Error   *mcpError     `json:"error,omitempty"`
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
	{Name: "queue_job", Description: "Queue a job. Either command (single command) or workflow (task workflow) plus repository is required.", InputSchema: mcpSchema(map[string]any{
		"title": map[string]any{"type": "string"}, "spec": map[string]any{"type": "string"},
		"source_url": map[string]any{"type": "string"}, "workflow": map[string]any{"type": "string"},
		"command": map[string]any{"type": "string"}, "prompt": map[string]any{"type": "string"},
		"repository": map[string]any{"type": "string"}, "model": map[string]any{"type": "string"},
		"project":    map[string]any{"type": "string"},
	}, "repository")},
	{Name: "cancel_job", Description: "Cancel a job and its latest run.", InputSchema: mcpSchema(map[string]any{"job_id": map[string]any{"type": "string"}}, "job_id")},
	{Name: "requeue_job", Description: "Requeue (retry) a failed, blocked, interrupted or cancelled job. Set previous_process_stopped=true to confirm the prior process is stopped.", InputSchema: mcpSchema(map[string]any{
		"job_id": map[string]any{"type": "string"}, "previous_process_stopped": map[string]any{"type": "boolean"},
	}, "job_id")},
	{Name: "get_setting", Description: "Read one stored setting by key.", InputSchema: mcpSchema(map[string]any{"key": map[string]any{"type": "string"}}, "key")},
	{Name: "set_setting", Description: "Write one settings key/value pair.", InputSchema: mcpSchema(map[string]any{
		"key": map[string]any{"type": "string"}, "value": map[string]any{"type": "string"},
	}, "key", "value")},
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
