package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
)

// getFactoryHandler serves GET /api/v1/factory: the control state, the
// effective job cap and one slot per lane.
func (s *Server) getFactoryHandler(response http.ResponseWriter, request *http.Request) {
	state, err := s.store.FactoryStatus(request.Context(), s.maxConcurrentJobs)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	response.Header().Set("Cache-Control", "no-store")
	writeJSON(response, http.StatusOK, state)
}

// startFactoryHandler resumes dispatch.
func (s *Server) startFactoryHandler(response http.ResponseWriter, request *http.Request) {
	if err := s.store.ResumeFactory(request.Context()); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	s.recordAudit(request, "factory.start", "resumed dispatch")
	s.writeFactoryState(response, request)
}

// stopFactoryHandler stops dispatch. The body carries mode ("drain" leaves the
// running job to finish, "cancel" stops it now) and an optional reason.
func (s *Server) stopFactoryHandler(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input struct {
		Mode   string `json:"mode"`
		Reason string `json:"reason"`
	}
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	mode := input.Mode
	if mode == "" {
		mode = "drain"
	}
	if mode != "drain" && mode != "cancel" {
		writeError(response, http.StatusBadRequest, errors.New(`mode must be "drain" or "cancel"`))
		return
	}
	actor := identityFrom(request).Name
	if err := s.store.PauseFactory(request.Context(), actor, input.Reason, mode); err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	action := "factory.drain"
	if mode == "cancel" {
		action = "factory.pause"
	}
	s.recordAudit(request, action, fmt.Sprintf("mode=%s reason=%s", mode, input.Reason))
	s.writeFactoryState(response, request)
}

// setFactoryJobsHandler sets the job cap. Body: {"value": 2}.
func (s *Server) setFactoryJobsHandler(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input struct {
		Value int `json:"value"`
	}
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	before, err := s.store.FactoryStatus(request.Context(), s.maxConcurrentJobs)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	clamped, err := s.store.SetFactoryJobCap(request.Context(), input.Value)
	if err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	s.recordAudit(request, "factory.job_cap", fmt.Sprintf("old=%d new=%d requested=%d", before.MaxConcurrentJobs, clamped, input.Value))
	s.writeFactoryState(response, request)
}

// setLaneHandler enables or disables one lane. Body: {"enabled": false}.
func (s *Server) setLaneHandler(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxRequestBytes) {
		return
	}
	var input struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	name := request.PathValue("name")
	if err := s.store.SetLaneEnabled(request.Context(), name, input.Enabled); err != nil {
		writeError(response, http.StatusBadRequest, err)
		return
	}
	action := "factory.lane_disable"
	value := "disabled"
	if input.Enabled {
		action = "factory.lane_enable"
		value = "enabled"
	}
	s.recordAudit(request, action, fmt.Sprintf("lane=%s new=%s", name, value))
	s.writeFactoryState(response, request)
}

// writeFactoryState answers a control change with the resulting state so a
// caller does not need a second request.
func (s *Server) writeFactoryState(response http.ResponseWriter, request *http.Request) {
	state, err := s.store.FactoryStatus(request.Context(), s.maxConcurrentJobs)
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, state)
}

// recordFactoryAudit writes one audit entry for a factory change made over MCP,
// where there is no HTTP request to read the principal from.
func (s *Server) recordFactoryAudit(ctx context.Context, identity TokenIdentity, action, detail string) {
	if err := s.store.RecordAudit(ctx, identity, action, detail); err != nil {
		log.Printf("audit %s: %v", action, err)
	}
}
