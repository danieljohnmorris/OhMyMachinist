// Package adapters defines the pluggable tracker and coding-agent interfaces
// the factory is built on, plus the per-project "factory setup" value that
// selects them. The first implementations wrap the existing Plane intake and
// the omp/Codex command paths; Linear is the second tracker adapter.
package adapters

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/owainlewis/machinist/internal/issues"
)

// Tracker kinds. A project picks one tracker adapter in its factory setup.
const (
	TrackerPlane  = "plane"
	TrackerLinear = "linear"
)

// Coding-agent kinds. A project picks one coding agent and, independently, one
// review agent. "command" is the generic command template form.
const (
	AgentOMP     = "omp"
	AgentCodex   = "codex"
	AgentCommand = "command"
)

// Model roles: the coding agent's model, the review agent's model, and the
// model used by non-coding steps.
const (
	RoleCoding    = "coding"
	RoleReview    = "review"
	RoleNonCoding = "non_coding"
)

// Model endpoint kinds.
const (
	EndpointLocal = "local"
	EndpointCloud = "cloud"
)

// Tracker is the common contract behind every tracker adapter: intake, status
// sync, links and resolve-on-done.
type Tracker interface {
	// Kind names the adapter ("plane", "linear").
	Kind() string
	// Intake lists the tickets carrying a label.
	Intake(ctx context.Context, label string) ([]issues.Issue, error)
	// Comment posts one comment to a ticket.
	Comment(ctx context.Context, key, body string) error
	// Resolve moves a ticket to its completed state.
	Resolve(ctx context.Context, key string) error
	// Link returns the canonical ticket URL, empty when the tracker cannot
	// derive one without a lookup.
	Link(ctx context.Context, key string) (string, error)
}

// CodingAgent is the common contract behind every coding-agent adapter. An
// agent carries the worker executor that runs it and, for review runs, the
// executor command template the review harness expects.
type CodingAgent interface {
	// Kind names the adapter ("omp", "codex", "command").
	Kind() string
	// ReviewCommand renders the review executor command for a model, leaving
	// {{env.NAME}} placeholders for the worker to expand from its environment.
	ReviewCommand(spec AgentSpec, model ModelSpec) string
}

// ModelSpec is one role's model choice: the model name or alias, the effort
// level, and whether the endpoint is a local server or a cloud API.
type ModelSpec struct {
	Model    string `json:"model" toml:"model"`
	Effort   string `json:"effort,omitempty" toml:"effort"`
	Endpoint string `json:"endpoint,omitempty" toml:"endpoint"`
	URL      string `json:"url,omitempty" toml:"url"`
}

// AgentSpec selects one coding-agent adapter. Executor names the worker
// executor that runs coding steps (omp/Codex/generic command); Command is the
// executor command template used for review runs, with {{model}}, {{effort}}
// and {{env.NAME}} placeholders.
type AgentSpec struct {
	Kind     string `json:"kind" toml:"kind"`
	Executor string `json:"executor,omitempty" toml:"executor"`
	Command  string `json:"command,omitempty" toml:"command"`
}

// FactorySetting is one project's (or the global default) pluggable setup.
// An empty field means "unset": the run keeps whatever the trigger or command
// configured, so an empty setting preserves the wrapped current behaviour.
type FactorySetting struct {
	Tracker     string               `json:"tracker,omitempty" toml:"tracker"`
	CodingAgent AgentSpec            `json:"coding_agent,omitempty" toml:"coding_agent"`
	ReviewAgent AgentSpec            `json:"review_agent,omitempty" toml:"review_agent"`
	Models      map[string]ModelSpec `json:"models,omitempty" toml:"models"`
}

// IsZero reports whether no field is set.
func (f FactorySetting) IsZero() bool {
	return f.Tracker == "" && f.CodingAgent.IsZero() && f.ReviewAgent.IsZero() && len(f.Models) == 0
}

// IsZero reports whether no field is set.
func (a AgentSpec) IsZero() bool {
	return a.Kind == "" && a.Executor == "" && a.Command == ""
}

