package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/owainlewis/machinist/internal/issues"
	"github.com/owainlewis/machinist/internal/triggers"
	"github.com/pelletier/go-toml/v2"
)

const (
	defaultTimeout           = 30 * time.Minute
	maxConfigBytes           = 1 << 20
	maxPromptBytes           = 256 << 10
	maxInputPromptBytes      = 256 << 10
	maxRenderedPromptBytes   = maxPromptBytes + maxInputPromptBytes
	maxTokenBytes            = 8 << 10
	defaultWorkerDirName     = ".machinist/worker"
	defaultServerDirName     = ".machinist/server"
	promptParameter          = "{{machinist.prompt}}"
	modelParameter           = "{{machinist.model}}"
	machinistParameterPrefix = "{{machinist"
	legacyFactoryPrefix      = "{{factory."

	// Config overlays carry instance-specific values (hosts, paths, tokens,
	// projects) that must not live in git. The overlay is merged on top of the
	// tracked config: base first, overlay second, overlay wins. The overlay
	// path is MACHINIST_CONFIG_OVERLAY (MACHINIST_WORKER_CONFIG_OVERLAY for
	// worker.toml) when set, else the sibling config.local.toml
	// (worker.local.toml) next to the base file when it exists.
	configOverlayEnv       = "MACHINIST_CONFIG_OVERLAY"
	workerConfigOverlayEnv = "MACHINIST_WORKER_CONFIG_OVERLAY"
	configOverlayName      = "config.local.toml"
	workerOverlayName      = "worker.local.toml"
)

type Worker struct {
	Name          string `toml:"name"`
	DataDirectory string `toml:"data_directory"`
	// RawLogMaxBytes caps the raw agent log artifact uploaded for each run.
	// Zero uses the runner default (64 MB).
	RawLogMaxBytes int64                 `toml:"raw_log_max_bytes"`
	ControlPlane   ControlPlane          `toml:"control_plane"`
	Executors      map[string]Executor   `toml:"executors"`
	Repositories   map[string]Repository `toml:"repositories"`
	configDir      string
}

type ControlPlane struct {
	URL       string `toml:"url"`
	TokenFile string `toml:"token_file"`
}

type Executor struct {
	Command []string          `toml:"command"`
	Models  map[string]string `toml:"models"`
}

func (e Executor) supportsModel() bool {
	return slices.ContainsFunc(e.Command, func(argument string) bool { return strings.Contains(argument, modelParameter) })
}

type Repository struct {
	Path string `toml:"path"`
}

type Server struct {
	Listen            string `toml:"listen"`
	Database          string `toml:"database"`
	WorkerTokenFile   string `toml:"worker_token_file"`
	MaxConcurrentJobs *int   `toml:"max_concurrent_jobs"`
	// LeaseGrace is how long after a control-plane (re)start a run lease that
	// lapsed — typically while the control plane was down — still belongs to
	// its worker (MACH-71): the worker can re-attach by heartbeat and the run
	// is not redelivered to another lane until the grace elapses with no
	// heartbeat. It is measured from the server start, not from the last
	// heartbeat before the outage. Empty means 3m; two to five minutes is a
	// sensible range (a few heartbeat intervals).
	LeaseGrace string `toml:"lease_grace"`
	// Auth selects the API authentication mode: "tokens" (default) requires
	// scoped bearer tokens for machine access, "none" disables auth and is
	// only allowed on loopback or trusted Tailscale interfaces.
	Auth      string `toml:"auth"`
	configDir string
}

// AuthTokens and AuthNone name the supported server auth modes.
const (
	AuthTokens = "tokens"
	AuthNone   = "none"
)

type Config struct {
	Storage      Storage             `toml:"storage"`
	Workflows    map[string]Workflow `toml:"workflows"`
	Server       Server              `toml:"server"`
	Commands     map[string]Command  `toml:"commands"`
	GitHub       GitHub              `toml:"github"`
	IssueSources issues.Config       `toml:"issue_sources"`
	Triggers     TriggerDefinitions  `toml:"triggers"`
	Alerts       Alerts              `toml:"alerts"`
	Review       Review              `toml:"review"`
	Plane        Plane               `toml:"plane"`
	Tracker      Tracker             `toml:"tracker"`
	Healing      Healing             `toml:"healing"`
	Health       Health              `toml:"health"`
	path         string
}

