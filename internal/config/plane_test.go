package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultMergePoliciesSeed(t *testing.T) {
	policies := DefaultMergePolicies()
	mach, ok := policies["MACH"]
	if !ok || !mach.AutoMerge {
		t.Fatalf("MACH seed = %+v, want auto-merge on", mach)
	}
	if mach.DeployCommand == "" || !strings.Contains(mach.DeployCommand, "/opt/machinist/bin") {
		t.Fatalf("MACH deploy command = %q", mach.DeployCommand)
	}
	if !strings.Contains(mach.DeployCommand, "systemctl restart machinist-control") {
		t.Fatal("MACH deploy must restart the control plane")
	}
	if !strings.Contains(mach.DeployCommand, "backups") {
		t.Fatal("MACH deploy must back up the database")
	}
	if mach.HealthCheck == nil || !strings.Contains(mach.HealthCheck.Command, "{sha}") {
		t.Fatalf("MACH health check = %+v, want a {sha} command", mach.HealthCheck)
	}
	spark, ok := policies["SPARK"]
	if !ok || !spark.AutoMerge {
		t.Fatalf("SPARK seed = %+v, want auto-merge on", spark)
	}
	if !strings.Contains(spark.DeployCommand, "100.76.111.74") || !strings.Contains(spark.DeployCommand, "docker compose up --build -d") {
		t.Fatalf("SPARK deploy command = %q", spark.DeployCommand)
	}
	if spark.HealthCheck == nil || spark.HealthCheck.URL != "http://100.76.111.74:5555/" || spark.HealthCheck.Status != 200 {
		t.Fatalf("SPARK health check = %+v", spark.HealthCheck)
	}
}

func TestResolveMergePoliciesDefaultsAndOverride(t *testing.T) {
	policies := ResolveMergePolicies(nil)
	if !policies["MACH"].AutoMerge || !policies["SPARK"].AutoMerge {
		t.Fatal("seeds must be on with no configuration")
	}
	if policies["UNKNOWN-PROJECT"].AutoMerge {
		t.Fatal("unknown projects must default to off")
	}
	for name, policy := range policies {
		if policy.MergeMethod != "squash" {
			t.Fatalf("%s merge method = %q, want normalized squash", name, policy.MergeMethod)
		}
	}
	// A configured entry replaces the seed for that project and normalizes.
	policies = ResolveMergePolicies(map[string]MergePolicy{
		"MACH":  {AutoMerge: false},
		"spark": {AutoMerge: true, MergeMethod: " merge "},
	})
	if policies["MACH"].AutoMerge {
		t.Fatal("configured MACH entry must replace the seed")
	}
	if !policies["SPARK"].AutoMerge || policies["SPARK"].MergeMethod != "merge" {
		t.Fatalf("SPARK policy = %+v", policies["SPARK"])
	}
}

func TestMergePolicyDeployDeadline(t *testing.T) {
	if got := (MergePolicy{}).DeployDeadline(); got != DefaultDeployTimeout {
		t.Fatalf("default deadline = %v", got)
	}
	if got := (MergePolicy{DeployTimeout: "30s"}).DeployDeadline(); got != 30*time.Second {
		t.Fatalf("configured deadline = %v", got)
	}
	if got := (MergePolicy{DeployTimeout: "bogus"}).DeployDeadline(); got != DefaultDeployTimeout {
		t.Fatalf("bogus deadline = %v", got)
	}
}

func TestMergePoliciesFromSettings(t *testing.T) {
	configured, err := MergePoliciesFromSettings(`{"LINEAR":{"auto_merge":true}}`, map[string]string{
		"plane.merge_policy.mach": `{"auto_merge":false}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !configured["LINEAR"].AutoMerge {
		t.Fatal("whole-map entry lost")
	}
	if configured["MACH"].AutoMerge {
		t.Fatal("per-project override lost")
	}
	if _, err := MergePoliciesFromSettings("not json", nil); err == nil {
		t.Fatal("malformed map JSON must error")
	}
	if _, err := MergePoliciesFromSettings("", map[string]string{"plane.merge_policy.mach": "not json"}); err == nil {
		t.Fatal("malformed project JSON must error")
	}
}

func TestLoadConfigMergePolicies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	definition := `
[plane.merge_policies.LINEAR]
auto_merge = true
merge_method = "rebase"
deploy_command = "make deploy"

[plane.merge_policies.LINEAR.health_check]
url = "https://linear.example/health"
status = 200
expect = "ok"
`
	if err := os.WriteFile(path, []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	definitions, err := LoadDefinitions(path)
	if err != nil {
		t.Fatal(err)
	}
	policy, ok := definitions.Plane.MergePolicies["LINEAR"]
	if !ok || !policy.AutoMerge || policy.MergeMethod != "rebase" || policy.DeployCommand != "make deploy" {
		t.Fatalf("policy = %+v", policy)
	}
	if policy.HealthCheck == nil || policy.HealthCheck.URL != "https://linear.example/health" || policy.HealthCheck.Expect != "ok" {
		t.Fatalf("health check = %+v", policy.HealthCheck)
	}
	normalized := ResolveMergePolicies(definitions.Plane.MergePolicies)
	if normalized["LINEAR"].DeployDeadline() != DefaultDeployTimeout {
		t.Fatalf("normalized deadline = %v", normalized["LINEAR"].DeployDeadline())
	}
}