// Model returns the model spec for a role; the zero spec when unset.
func (f FactorySetting) Model(role string) ModelSpec {
	return f.Models[role]
}

// TrackerKinds lists the supported tracker adapters.
func TrackerKinds() []string { return []string{TrackerPlane, TrackerLinear} }

// AgentKinds lists the supported coding-agent adapters.
func AgentKinds() []string { return []string{AgentOMP, AgentCodex, AgentCommand} }

// Roles lists the supported model roles.
func Roles() []string { return []string{RoleCoding, RoleReview, RoleNonCoding} }

// Endpoints lists the supported model endpoint kinds.
func Endpoints() []string { return []string{EndpointLocal, EndpointCloud} }

// Validate checks a factory setting against the supported adapters and roles.
// Empty fields are allowed (unset); set fields must be known and well formed.
func Validate(setting FactorySetting) error {
	if setting.Tracker != "" && !slices.Contains(TrackerKinds(), setting.Tracker) {
		return fmt.Errorf("unknown tracker %q (supported: %s)", setting.Tracker, strings.Join(TrackerKinds(), ", "))
	}
	if err := validateAgent("coding_agent", setting.CodingAgent, false); err != nil {
		return err
	}
	if err := validateAgent("review_agent", setting.ReviewAgent, true); err != nil {
		return err
	}
	roles := Roles()
	for role, spec := range setting.Models {
		if !slices.Contains(roles, role) {
			return fmt.Errorf("unknown model role %q (supported: %s)", role, strings.Join(roles, ", "))
		}
		if err := validateModel(role, spec); err != nil {
			return err
		}
	}
	return nil
}

// validateAgent checks one agent spec. isReview marks the review agent, which
// runs a command string rather than naming a worker executor: it must render a
// command, so its kind is required and the generic "command" kind must carry a
// command template. A coding agent needs an executor or a command instead.
func validateAgent(name string, spec AgentSpec, isReview bool) error {
	if spec.IsZero() {
		return nil
	}
	if spec.Kind != "" && !slices.Contains(AgentKinds(), spec.Kind) {
		return fmt.Errorf("%s kind %q is unknown (supported: %s)", name, spec.Kind, strings.Join(AgentKinds(), ", "))
	}
	if err := validateLine(name, spec.Kind); err != nil {
		return err
	}
	if err := validateLine(name+" executor", spec.Executor); err != nil {
		return err
	}
	if err := validateLine(name+" command", spec.Command); err != nil {
		return err
	}
	if isReview {
		// A review agent renders its command through the adapter its kind
		// names; an unset kind or a "command" kind with no template would
		// later yield no review executor at all.
		if spec.Kind == "" {
			return fmt.Errorf("%s needs a kind", name)
		}
		if spec.Kind == AgentCommand && spec.Command == "" {
			return fmt.Errorf("%s kind %q needs a command", name, AgentCommand)
		}
		return nil
	}
	if spec.Kind != "" && spec.Executor == "" && spec.Command == "" {
		return fmt.Errorf("%s needs an executor or a command", name)
	}
	return nil
}

func validateModel(role string, spec ModelSpec) error {
	if err := validateLine(role+" model", spec.Model); err != nil {
		return err
	}
	if err := validateLine(role+" effort", spec.Effort); err != nil {
		return err
	}
	if spec.Endpoint != "" && !slices.Contains(Endpoints(), spec.Endpoint) {
		return fmt.Errorf("%s endpoint %q is unknown (supported: %s)", role, spec.Endpoint, strings.Join(Endpoints(), ", "))
	}
	if spec.Endpoint == EndpointLocal {
		if err := validateLine(role+" url", spec.URL); err != nil {
			return err
		}
		if strings.TrimSpace(spec.URL) == "" {
			return fmt.Errorf("%s local endpoint needs a url", role)
		}
	}
	return nil
}

func validateLine(field, value string) error {
	if strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("%s must be one line", field)
	}
	if len(value) > 1024 {
		return fmt.Errorf("%s is too long", field)
	}
	return nil
}

