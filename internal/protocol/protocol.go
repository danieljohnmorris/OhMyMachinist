package protocol

import "encoding/json"

type PollRequest struct {
	SharedOutputs bool                `json:"shared_outputs,omitempty"`
	Reviews       bool                `json:"reviews,omitempty"`
	Artifacts     bool                `json:"artifacts,omitempty"`
	Workflows     bool                `json:"workflows,omitempty"`
	InstanceID    string              `json:"instance_id"`
	Name          string              `json:"name"`
	Executors     []string            `json:"executors"`
	Repositories  []string            `json:"repositories"`
	Models        map[string][]string `json:"models,omitempty"`
}

type PollResponse struct {
	Run *RunSpec `json:"run,omitempty"`
}

type RunSpec struct {
	Revision        *Revision           `json:"revision,omitempty"`
	Task            *Task               `json:"task,omitempty"`
	Inputs          map[string]Artifact `json:"inputs,omitempty"`
	RequiredOutputs []string            `json:"required_outputs,omitempty"`
	ArtifactLimits  *ArtifactLimits     `json:"artifact_limits,omitempty"`
	Workflow        bool                `json:"workflow,omitempty"`
	// ResumeSession continues the named coding agent session instead of
	// starting fresh; it is set on the run a Resume created.
	ResumeSession string `json:"resume_session,omitempty"`
	ID              string              `json:"id"`
	JobID           string              `json:"job_id"`
	Command         string              `json:"command"`
	CommandHash     string              `json:"command_hash"`
	Executor        string              `json:"executor"`
	Model           string              `json:"model,omitempty"`
	Repository      string              `json:"repository"`
	RenderedPrompt  string              `json:"rendered_prompt"`
	TimeoutMillis   int64               `json:"timeout_millis"`
	LeaseToken      string              `json:"lease_token"`
}

type Heartbeat struct {
	InstanceID string `json:"instance_id"`
	LeaseToken string `json:"lease_token"`
}

// LiveEvents carries event log lines a worker pushes while a run is still
// executing, so the control plane can serve a live transcript.
type LiveEvents struct {
	InstanceID string `json:"instance_id"`
	LeaseToken string `json:"lease_token"`
	Events     string `json:"events"`
}

// PokeDeliveries asks the control plane for steer messages (Poke) queued for a
// run the worker holds, marking them delivered.
type PokeDeliveries struct {
	InstanceID string `json:"instance_id"`
	LeaseToken string `json:"lease_token"`
}

// PokeBatch is the pending steer text the worker writes into the run's stdin.
type PokeBatch struct {
	Messages []string `json:"messages"`
}

type Completion struct {
	Artifacts        []string        `json:"artifacts,omitempty"`
	PublicationError string          `json:"publication_error,omitempty"`
	// SessionID is the coding agent's own session identifier, when the executor
	// reports one, so a later Resume can continue the same session.
	SessionID  string `json:"session_id,omitempty"`
	InstanceID string `json:"instance_id"`
	LeaseToken       string          `json:"lease_token"`
	State            string          `json:"state"`
	ExitCode         int             `json:"exit_code"`
	Error            string          `json:"error,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
	Events           string          `json:"events,omitempty"`
}