// Review configures the built-in PR review. DefaultProfile names the library
// profile used when a repository has no profile of its own (repo
// .machinist/review.yaml at the base ref and the per-project Settings choice
// both take precedence); it falls back to the shipped `default` profile.
type Review struct {
	DefaultProfile string `toml:"default_profile"`
}

// Alerts configures the pending failure alert spool and the optional webhook
// that wakes the external alert watcher. When Directory is unset no alert
// files are written and failures alert nowhere.
type Alerts struct {
	// Directory is the pending spool an external watcher polls. Empty
	// disables alert files.
	Directory string `toml:"directory"`
	// WebhookURL receives a POST of every spooled alert so the watcher can
	// wake on push instead of polling. Empty disables the webhook. A webhook
	// failure is logged and never fails the run completion or the file write.
	WebhookURL string `toml:"webhook_url"`
	// WebhookHeader carries the secret to the watcher. Empty means
	// "X-Machinist-Alert-Token".
	WebhookHeader string `toml:"webhook_header"`
	// WebhookSecretEnv names the environment variable holding the webhook
	// secret. The secret is never logged.
	WebhookSecretEnv string `toml:"webhook_secret_env"`
	// WebhookSecretFile is a path to a file holding the webhook secret, used
	// when WebhookSecretEnv is unset.
	WebhookSecretFile string `toml:"webhook_secret_file"`
	// HealingAlerts also spools a healing-<run>.json when self-healing
	// requeues a job or schedules a backoff retry. Off by default: a retry is
	// not a failure a human has to see.
	HealingAlerts bool `toml:"healing_alerts"`
}

// Healing configures the self-healing pass (MACH-25): when a run fails, times
// out or is refused, Machinist classifies the cause and requeues within a
// per-ticket retry budget instead of waiting for an external watcher.
type Healing struct {
	// MaxRetries is the default requeue budget per ticket. Zero means the
	// default of 2; a negative value disables self-healing entirely.
	MaxRetries int `toml:"max_retries"`
	// MaxRetriesPerProject overrides MaxRetries per project key (e.g. MACH).
	MaxRetriesPerProject map[string]int `toml:"max_retries_per_project"`
	// DiagnoseCommand classifies a failure: the diagnose prompt goes to stdin
	// and the process prints {"cause": "...", "summary": "..."} on stdout,
	// e.g. ["/usr/local/lib/machinist-factory/omp-glm", "low"]. Empty uses the
	// built-in keyword classifier on the run's error and transcript.
	DiagnoseCommand []string `toml:"diagnose_command"`
	// DiagnoseTimeout bounds the diagnose command. Empty means 5m.
	DiagnoseTimeout string `toml:"diagnose_timeout"`
	// Backoff is the delay before the first infra-cause retry. Empty means 5m;
	// each further infra retry doubles it.
	Backoff string `toml:"backoff"`
}

// DiagnoseBudget returns the effective retry budget for a project key. The
// per-project table wins over the default; the default itself is 2 retries.
func (h Healing) DiagnoseBudget(project string) int {
	if h.MaxRetries < 0 {
		return 0
	}
	if h.MaxRetries == 0 {
		if override, ok := h.MaxRetriesPerProject[project]; ok {
			if override < 0 {
				return 0
			}
			return override
		}
		return 2
	}
	if override, ok := h.MaxRetriesPerProject[project]; ok {
		if override < 0 {
			return 0
		}
		return override
	}
	return h.MaxRetries
}

// DiagnoseDeadline parses the diagnose timeout, defaulting to 5 minutes.
func (h Healing) DiagnoseDeadline() (time.Duration, error) {
	if h.DiagnoseTimeout == "" {
		return 5 * time.Minute, nil
	}
	parsed, err := time.ParseDuration(h.DiagnoseTimeout)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("healing.diagnose_timeout must be a positive duration: %w", err)
	}
	return parsed, nil
}

// HealingBackoff parses the base retry backoff, defaulting to 5 minutes.
func (h Healing) HealingBackoff() (time.Duration, error) {
	if h.Backoff == "" {
		return 5 * time.Minute, nil
	}
	parsed, err := time.ParseDuration(h.Backoff)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("healing.backoff must be a positive duration: %w", err)
	}
	return parsed, nil
}

