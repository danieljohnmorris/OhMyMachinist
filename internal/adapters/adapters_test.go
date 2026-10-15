package adapters

import (
	"strings"
	"testing"
)

func TestValidateAcceptsSupportedSetup(t *testing.T) {
	setting := FactorySetting{
		Tracker:     TrackerLinear,
		CodingAgent: AgentSpec{Kind: AgentCodex, Executor: "codex"},
		ReviewAgent: AgentSpec{Kind: AgentOMP, Command: "omp --model {{model}} -"},
		Models: map[string]ModelSpec{
			RoleCoding: {Model: "glm-5", Effort: "high", Endpoint: EndpointLocal, URL: "http://spark:8080"},
			RoleReview: {Model: "gpt-5", Effort: "medium", Endpoint: EndpointCloud},
		},
	}
	if err := Validate(setting); err != nil {
		t.Fatalf("Validate(%+v) = %v, want nil", setting, err)
	}
}

func TestValidateRejectsUnknownValues(t *testing.T) {
	cases := map[string]FactorySetting{
		"tracker":     {Tracker: "jira"},
		"agent kind":  {CodingAgent: AgentSpec{Kind: "aider", Executor: "aider"}},
		"agent shape": {CodingAgent: AgentSpec{Kind: AgentOMP}},
		"role":        {Models: map[string]ModelSpec{"deploy": {Model: "x"}}},
		"endpoint":    {Models: map[string]ModelSpec{RoleCoding: {Model: "x", Endpoint: "onprem"}}},
		"local url":   {Models: map[string]ModelSpec{RoleCoding: {Model: "x", Endpoint: EndpointLocal}}},
		"newline":     {CodingAgent: AgentSpec{Kind: AgentOMP, Executor: "omp\nevil"}},
	}
	for name, setting := range cases {
		if err := Validate(setting); err == nil {
			t.Errorf("Validate(%s) = nil, want error", name)
		}
	}
}

func TestValidateAllowsEmptySetting(t *testing.T) {
	if err := Validate(FactorySetting{}); err != nil {
		t.Fatalf("Validate(empty) = %v, want nil", err)
	}
}

func TestValidateAllowsReviewAgentKindAlone(t *testing.T) {
	if err := Validate(FactorySetting{ReviewAgent: AgentSpec{Kind: AgentOMP}}); err != nil {
		t.Fatalf("Validate(review agent kind only) = %v, want nil", err)
	}
	if err := Validate(FactorySetting{ReviewAgent: AgentSpec{Kind: "aider"}}); err == nil {
		t.Fatalf("Validate(unknown review kind) = nil, want error")
	}
}

// A review agent must render a command, so a spec that would resolve to no
// review executor is rejected instead of being silently dropped.
func TestValidateRejectsUnrenderableReviewAgent(t *testing.T) {
	cases := map[string]AgentSpec{
		"command without kind":  {Command: "my-reviewer --model {{model}}"},
		"executor without kind": {Executor: "omp"},
		"command kind no command": {
			Kind: AgentCommand,
		},
	}
	for name, spec := range cases {
		if err := Validate(FactorySetting{ReviewAgent: spec}); err == nil {
			t.Errorf("Validate(review agent %s) = nil, want error", name)
		}
	}
}

func TestNormalizeTrimsAndDropsEmptyEntries(t *testing.T) {
	setting := Normalize(FactorySetting{
		Tracker:     "  plane ",
		CodingAgent: AgentSpec{Kind: " omp ", Executor: " omp "},
		Models: map[string]ModelSpec{
			RoleCoding:    {Model: " glm-5 ", Endpoint: " local ", URL: " http://spark "},
			RoleNonCoding: {},
		},
	})
	if setting.Tracker != TrackerPlane {
		t.Errorf("tracker = %q, want plane", setting.Tracker)
	}
	if setting.CodingAgent.Kind != AgentOMP || setting.CodingAgent.Executor != "omp" {
		t.Errorf("coding agent = %+v, want trimmed omp", setting.CodingAgent)
	}
	if _, ok := setting.Models[RoleNonCoding]; ok {
		t.Errorf("empty non_coding model should be dropped: %+v", setting.Models)
	}
	got := setting.Models[RoleCoding]
	if got.Model != "glm-5" || got.URL != "http://spark" {
		t.Errorf("coding model = %+v, want trimmed values", got)
	}
}

func TestRenderCommandSubstitutesModelAndKeepsEnvPlaceholders(t *testing.T) {
	model := ModelSpec{Model: "glm-5", Effort: "high", URL: "http://spark:8080"}
	got := RenderCommand("omp --model {{model}} --effort {{effort}} --api-key {{env.GLM_KEY}}", model, nil)
	if !strings.Contains(got, "--model glm-5") || !strings.Contains(got, "--effort high") {
		t.Errorf("rendered command = %q, want model and effort substituted", got)
	}
	if !strings.Contains(got, "{{env.GLM_KEY}}") {
		t.Errorf("rendered command = %q, want env placeholder left for the worker", got)
	}
	expanded := RenderCommand("r {{env.GLM_KEY}}", model, func(name string) string { return "secret-" + name })
	if expanded != "r secret-GLM_KEY" {
		t.Errorf("expanded command = %q, want secret substitution", expanded)
	}
}

func TestAgentForUsesDefaultReviewCommandPerKind(t *testing.T) {
	for kind, want := range map[string]string{
		AgentOMP:   "omp --model glm-5 -",
		AgentCodex: "codex exec --model glm-5",
	} {
		agent, ok := AgentFor(kind)
		if !ok {
			t.Fatalf("AgentFor(%q) not found", kind)
		}
		if got := agent.ReviewCommand(AgentSpec{Kind: kind}, ModelSpec{Model: "glm-5"}); got != want {
			t.Errorf("%s review command = %q, want %q", kind, got, want)
		}
	}
	if _, ok := AgentFor("aider"); ok {
		t.Errorf("AgentFor(aider) = found, want missing")
	}
}

func TestCatalogHelpers(t *testing.T) {
	if got := DocumentedTrackers(); len(got) != 2 || got[0] != TrackerLinear {
		t.Errorf("DocumentedTrackers() = %v, want [linear plane]", got)
	}
	if got := Roles(); len(got) != 3 {
		t.Errorf("Roles() = %v, want 3 roles", got)
	}
}
