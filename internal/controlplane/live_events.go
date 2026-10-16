package controlplane

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"net/http"

	"github.com/owainlewis/machinist/internal/protocol"
	"github.com/owainlewis/machinist/internal/runner"
)

// ErrLiveEventsFull reports that a running run already holds the maximum
// event log size, so further live chunks are dropped until completion
// delivers the full log.
var ErrLiveEventsFull = errors.New("live event log is full")

// RunEvents is the event log the run page shows: live chunks while the run
// executes, then the full log stored by completion.
type RunEvents struct {
	State  string `json:"state"`
	Events string `json:"events"`
}

// AppendLiveEvents appends event log lines pushed by the worker that holds the
// run lease. Completion later replaces them with the full event log.
func (s *Store) AppendLiveEvents(ctx context.Context, runID string, input protocol.LiveEvents) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var state, instanceID, leaseToken string
	var size int64
	if err := tx.QueryRowContext(ctx, `SELECT state,COALESCE(worker_instance,''),COALESCE(lease_token,''),LENGTH(CAST(COALESCE(events,'') AS BLOB)) FROM runs WHERE id=?`, runID).Scan(&state, &instanceID, &leaseToken, &size); err != nil {
		return err
	}
	if state != "running" {
		return ErrRunState
	}
	if subtle.ConstantTimeCompare([]byte(instanceID), []byte(input.InstanceID)) != 1 || subtle.ConstantTimeCompare([]byte(leaseToken), []byte(input.LeaseToken)) != 1 {
		return ErrLeaseConflict
	}
	if size+int64(len(input.Events)) > runner.MaxEventLogBytes {
		return ErrLiveEventsFull
	}
	// New output is progress: it moves the run's last-progress clock so a
	// silent agent (a lost LLM backend) stalls after the configured idle time.
	result, err := tx.ExecContext(ctx, `UPDATE runs SET events=COALESCE(events,'')||?,last_progress_at=? WHERE id=? AND state='running' AND worker_instance=? AND lease_token=?`, input.Events, s.now().UTC().Format(time.RFC3339Nano), runID, input.InstanceID, input.LeaseToken)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return ErrLeaseConflict
	}
	return tx.Commit()
}

// RunEvents returns a run state and its stored event log.
func (s *Store) RunEvents(ctx context.Context, runID string) (RunEvents, error) {
	var output RunEvents
	err := s.db.QueryRowContext(ctx, `SELECT state,COALESCE(events,'') FROM runs WHERE id=?`, runID).Scan(&output.State, &output.Events)
	return output, err
}

func (s *Server) liveEvents(response http.ResponseWriter, request *http.Request) {
	if !limitRequestBody(response, request, maxCompletionBytes) {
		return
	}
	var input protocol.LiveEvents
	if err := decodeJSON(request, &input); err != nil {
		writeDecodeError(response, err)
		return
	}
	if input.InstanceID == "" || input.LeaseToken == "" {
		writeError(response, http.StatusBadRequest, errors.New("instance_id and lease_token are required"))
		return
	}
	err := s.store.AppendLiveEvents(request.Context(), request.PathValue("id"), input)
	switch {
	case err == nil:
		response.WriteHeader(http.StatusNoContent)
	case errors.Is(err, ErrLeaseConflict), errors.Is(err, ErrRunState):
		writeError(response, http.StatusConflict, err)
	case errors.Is(err, ErrLiveEventsFull):
		writeError(response, http.StatusRequestEntityTooLarge, err)
	case errors.Is(err, sql.ErrNoRows):
		writeError(response, http.StatusNotFound, errors.New("run not found"))
	default:
		writeError(response, http.StatusInternalServerError, err)
	}
}

func (s *Server) runEvents(response http.ResponseWriter, request *http.Request) {
	output, err := s.store.RunEvents(request.Context(), request.PathValue("id"))
	if errors.Is(err, sql.ErrNoRows) {
		writeError(response, http.StatusNotFound, errors.New("run not found"))
		return
	}
	if err != nil {
		writeError(response, http.StatusInternalServerError, err)
		return
	}
	writeJSON(response, http.StatusOK, output)
}