// Health configures endpoint health checks (MACH-25): Machinist alerts when a
// configured endpoint is unreachable. Alerts land in the same spool directory
// as failure alerts.
type Health struct {
	// Every is the check cadence. Empty means 1m.
	Every string `toml:"every"`
	// Timeout bounds each request. Empty means 10s.
	Timeout string `toml:"timeout"`
	// Endpoints maps a name to the URL to probe, e.g. glm = the GLM API
	// endpoint, sparks = the Sparks host, pipsqueek = the pipsqueek host.
	Endpoints map[string]string `toml:"endpoints"`
}

// Interval parses the check cadence, defaulting to one minute.
func (h Health) Interval() (time.Duration, error) {
	if h.Every == "" {
		return time.Minute, nil
	}
	parsed, err := time.ParseDuration(h.Every)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("health.every must be a positive duration: %w", err)
	}
	return parsed, nil
}

// RequestTimeout parses the per-request timeout, defaulting to 10 seconds.
func (h Health) RequestTimeout() (time.Duration, error) {
	if h.Timeout == "" {
		return 10 * time.Second, nil
	}
	parsed, err := time.ParseDuration(h.Timeout)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("health.timeout must be a positive duration: %w", err)
	}
	return parsed, nil
}

// ResolveAlerts expands the alert spool directory like other configured paths
// (home expansion, relative to the config file). An empty directory disables
// alerting.
func (c Config) ResolveAlerts() (string, error) {
	if c.Alerts.Directory == "" {
		return "", nil
	}
	return resolveConfigPath(c.Alerts.Directory, filepath.Dir(c.path))
}

type GitHub struct {
	Repositories map[string]string `toml:"repositories"`
}

type TriggerDefinitions struct {
	GitHub   map[string]GitHubTrigger   `toml:"github"`
	Issue    map[string]IssueTrigger    `toml:"issue"`
	Interval map[string]IntervalTrigger `toml:"interval"`
	Cron     map[string]CronTrigger     `toml:"cron"`
}

type TriggerSelection struct {
	Command string `toml:"command"`
	Model   string `toml:"model"`
}

type GitHubTrigger struct {
	TriggerSelection
	Every string `toml:"every"`
	Label string `toml:"label"`
}

type IssueTrigger struct {
	TriggerSelection
	Every      string `toml:"every"`
	Source     string `toml:"source"`
	Project    string `toml:"project"`
	Repository string `toml:"repository"`
}

type IntervalTrigger struct {
	TriggerSelection
	Every      string `toml:"every"`
	Repository string `toml:"repository"`
	Prompt     string `toml:"prompt"`
}

type CronTrigger struct {
	TriggerSelection
	Schedule   string `toml:"schedule"`
	Timezone   string `toml:"timezone"`
	Repository string `toml:"repository"`
	Prompt     string `toml:"prompt"`
}

type ResolvedTrigger struct {
	Identity           string
	Family             string
	Name               string
	Repository         string
	GitHubRepository   string
	GitHubRepositories map[string]string
	SourceKind         string
	SourceProject      string
	Every              time.Duration
	Schedule           string
	Timezone           string
	Label              string
	SelectionName      string
	Model              string
	Prompt             string
	Command            ResolvedCommand
	Signature          string
	cron               *triggers.Cron
}

type Command struct {
	Executor   string `toml:"executor"`
	PromptFile string `toml:"prompt_file"`
	Timeout    string `toml:"timeout"`
}

type ResolvedCommand struct {
	Name       string
	Executor   string
	Command    []string
	Model      string
	Prompt     string
	Timeout    time.Duration
	Definition string
	Hash       string
}

func LoadWorker(path string) (Worker, error) {
	worker := Worker{}
	if path == "" {
		defaultPath, err := defaultConfigPath("worker.toml")
		if err != nil {
			return Worker{}, err
		}
		path = defaultPath
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			worker.configDir = filepath.Dir(path)
			return applyWorkerDefaults(worker)
		} else if err != nil {
			return Worker{}, fmt.Errorf("inspect worker config: %w", err)
		}
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return Worker{}, fmt.Errorf("resolve worker config: %w", err)
	}
	body, err := readBoundedFile(absPath, maxConfigBytes)
	if err != nil {
		return Worker{}, fmt.Errorf("read worker config %q: %w", absPath, err)
	}
	body, err = mergeConfigOverlay(absPath, body, workerConfigOverlayEnv, workerOverlayName)
	if err != nil {
		return Worker{}, err
	}
	decoder := toml.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&worker); err != nil {
		return Worker{}, fmt.Errorf("parse worker config %q: %w", absPath, err)
	}
	worker.configDir = filepath.Dir(absPath)
	return applyWorkerDefaults(worker)
}

