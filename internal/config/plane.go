package config

// Plane configures the built-in Plane intake. Every value has a default that
// matches the retired plane-intake.py so an empty table keeps working; the
// same knobs are editable at runtime through Settings ("plane.*" keys).
type Plane struct {
	URL               string            `toml:"url"`
	Workspace         string            `toml:"workspace"`
	WebhookSecret     string            `toml:"webhook_secret"`
	TriggerLabel      string            `toml:"trigger_label"`
	TriggerLabelIDs   []string          `toml:"trigger_label_ids"`
	ApproveLabel      string            `toml:"approve_label"`
	FailedLabel       string            `toml:"failed_label"`
	ProgressState     string            `toml:"progress_state"`
	ReviewState       string            `toml:"review_state"`
	Command           string            `toml:"command"`
	AutoSubmit        *bool             `toml:"auto_submit"`
	StartComment      *bool             `toml:"start_comment"`
	Shadow            *bool             `toml:"shadow"`
	AutoMergeProjects []string          `toml:"auto_merge_projects"`
	Projects          []string          `toml:"projects"`
	DefaultRepo       string            `toml:"default_repo"`
	UIURL             string            `toml:"ui_url"`
	PromptTemplate    string            `toml:"prompt_template"`
	PollEvery         string            `toml:"poll_every"`
	SyncEvery         string            `toml:"sync_every"`
	SyncSince         string            `toml:"sync_since"`
	Repositories      map[string]string `toml:"repositories"`
	Identifiers       map[string]string `toml:"identifiers"`
	Supervisors       map[string]string `toml:"supervisors"`
}

// PlaneRepositoryMapFile and PlaneSupervisorMapFile are the legacy JSON state
// files imported on first start.
const (
	PlaneRepositoryMapFile = "plane-repos.json"
	PlaneSupervisorMapFile = "plane-supervisors.json"
	PlaneJobsDirectory     = "plane-jobs"
	PlaneAlertsDirectory   = "alerts"
)