// Normalize trims whitespace and drops empty agent/model entries so stored
// settings stay canonical.
func Normalize(setting FactorySetting) FactorySetting {
	setting.Tracker = strings.TrimSpace(setting.Tracker)
	setting.CodingAgent = normalizeAgent(setting.CodingAgent)
	setting.ReviewAgent = normalizeAgent(setting.ReviewAgent)
	if len(setting.Models) == 0 {
		setting.Models = nil
		return setting
	}
	models := make(map[string]ModelSpec, len(setting.Models))
	for role, spec := range setting.Models {
		spec.Model = strings.TrimSpace(spec.Model)
		spec.Effort = strings.TrimSpace(spec.Effort)
		spec.Endpoint = strings.TrimSpace(spec.Endpoint)
		spec.URL = strings.TrimSpace(spec.URL)
		if spec.Model == "" && spec.Effort == "" && spec.Endpoint == "" && spec.URL == "" {
			continue
		}
		models[strings.TrimSpace(role)] = spec
	}
	if len(models) == 0 {
		models = nil
	}
	setting.Models = models
	return setting
}

func normalizeAgent(spec AgentSpec) AgentSpec {
	spec.Kind = strings.TrimSpace(spec.Kind)
	spec.Executor = strings.TrimSpace(spec.Executor)
	spec.Command = strings.TrimSpace(spec.Command)
	return spec
}

// ---------------------------------------------------------------------------
// Coding-agent adapters

// agentAdapter is the per-kind knowledge behind CodingAgent.
type agentAdapter struct {
	kind           string
	defaultCommand string
}

func (a agentAdapter) Kind() string { return a.kind }

func (a agentAdapter) ReviewCommand(spec AgentSpec, model ModelSpec) string {
	template := strings.TrimSpace(spec.Command)
	if template == "" {
		template = a.defaultCommand
	}
	return RenderCommand(template, model, nil)
}

var agentAdapters = []agentAdapter{
	{kind: AgentOMP, defaultCommand: "omp --model {{model}} -"},
	{kind: AgentCodex, defaultCommand: "codex exec --model {{model}}"},
	{kind: AgentCommand},
}

// AgentFor returns the coding-agent adapter for a kind, and whether it is known.
func AgentFor(kind string) (CodingAgent, bool) {
	for _, adapter := range agentAdapters {
		if adapter.kind == kind {
			return adapter, true
		}
	}
	return nil, false
}

// RenderCommand substitutes {{model}}, {{effort}} and {{endpoint}} in a
// template. {{env.NAME}} placeholders are left intact unless env is non-nil,
// so review commands reach the worker unexpanded and secrets are read from the
// worker's environment, never stored in the control plane.
func RenderCommand(template string, model ModelSpec, env func(string) string) string {
	replacer := strings.NewReplacer(
		"{{model}}", model.Model,
		"{{effort}}", model.Effort,
		"{{endpoint}}", model.URL,
	)
	return replaceEnv(replacer.Replace(template), env)
}

func replaceEnv(template string, env func(string) string) string {
	if env == nil || !strings.Contains(template, "{{env.") {
		return template
	}
	var builder strings.Builder
	for {
		start := strings.Index(template, "{{env.")
		if start < 0 {
			builder.WriteString(template)
			return builder.String()
		}
		builder.WriteString(template[:start])
		rest := template[start+len("{{env."):]
		end := strings.Index(rest, "}}")
		if end < 0 {
			builder.WriteString(template[start:])
			return builder.String()
		}
		name := strings.TrimSpace(rest[:end])
		if name == "" {
			builder.WriteString("{{env.")
			template = rest
			continue
		}
		builder.WriteString(env(name))
		template = rest[end+2:]
	}
}

// DocumentedTrackers names the trackers for the settings catalogue.
func DocumentedTrackers() []string { return sortedCopy(TrackerKinds()) }

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// ErrUnsupportedTracker reports a factory setting that names no known tracker.
var ErrUnsupportedTracker = errors.New("adapters: unsupported tracker")