func LoadConfig(path string) (Config, error) {
	if path == "" {
		defaultPath, err := defaultConfigPath("config.toml")
		if err != nil {
			return Config{}, err
		}
		path = defaultPath
	}
	machinistConfig, err := loadConfigFile(path)
	if err != nil {
		return Config{}, err
	}
	machinistConfig.Server.configDir = filepath.Dir(machinistConfig.path)
	machinistConfig.Server, err = applyServerDefaults(machinistConfig.Server)
	if err != nil {
		return Config{}, err
	}
	return machinistConfig, nil
}

func loadConfigFile(path string) (Config, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("resolve Machinist config: %w", err)
	}
	body, err := readBoundedFile(absPath, maxConfigBytes)
	if err != nil {
		return Config{}, fmt.Errorf("read Machinist config %q: %w", absPath, err)
	}
	body, err = mergeConfigOverlay(absPath, body, configOverlayEnv, configOverlayName)
	if err != nil {
		return Config{}, err
	}
	var raw map[string]any
	if err := toml.Unmarshal(body, &raw); err != nil {
		return Config{}, fmt.Errorf("parse Machinist config %q: %w", absPath, err)
	}
	if _, ok := raw["pipelines"]; ok {
		return Config{}, fmt.Errorf("parse Machinist config %q: pipelines were removed; replace each pipeline with a repository-owned orchestration script configured under [commands]", absPath)
	}
	if _, ok := raw["shepherd"]; ok {
		return Config{}, fmt.Errorf("parse Machinist config %q: shepherd schedules were removed; schedule a command with a [triggers.cron.NAME] or [triggers.interval.NAME] trigger", absPath)
	}
	if _, ok := raw["agents"]; ok {
		return Config{}, fmt.Errorf("parse Machinist config %q: agents were renamed to commands; move [agents.NAME] definitions to [commands.NAME] and use --command", absPath)
	}
	if err := validateTriggerKeys(raw); err != nil {
		return Config{}, fmt.Errorf("parse Machinist config %q: %w", absPath, err)
	}
	machinistConfig := Config{path: absPath}
	decoder := toml.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&machinistConfig); err != nil {
		return Config{}, fmt.Errorf("parse Machinist config %q: %w", absPath, err)
	}
	for _, name := range machinistConfig.WorkflowNames() {
		if _, err := machinistConfig.ResolveTaskWorkflow(name, ""); err != nil {
			return Config{}, err
		}
	}
	return machinistConfig, nil
}

func (c Config) Path() string { return c.path }

// CommandNames lists the defined command names in sorted order.
func (c Config) CommandNames() []string { return sortedMapKeys(c.Commands) }

func (w Worker) ResolveMachinistConfig(override string) (string, error) {
	if override != "" {
		return resolveConfigPath(override, "")
	}
	return resolveConfigPath("config.toml", w.configDir)
}

func (w Worker) ResolveCommandModel(command ResolvedCommand, requestedModel string) (ResolvedCommand, error) {
	executor, ok := w.Executors[command.Executor]
	if !ok {
		return ResolvedCommand{}, fmt.Errorf("executor %q is not configured on this worker", command.Executor)
	}
	if err := validateCommand(command.Executor, executor.Command); err != nil {
		return ResolvedCommand{}, err
	}
	model, err := resolveModel(command.Executor, executor, requestedModel)
	if err != nil {
		return ResolvedCommand{}, err
	}
	command.Command = make([]string, 0, len(executor.Command))
	for _, argument := range executor.Command {
		if model == "" && strings.Contains(argument, modelParameter) {
			continue
		}
		command.Command = append(command.Command, strings.ReplaceAll(argument, modelParameter, model))
	}
	command.Model = model
	return command, nil
}

