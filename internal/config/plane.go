package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// MergeHealthCheck verifies a deploy: either a command (exit 0, and its
// output must contain expect when expect is set) or a URL (expected status,
// default 200, and the body must contain expect when expect is set). The
// literal "{sha}" in command and expect is replaced with the merge SHA before
// the check runs.
type MergeHealthCheck struct {
	Command string `toml:"command" json:"command"`
	URL     string `toml:"url" json:"url"`
	Status  int    `toml:"status" json:"status"`
	Expect  string `toml:"expect" json:"expect"`
}

// MergePolicy is the per-project auto-merge and deploy policy (MACH-24),
// keyed by the Plane project identifier ("MACH", "SPARK", …) in
// [plane.merge_policies]. auto_merge off projects are never touched.
type MergePolicy struct {
	AutoMerge bool `toml:"auto_merge" json:"auto_merge"`
	// MergeMethod is the GitHub merge method: squash (default), merge or rebase.
	MergeMethod string `toml:"merge_method" json:"merge_method"`
	// DeployCommand runs after the merge. A failure rolls back to the
	// previous release (RollbackCommand) and moves the ticket to Needs
	// attention instead of leaving the merge in place unverified.
	DeployCommand string `toml:"deploy_command" json:"deploy_command"`
	// RollbackCommand restores the previous release; it needs the deploy to
	// have kept one (the MACH seed keeps machinist.prev-before-<sha>).
	RollbackCommand string            `toml:"rollback_command" json:"rollback_command"`
	HealthCheck     *MergeHealthCheck `toml:"health_check" json:"health_check"`
	// DeployTimeout bounds the deploy command; default 15m.
	DeployTimeout string `toml:"deploy_timeout" json:"deploy_timeout"`
}

// DefaultDeployTimeout bounds a policy deploy command.
const DefaultDeployTimeout = 15 * time.Minute

// DefaultMACHDeployCommand rebuilds the control plane from merged main and
// restarts it: versioned copy (machinist.<version>) plus a
// machinist.prev-before-<sha> copy for rollback and a SQLite DB backup, all
// under /opt/machinist.
const DefaultMACHDeployCommand = `set -euo pipefail
cd /opt/machinist/src/OhMyMachinist
git -c safe.directory=/opt/machinist/src/OhMyMachinist fetch origin main
git -c safe.directory=/opt/machinist/src/OhMyMachinist checkout -q main
git -c safe.directory=/opt/machinist/src/OhMyMachinist pull --ff-only origin main
just build
sha="$(git -c safe.directory=/opt/machinist/src/OhMyMachinist rev-parse --short HEAD)"
version="$(./bin/machinist version)"
cp -f /opt/machinist/bin/machinist "/opt/machinist/bin/machinist.prev-before-$sha"
cp -f ./bin/machinist "/opt/machinist/bin/machinist.$version"
mkdir -p /home/machinist/.machinist/server/backups
if command -v sqlite3 >/dev/null 2>&1; then
  sqlite3 /home/machinist/.machinist/server/machinist.db ".backup '/home/machinist/.machinist/server/backups/machinist.db.$(date +%Y%m%d-%H%M%S)'"
else
  cp -f /home/machinist/.machinist/server/machinist.db "/home/machinist/.machinist/server/backups/machinist.db.$(date +%Y%m%d-%H%M%S)"
fi
install -m 0755 ./bin/machinist /opt/machinist/bin/machinist
systemctl restart machinist-control`

// DefaultMACHRollbackCommand reinstalls the previous binary copy and
// restarts the control plane; the restart is last so an interrupted rollback
// still leaves the previous binary in place.
const DefaultMACHRollbackCommand = `set -euo pipefail
cp -f "$(ls -1t /opt/machinist/bin/machinist.prev-before-* | head -1)" /opt/machinist/bin/machinist
systemctl restart machinist-control`

// DefaultMergePolicies seeds the authorised MACH auto-merge project (MACH-24):
// MACH / OhMyMachinist rebuilds the control plane from merged main and
// restarts it; SPARK / sparkDash pulls and rebuilds the compose stack on the
// host named by MACHINIST_SPARK_HOST (see DefaultSPARKPolicy). Every other
// project is off until configured.
func DefaultMergePolicies() map[string]MergePolicy {
	return map[string]MergePolicy{
		"MACH": {
			AutoMerge:       true,
			DeployCommand:   DefaultMACHDeployCommand,
			RollbackCommand: DefaultMACHRollbackCommand,
			HealthCheck: &MergeHealthCheck{
				Command: `/opt/machinist/bin/machinist version | grep -q '{sha}' && curl -fsS http://127.0.0.1:7331/ | grep -q 'assets/'`,
			},
			DeployTimeout: "20m",
		},
	}
}

// DefaultSPARKPolicy seeds the SPARK auto-merge policy from the operator's
// environment: the deploy target and health-check host come from
// MACHINIST_SPARK_HOST, so no private address is baked into the tool. Without
// the host the policy is absent and the project defaults to auto-merge off.
func DefaultSPARKPolicy() (MergePolicy, bool) {
	host := os.Getenv("MACHINIST_SPARK_HOST")
	if host == "" {
		return MergePolicy{}, false
	}
	return MergePolicy{
		AutoMerge:     true,
		DeployCommand: fmt.Sprintf(`ssh machinist@%s 'cd ~/sparkDash && git pull --ff-only && docker compose up --build -d'`, host),
		HealthCheck: &MergeHealthCheck{
			URL:    fmt.Sprintf("http://%s:5555/", host),
			Status: 200,
		},
		DeployTimeout: "20m",
	}, true
}

