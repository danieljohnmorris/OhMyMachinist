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
	ID              string              `json:"id"`
	JobID           string              `json:"job_id"`
	Command         string              `json:"command"`
	CommandHash     string              `json:"command_hash"`
	Executor        string              `json:"executor"`
	Model           string              `json:"model,omitempty"`
	Repository      string              `json:"repository"`
	Project         string              `json:"project,omitempty"`
	RenderedPrompt  string              `json:"rendered_prompt"`
	TimeoutMillis   int64               `json:"timeout_millis"`
	LeaseToken      string              `json:"lease_token"`
}

// LoadedSkill is one skill a run loaded, recorded so the API can show what the
// agent and reviewers had available. It never carries skill content.
type LoadedSkill struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Scope   string `json:"scope"`
	Method  string `json:"method"`
}

// RunSkills is the ordered skills a run loaded, with any load warnings.
type RunSkills struct {
	Skills   []LoadedSkill `json:"skills"`
	Warnings []string      `json:"warnings,omitempty"`
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

type Completion struct {
	Artifacts        []string        `json:"artifacts,omitempty"`
	PublicationError string          `json:"publication_error,omitempty"`
	Skills           *RunSkills      `json:"skills,omitempty"`
	InstanceID       string          `json:"instance_id"`
	LeaseToken       string          `json:"lease_token"`
	State            string          `json:"state"`
	ExitCode         int             `json:"exit_code"`
	Error            string          `json:"error,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
	Events           string          `json:"events,omitempty"`
}