func resolveModel(name string, executor Executor, requested string) (string, error) {
	model := strings.TrimSpace(requested)
	if model == "" {
		return "", nil
	}
	if !executor.supportsModel() {
		return "", fmt.Errorf("executor %q does not support model selection; add %s to its command", name, modelParameter)
	}
	if resolved, ok := executor.Models[model]; ok {
		model = strings.TrimSpace(resolved)
	} else if len(executor.Models) > 0 {
		return "", fmt.Errorf("model %q is not configured for executor %q", model, name)
	}
	if model == "" || len(model) > 128 || strings.ContainsAny(model, "\x00\r\n") {
		return "", fmt.Errorf("executor %q model is invalid", name)
	}
	return model, nil
}

func (w Worker) ResolveRepository(name string) (string, error) {
	repository, ok := w.Repositories[name]
	if !ok {
		return "", fmt.Errorf("repository %q is not configured on this worker", name)
	}
	return resolveConfigPath(repository.Path, w.configDir)
}

func (w Worker) WorkerToken() (string, error) {
	if strings.TrimSpace(w.ControlPlane.TokenFile) == "" {
		return "", errors.New("control_plane.token_file is required")
	}
	path, err := resolveConfigPath(w.ControlPlane.TokenFile, w.configDir)
	if err != nil {
		return "", err
	}
	return readToken(path)
}

func (w Worker) ExecutorNames() []string { return sortedMapKeys(w.Executors) }

func (w Worker) ModelCapabilities() map[string][]string {
	capabilities := make(map[string][]string)
	for name, executor := range w.Executors {
		if executor.supportsModel() {
			capabilities[name] = sortedMapKeys(executor.Models)
		}
	}
	return capabilities
}

func (w Worker) RepositoryNames() []string { return sortedMapKeys(w.Repositories) }

func (s Server) WorkerToken() (string, error) {
	return readToken(s.WorkerTokenFile)
}

func (s Server) ConcurrentJobLimit() int {
	if s.MaxConcurrentJobs == nil {
		return 0
	}
	return *s.MaxConcurrentJobs
}

// LeaseGraceDuration parses the restart lease grace (MACH-71), defaulting to
// 3 minutes.
func (s Server) LeaseGraceDuration() (time.Duration, error) {
	if s.LeaseGrace == "" {
		return 3 * time.Minute, nil
	}
	parsed, err := time.ParseDuration(s.LeaseGrace)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("server.lease_grace must be a positive duration: %w", err)
	}
	return parsed, nil
}

func LoadCommand(definitionPath, name string) (ResolvedCommand, error) {
	if strings.TrimSpace(name) == "" {
		return ResolvedCommand{}, errors.New("command name is required")
	}
	definition, err := loadConfigFile(definitionPath)
	if err != nil {
		return ResolvedCommand{}, err
	}
	return definition.ResolveCommand(name)
}

// ResolveCommand resolves one named command from an already loaded definition.
func (c Config) ResolveCommand(name string) (ResolvedCommand, error) {
	command, ok := c.Commands[name]
	if !ok {
		return ResolvedCommand{}, fmt.Errorf("command %q is not defined in %s", name, c.path)
	}
	return resolveCommand(c.path, name, command)
}

func LoadDefinitions(path string) (Config, error) { return loadConfigFile(path) }

func resolveCommand(definitionPath, name string, command Command) (ResolvedCommand, error) {
	if strings.TrimSpace(command.Executor) == "" {
		return ResolvedCommand{}, fmt.Errorf("command %q must define executor", name)
	}
	prompt := promptParameter
	if strings.TrimSpace(command.PromptFile) != "" {
		promptPath, err := expandHome(command.PromptFile)
		if err != nil {
			return ResolvedCommand{}, fmt.Errorf("resolve command %q prompt: %w", name, err)
		}
		if !filepath.IsAbs(promptPath) {
			promptPath = filepath.Join(filepath.Dir(definitionPath), promptPath)
		}
		body, err := readBoundedFile(filepath.Clean(promptPath), maxPromptBytes)
		if err != nil {
			return ResolvedCommand{}, fmt.Errorf("read command %q prompt %q: %w", name, promptPath, err)
		}
		prompt = string(body)
		if strings.TrimSpace(prompt) == "" {
			return ResolvedCommand{}, fmt.Errorf("command %q prompt is empty", name)
		}
		if err := validatePromptParameters(name, prompt); err != nil {
			return ResolvedCommand{}, err
		}
	}
	timeout := defaultTimeout
	if command.Timeout != "" {
		var err error
		timeout, err = time.ParseDuration(command.Timeout)
		if err != nil {
			return ResolvedCommand{}, fmt.Errorf("command %q timeout: %w", name, err)
		}
		if timeout <= 0 {
			return ResolvedCommand{}, fmt.Errorf("command %q timeout must be positive", name)
		}
	}

	resolved := ResolvedCommand{
		Name:       name,
		Executor:   command.Executor,
		Prompt:     prompt,
		Timeout:    timeout,
		Definition: definitionPath,
	}
	var err error
	resolved.Hash, err = commandHash(resolved)
	if err != nil {
		return ResolvedCommand{}, err
	}
	return resolved, nil
}