// ResolveMergePolicies layers the configured policies over the seeds: a
// project entry replaces the seed for that project entirely, every other
// project keeps its seed, and unknown projects default to auto-merge off.
// Normalises merge methods and deploy timeouts.
func ResolveMergePolicies(configured map[string]MergePolicy) map[string]MergePolicy {
	policies := map[string]MergePolicy{}
	for project, policy := range DefaultMergePolicies() {
		policies[project] = policy
	}
	if spark, ok := DefaultSPARKPolicy(); ok {
		policies["SPARK"] = spark
	}
	for project, policy := range configured {
		if strings.TrimSpace(project) == "" {
			continue
		}
		policies[strings.ToUpper(strings.TrimSpace(project))] = policy
	}
	for project, policy := range policies {
		policies[project] = normalizeMergePolicy(policy)
	}
	return policies
}

func normalizeMergePolicy(policy MergePolicy) MergePolicy {
	switch strings.ToLower(strings.TrimSpace(policy.MergeMethod)) {
	case "merge", "rebase":
		policy.MergeMethod = strings.ToLower(strings.TrimSpace(policy.MergeMethod))
	default:
		policy.MergeMethod = "squash"
	}
	if policy.DeployTimeout == "" {
		policy.DeployTimeout = "15m"
	}
	return policy
}

// DeployDeadline parses the deploy timeout, falling back to the default.
func (p MergePolicy) DeployDeadline() time.Duration {
	deadline, err := time.ParseDuration(p.DeployTimeout)
	if err != nil || deadline <= 0 {
		return DefaultDeployTimeout
	}
	return deadline
}

// MergePoliciesFromSettings parses the settings-table forms of the merge
// policies: the whole map ("plane.merge_policies", imported from
// plane-repos.json) and per-project overrides ("plane.merge_policy.MACH") as
// JSON MergePolicy objects.
func MergePoliciesFromSettings(whole string, settings map[string]string) (map[string]MergePolicy, error) {
	configured := map[string]MergePolicy{}
	if strings.TrimSpace(whole) != "" {
		if err := json.Unmarshal([]byte(whole), &configured); err != nil {
			return nil, fmt.Errorf("parse plane.merge_policies JSON: %w", err)
		}
	}
	for key, raw := range settings {
		identifier, ok := strings.CutPrefix(key, "plane.merge_policy.")
		if !ok || strings.TrimSpace(identifier) == "" {
			continue
		}
		var policy MergePolicy
		if err := json.Unmarshal([]byte(raw), &policy); err != nil {
			return nil, fmt.Errorf("parse plane.merge_policy.%s JSON: %w", strings.ToUpper(identifier), err)
		}
		configured[strings.ToUpper(strings.TrimSpace(identifier))] = policy
	}
	return configured, nil
}

// Plane configures the built-in Plane intake. Every value has a default that
// matches the retired plane-intake.py so an empty table keeps working; the
// same knobs are editable at runtime through Settings ("plane.*" keys).
type Plane struct {
	URL               string                 `toml:"url"`
	Workspace         string                 `toml:"workspace"`
	WebhookSecret     string                 `toml:"webhook_secret"`
	TriggerLabel      string                 `toml:"trigger_label"`
	TriggerLabelIDs   []string               `toml:"trigger_label_ids"`
	ApproveLabel      string                 `toml:"approve_label"`
	FailedLabel       string                 `toml:"failed_label"`
	ProgressState     string                 `toml:"progress_state"`
	ReviewState       string                 `toml:"review_state"`
	Command           string                 `toml:"command"`
	AutoSubmit        *bool                  `toml:"auto_submit"`
	StartComment      *bool                  `toml:"start_comment"`
	Shadow            *bool                  `toml:"shadow"`
	AutoMergeProjects []string               `toml:"auto_merge_projects"`
	MergePolicies     map[string]MergePolicy `toml:"merge_policies"`
	Projects          []string               `toml:"projects"`
	DefaultRepo       string                 `toml:"default_repo"`
	UIURL             string                 `toml:"ui_url"`
	PromptTemplate    string                 `toml:"prompt_template"`
	PollEvery         string                 `toml:"poll_every"`
	SyncEvery         string                 `toml:"sync_every"`
	SyncSince         string                 `toml:"sync_since"`
	Repositories      map[string]string      `toml:"repositories"`
	Identifiers       map[string]string      `toml:"identifiers"`
	Supervisors       map[string]string      `toml:"supervisors"`
}

// PlaneRepositoryMapFile and PlaneSupervisorMapFile are the legacy JSON state
// files imported on first start.
const (
	PlaneRepositoryMapFile = "plane-repos.json"
	PlaneSupervisorMapFile = "plane-supervisors.json"
	PlaneJobsDirectory     = "plane-jobs"
	PlaneAlertsDirectory   = "alerts"
)
