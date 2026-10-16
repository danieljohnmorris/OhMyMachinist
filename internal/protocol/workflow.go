package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

type StepResult struct {
	ApprovalRequired bool           `json:"approval_required,omitempty"`
	Outcome          string         `json:"outcome"`
	Summary          string         `json:"summary"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	// Stage, ReviewRound and JobType let an executor report the workflow stage
	// it reached (MACH-43); the control plane records them as job metadata.
	Stage       string `json:"stage,omitempty"`
	ReviewRound int    `json:"review_round,omitempty"`
	JobType     string `json:"job_type,omitempty"`
}

// StageMetadata returns the stage fields an executor reported, ready to merge
// into job metadata. It is empty when the result carries no stage information.
func (r StepResult) StageMetadata() map[string]any {
	metadata := map[string]any{}
	if r.JobType != "" {
		metadata["job_type"] = r.JobType
	}
	if r.Stage != "" {
		metadata["stage"] = r.Stage
	}
	if r.ReviewRound > 0 {
		metadata["review_round"] = r.ReviewRound
	}
	return metadata
}

func ParseStepResult(body []byte) (*StepResult, error) {
	if len(body) > 16384 {
		return nil, errors.New("step result exceeds 16 KiB")
	}
	var result StepResult
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("step result must contain one JSON object")
	}
	if result.Outcome != "complete" && result.Outcome != "blocked" && result.Outcome != "failed" {
		return nil, errors.New("step outcome must be complete, blocked, or failed")
	}
	if strings.TrimSpace(result.Summary) == "" {
		return nil, errors.New("step summary is required")
	}
	return &result, nil
}