func RenderPrompt(command ResolvedCommand, prompt string) (ResolvedCommand, error) {
	if strings.TrimSpace(prompt) == "" {
		return ResolvedCommand{}, errors.New("prompt is required")
	}
	if len(prompt) > maxInputPromptBytes {
		return ResolvedCommand{}, fmt.Errorf("prompt exceeds %d bytes", maxInputPromptBytes)
	}
	parameterCount := strings.Count(command.Prompt, promptParameter)
	if parameterCount == 0 {
		return ResolvedCommand{}, fmt.Errorf("command %q prompt must include %s", command.Name, promptParameter)
	}
	literalBytes := len(command.Prompt) - parameterCount*len(promptParameter)
	if literalBytes > maxRenderedPromptBytes || len(prompt) > (maxRenderedPromptBytes-literalBytes)/parameterCount {
		return ResolvedCommand{}, fmt.Errorf("rendered command prompt exceeds %d bytes", maxRenderedPromptBytes)
	}
	command.Prompt = strings.ReplaceAll(command.Prompt, promptParameter, prompt)
	return command, nil
}

func validatePromptParameters(commandName, prompt string) error {
	if strings.Contains(prompt, legacyFactoryPrefix) {
		return fmt.Errorf("command %q prompt uses the unsupported legacy Factory parameter namespace", commandName)
	}
	hasPrompt := false
	remaining := prompt
	for {
		start := strings.Index(remaining, machinistParameterPrefix)
		if start < 0 {
			break
		}
		remaining = remaining[start:]
		end := strings.Index(remaining, "}}")
		if end < 0 {
			return fmt.Errorf("command %q prompt contains a malformed Machinist parameter", commandName)
		}
		parameter := remaining[:end+2]
		if parameter != promptParameter {
			return fmt.Errorf("command %q prompt uses unsupported Machinist parameter %q", commandName, parameter)
		}
		hasPrompt = true
		remaining = remaining[end+2:]
	}
	if !hasPrompt && !strings.Contains(prompt, "{{task.") && !strings.Contains(prompt, "{{inputs.") && !strings.Contains(prompt, "{{stage.") {
		return fmt.Errorf("command %q prompt must include %s", commandName, promptParameter)
	}
	return nil
}

func applyWorkerDefaults(worker Worker) (Worker, error) {
	return applyWorkerDefaultsWithHostname(worker, os.Hostname)
}

func applyWorkerDefaultsWithHostname(worker Worker, getHostname func() (string, error)) (Worker, error) {
	worker.Name = strings.TrimSpace(worker.Name)
	if worker.Name == "" {
		hostname, err := getHostname()
		if err != nil {
			return Worker{}, fmt.Errorf("find machine hostname: %w", err)
		}
		worker.Name = strings.TrimSpace(hostname)
		if worker.Name == "" {
			return Worker{}, errors.New("find machine hostname: hostname is empty")
		}
	}
	if worker.DataDirectory == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Worker{}, fmt.Errorf("find user home directory: %w", err)
		}
		worker.DataDirectory = filepath.Join(home, filepath.FromSlash(defaultWorkerDirName))
	}
	dataDirectory, err := expandHome(worker.DataDirectory)
	if err != nil {
		return Worker{}, fmt.Errorf("resolve worker data directory: %w", err)
	}
	if !filepath.IsAbs(dataDirectory) && worker.configDir != "" {
		dataDirectory = filepath.Join(worker.configDir, dataDirectory)
	}
	worker.DataDirectory, err = filepath.Abs(dataDirectory)
	if err != nil {
		return Worker{}, fmt.Errorf("resolve worker data directory: %w", err)
	}
	worker.DataDirectory = filepath.Clean(worker.DataDirectory)
	if worker.RawLogMaxBytes < 0 {
		return Worker{}, errors.New("raw_log_max_bytes must not be negative")
	}
	for name, repository := range worker.Repositories {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(repository.Path) == "" {
			return Worker{}, errors.New("repository names and paths must be non-empty")
		}
		path, err := resolveConfigPath(repository.Path, worker.configDir)
		if err != nil {
			return Worker{}, fmt.Errorf("resolve repository %q: %w", name, err)
		}
		repository.Path = path
		worker.Repositories[name] = repository
	}
	for name, executor := range worker.Executors {
		if err := validateCommand(name, executor.Command); err != nil {
			return Worker{}, err
		}
		for alias, model := range executor.Models {
			if strings.TrimSpace(alias) == "" || strings.TrimSpace(model) == "" {
				return Worker{}, fmt.Errorf("executor %q model aliases and values must be non-empty", name)
			}
		}
		if len(executor.Models) > 0 && !executor.supportsModel() {
			return Worker{}, fmt.Errorf("executor %q defines models but its command does not contain %s", name, modelParameter)
		}
	}
	return worker, nil
}

// isDatabaseURL reports whether the configured database is a connection URL
// (Postgres) rather than a SQLite file path.
func isDatabaseURL(database string) bool {
	trimmed := strings.TrimSpace(database)
	return strings.HasPrefix(trimmed, "postgres://") || strings.HasPrefix(trimmed, "postgresql://")
}

func applyServerDefaults(server Server) (Server, error) {
	if server.Listen == "" {
		server.Listen = "127.0.0.1:7331"
	}
	if server.Database == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Server{}, fmt.Errorf("find user home directory: %w", err)
		}
		server.Database = filepath.Join(home, filepath.FromSlash(defaultServerDirName), "machinist.db")
	} else if isDatabaseURL(server.Database) {
		// A postgres:// (or postgresql://) URL is a connection string, not a
		// filesystem path: it is used verbatim and never home-expanded.
		server.Database = strings.TrimSpace(server.Database)
	} else {
		path, err := resolveConfigPath(server.Database, server.configDir)
		if err != nil {
			return Server{}, fmt.Errorf("resolve database: %w", err)
		}
		server.Database = path
	}
	if strings.TrimSpace(server.WorkerTokenFile) == "" {
		return Server{}, errors.New("worker_token_file is required")
	}
	if server.Auth == "" {
		server.Auth = AuthTokens
	}
	if server.Auth != AuthTokens && server.Auth != AuthNone {
		return Server{}, fmt.Errorf("auth must be %q or %q", AuthTokens, AuthNone)
	}
	if server.MaxConcurrentJobs != nil && *server.MaxConcurrentJobs <= 0 {
		return Server{}, errors.New("max_concurrent_jobs must be positive")
	}
	tokenPath, err := resolveConfigPath(server.WorkerTokenFile, server.configDir)
	if err != nil {
		return Server{}, fmt.Errorf("resolve worker token file: %w", err)
	}
	server.WorkerTokenFile = tokenPath
	return server, nil
}

func validateCommand(name string, command []string) error {
	if len(command) == 0 || strings.TrimSpace(command[0]) == "" {
		return fmt.Errorf("executor %q must define a non-empty command", name)
	}
	for index, argument := range command {
		if strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("executor %q command argument %d contains a null byte", name, index)
		}
		if strings.Contains(argument, legacyFactoryPrefix) {
			return fmt.Errorf("executor %q command uses the unsupported legacy Factory parameter namespace", name)
		}
		if index == 0 && strings.Contains(argument, modelParameter) {
			return fmt.Errorf("executor %q command executable cannot contain %s", name, modelParameter)
		}
		if strings.Contains(argument, modelParameter) && (!strings.HasPrefix(argument, "--") || !strings.HasSuffix(argument, "="+modelParameter) || strings.Count(argument, modelParameter) != 1) {
			return fmt.Errorf("executor %q must use %s as a complete optional --flag=%s argument", name, modelParameter, modelParameter)
		}
	}
	return nil
}

// ValidateListenAuth rejects auth opt-out on untrusted interfaces. Auth-off
// servers may only bind loopback or the Tailscale CGNAT range; token auth may
// bind any loopback or Tailscale address. Public binds stay rejected.
func ValidateListenAuth(auth, listen string) error {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid listen address %q: %w", listen, err)
	}
	ip := net.ParseIP(host)
	trusted := host == "localhost" || (ip != nil && ip.IsLoopback())
	if !trusted && ip != nil {
		if ip4 := ip.To4(); ip4 != nil && ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
			trusted = true
		}
	}
	if !trusted {
		return fmt.Errorf("listen address %q must use a loopback or Tailscale host", listen)
	}
	return nil
}

func resolveConfigPath(path, base string) (string, error) {
	path, err := expandHome(path)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) && base != "" {
		path = filepath.Join(base, path)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absPath), nil
}

func readToken(path string) (string, error) {
	body, err := readBoundedFile(path, maxTokenBytes)
	if err != nil {
		return "", fmt.Errorf("read token file %q: %w", path, err)
	}
	token := strings.TrimSpace(string(body))
	if token == "" {
		return "", fmt.Errorf("token file %q is empty", path)
	}
	return token, nil
}

func sortedMapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func defaultConfigPath(name string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find user home directory: %w", err)
	}
	return filepath.Join(home, ".machinist", name), nil
}

func expandHome(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find user home directory: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, filepath.FromSlash(strings.TrimPrefix(path, "~/"))), nil
	}
	return path, nil
}

// mergeConfigOverlay returns the base config body with the instance overlay
// merged on top (base first, overlay second, overlay wins for scalars and
// arrays; tables merge key by key). The overlay is MACHINIST_*_OVERLAY when
// set, else the sibling <name> next to the base file when it exists. With no
// overlay the base body is returned untouched.
func mergeConfigOverlay(absPath string, body []byte, envKey, name string) ([]byte, error) {
	overlayPath := strings.TrimSpace(os.Getenv(envKey))
	explicit := overlayPath != ""
	if !explicit {
		overlayPath = filepath.Join(filepath.Dir(absPath), name)
	}
	overlayBody, err := readOptionalFile(overlayPath)
	if err != nil {
		return nil, fmt.Errorf("read config overlay %q: %w", overlayPath, err)
	}
	if overlayBody == nil {
		if explicit {
			return nil, fmt.Errorf("config overlay %q does not exist", overlayPath)
		}
		return body, nil
	}
	var base, overlay map[string]any
	if err := toml.Unmarshal(body, &base); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", absPath, err)
	}
	if err := toml.Unmarshal(overlayBody, &overlay); err != nil {
		return nil, fmt.Errorf("parse config overlay %q: %w", overlayPath, err)
	}
	merged, err := toml.Marshal(mergeTOML(base, overlay))
	if err != nil {
		return nil, fmt.Errorf("merge config overlay %q: %w", overlayPath, err)
	}
	return merged, nil
}

// mergeTOML deep-merges overlay into base and returns base. Nested tables
// merge; every other value (including arrays) is replaced by the overlay.
func mergeTOML(base, overlay map[string]any) map[string]any {
	for key, value := range overlay {
		if overlayTable, ok := value.(map[string]any); ok {
			if baseTable, ok := base[key].(map[string]any); ok {
				base[key] = mergeTOML(baseTable, overlayTable)
				continue
			}
		}
		base[key] = value
	}
	return base
}

// readOptionalFile reads path as a bounded config file. A missing file is not
// an error and returns nil so an absent sibling overlay is a no-op.
func readOptionalFile(path string) ([]byte, error) {
	body, err := readBoundedFile(path, maxConfigBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return body, nil
}

func readBoundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return body, nil
}

func commandHash(command ResolvedCommand) (string, error) {
	payload, err := json.Marshal(struct {
		Name     string        `json:"name"`
		Executor string        `json:"executor"`
		Prompt   string        `json:"prompt"`
		Timeout  time.Duration `json:"timeout"`
	}{command.Name, command.Executor, command.Prompt, command.Timeout})
	if err != nil {
		return "", fmt.Errorf("encode command definition: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
