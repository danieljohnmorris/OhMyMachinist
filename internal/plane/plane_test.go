package plane

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func sign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return hex.EncodeToString(mac.Sum(nil))
}

func TestVerifySignatureV1RawAndReserialised(t *testing.T) {
	const secret = "hunter2"
	raw := `{"event":"issue","action":"create","data":{"id":"aaaaaaaa-1111-1111-1111-111111111111","name":"T","sequence_id":7}}`
	if !VerifySignature(secret, []byte(raw), sign(secret, raw)) {
		t.Fatal("raw v1 delivery should verify")
	}
	// Plane CE signs json.dumps(payload); the re-serialised body must verify too.
	reencoded := `{"event": "issue", "action": "create", "data": {"id": "aaaaaaaa-1111-1111-1111-111111111111", "name": "T", "sequence_id": 7}}`
	if reencoded == raw {
		t.Fatal("test fixture must differ from raw")
	}
	if !VerifySignature(secret, []byte(reencoded), sign(secret, raw)) {
		t.Fatal("re-serialised delivery should verify")
	}
	if !VerifySignature(secret, []byte(raw), "sha256="+sign(secret, raw)) {
		t.Fatal("sha256= prefixed signature should verify")
	}
	if VerifySignature(secret, []byte(raw), sign("other", raw)) {
		t.Fatal("wrong secret must not verify")
	}
	if VerifySignature(secret, []byte(raw), "") || VerifySignature("", []byte(raw), sign(secret, raw)) {
		t.Fatal("missing secret or header must not verify")
	}
}

func TestParseWebhookV1AndV2(t *testing.T) {
	v1 := `{"event":"issue","action":"update","data":{"id":"i-1","name":"Fix","sequence_id":12,"project":"p-1","description_html":"<p>Do it</p>","labels":[{"id":"l1","name":"factory"}],"state":{"id":"s1","name":"Todo","group":"unstarted"}}}`
	event, issue, ok, err := ParseWebhook([]byte(v1), "delivery-1")
	if err != nil || !ok {
		t.Fatalf("v1 delivery: ok=%v err=%v", ok, err)
	}
	if event.Name != "issue" || event.Action != "update" || event.Delivery != "delivery-1" {
		t.Fatalf("unexpected event %+v", event)
	}
	if issue.ProjectID != "p-1" || issue.SequenceID != 12 || issue.StateGroup != "unstarted" {
		t.Fatalf("unexpected issue %+v", issue)
	}
	if got := Spec(issue); got != "Do it" {
		t.Fatalf("spec = %q", got)
	}

	v2 := `{"event":"workitem.created","action":"created","data":{"id":"i-2","name":"New","sequence_id":3,"project":"p-2"}}`
	event, _, ok, err = ParseWebhook([]byte(v2), "")
	if err != nil || !ok || event.Name != "workitem.created" {
		t.Fatalf("v2 delivery: ok=%v err=%v event=%+v", ok, err, event)
	}

	ignored := `{"event":"comment.created","action":"created","data":{"id":"i-3"}}`
	if _, _, ok, _ := ParseWebhook([]byte(ignored), ""); ok {
		t.Fatal("comment delivery must not be treated as an issue event")
	}
}

func TestHTMLToText(t *testing.T) {
	cases := []struct{ name, input, want string }{
		{"headings and code", `<h2>Title</h2><p>Hello <code>x</code></p>`, "## Title\n\nHello `x`"},
		{"lists", `<ul><li>one</li><li>two</li></ul>`, "- one\n\n- two"},
		{"checkboxes", `<li><input type="checkbox" checked>done</li><li><input type="checkbox">todo</li>`, "- [x] done\n\n- [ ] todo"},
		{"entities", `<p>a &amp; b &lt;c&gt;</p>`, "a & b <c>"},
		{"empty", "", ""},
	}
	for _, test := range cases {
		if got := HTMLToText(test.input); got != test.want {
			t.Errorf("%s: HTMLToText(%q) = %q, want %q", test.name, test.input, got, test.want)
		}
	}
}

func TestSpecExtractionAndHash(t *testing.T) {
	issue := Issue{Name: "T", DescriptionHTML: "<h1>Spec</h1><p>Body</p>"}
	if got := Spec(issue); got != "# Spec\n\nBody" {
		t.Fatalf("spec = %q", got)
	}
	stripped := Issue{Name: "T", DescriptionStripped: "plain text"}
	if got := Spec(stripped); got != "plain text" {
		t.Fatalf("stripped fallback = %q", got)
	}
	if Spec(Issue{Name: "T"}) != "" {
		t.Fatal("no description should yield an empty spec")
	}
	base := SpecHash("T", "Body")
	if len(base) != 8 {
		t.Fatalf("hash %q is not 8 characters", base)
	}
	if SpecHash("T", "Body") != base {
		t.Fatal("hash must be stable")
	}
	if SpecHash("T2", "Body") == base || SpecHash("T", "Body2") == base {
		t.Fatal("name or spec change must change the hash")
	}
}

func TestTriggerAndApproveRules(t *testing.T) {
	const triggerID = DefaultTriggerLabelID
	triggered := Issue{Labels: []Label{{ID: "x", Name: "factory"}}}
	if !Triggered(triggered, "factory", nil) {
		t.Fatal("label name should trigger")
	}
	byID := Issue{Labels: []Label{{ID: triggerID, Name: "Factory Queue"}}}
	if !Triggered(byID, "factory", []string{triggerID}) {
		t.Fatal("configured label UUID should trigger")
	}
	none := Issue{Labels: []Label{{ID: "x", Name: "bug"}}}
	if Triggered(none, "factory", nil) {
		t.Fatal("unrelated label must not trigger")
	}
	if !Eligible(triggered, "factory", nil) {
		t.Fatal("clean triggered issue should be eligible")
	}
	if Eligible(Issue{IsDraft: true, Labels: triggered.Labels}, "factory", nil) {
		t.Fatal("draft must not be eligible")
	}
	if Eligible(Issue{DeletedAt: "2026-01-01", Labels: triggered.Labels}, "factory", nil) {
		t.Fatal("deleted issue must not be eligible")
	}

	approved := Issue{Labels: []Label{{Name: "approved"}}}
	if !NeedsApproveBridge(approved, "approved", nil, "factory", nil) {
		t.Fatal("approved without factory should bridge")
	}
	both := Issue{Labels: []Label{{Name: "approved"}, {Name: "factory"}}}
	if NeedsApproveBridge(both, "approved", nil, "factory", nil) {
		t.Fatal("already triggered issue must not bridge")
	}
	byIDTrigger := Issue{Labels: []Label{{Name: "approved"}, {ID: triggerID}}}
	if NeedsApproveBridge(byIDTrigger, "approved", nil, "factory", []string{triggerID}) {
		t.Fatal("trigger label by UUID must prevent bridging")
	}
}

func TestRepoMapAndUnmapped(t *testing.T) {
	m := RepoMap{
		Repositories: map[string]string{"p-1": "factory-smoke", "MACH": "OhMyMachinist"},
		Identifiers:  map[string]string{"p-1": "DAN"},
	}
	if repo, ok := m.RepoFor("p-1", "DAN"); !ok || repo != "factory-smoke" {
		t.Fatalf("project id mapping = %q ok=%v", repo, ok)
	}
	if repo, ok := m.RepoFor("p-9", "MACH"); !ok || repo != "OhMyMachinist" {
		t.Fatalf("identifier mapping = %q ok=%v", repo, ok)
	}
	if _, ok := m.RepoFor("unmapped", "NOPE"); ok {
		t.Fatal("unmapped project without default must not resolve")
	}
	withDefault := RepoMap{Repositories: m.Repositories, Default: "fallback"}
	if repo, ok := withDefault.RepoFor("unmapped", "NOPE"); !ok || repo != "fallback" {
		t.Fatalf("default mapping = %q ok=%v", repo, ok)
	}
	if got := m.IdentifierFor("p-1"); got != "DAN" {
		t.Fatalf("identifier = %q", got)
	}
	if got := m.IdentifierFor("abcdef123456"); got != "abcdef12" {
		t.Fatalf("identifier fallback = %q", got)
	}
}

func TestSupervisorMap(t *testing.T) {
	m := SupervisorMap{"OMM": "Om", "ILO": "ilo", "_default": "LocalAI"}
	if got := m.SupervisorFor("OMM-12"); got != "Om" {
		t.Fatalf("OMM supervisor = %q", got)
	}
	if got := m.SupervisorFor("MACH-30"); got != "LocalAI" {
		t.Fatalf("default supervisor = %q", got)
	}
	if got := (SupervisorMap{}).SupervisorFor("ANY-1"); got != DefaultSupervisor {
		t.Fatalf("empty map supervisor = %q", got)
	}
}

func TestParsePullRequest(t *testing.T) {
	plain := `{"type":"process.output","data":"working\ndeliver-pr: PR: https://github.com/dan/repo/pull/9\n"}
{"type":"process.output","data":"no marker"}`
	if url, ok := ParsePullRequest(plain); !ok || url != "https://github.com/dan/repo/pull/9" {
		t.Fatalf("plain parse = %q ok=%v", url, ok)
	}
	encoded, _ := json.Marshal(map[string]string{"type": "process.output", "data": "deliver-pr: PR: https://github.com/dan/repo/pull/10", "encoding": "base64"})
	// data must be the base64 of the marker text.
	event := `{"type":"process.output","data":"ZGVsaXZlci1wcjogUFI6IGh0dHBzOi8vZ2l0aHViLmNvbS9kYW4vcmVwby9wdWxsLzEw","encoding":"base64"}`
	if url, ok := ParsePullRequest(event); !ok || url != "https://github.com/dan/repo/pull/10" {
		t.Fatalf("base64 parse = %q ok=%v", url, ok)
	}
	_ = encoded
	if _, ok := ParsePullRequest("not json"); ok {
		t.Fatal("invalid events must parse to no PR")
	}
}

func TestStateSyncTransitions(t *testing.T) {
	if phase, ok := PhaseFor("running"); !ok || phase != PhaseStart {
		t.Fatalf("running phase = %q ok=%v", phase, ok)
	}
	if phase, ok := PhaseFor("succeeded"); !ok || phase != PhaseReview {
		t.Fatalf("succeeded phase = %q ok=%v", phase, ok)
	}
	if phase, ok := PhaseFor("failed"); !ok || phase != PhaseFailed {
		t.Fatalf("failed phase = %q ok=%v", phase, ok)
	}
	if _, ok := PhaseFor("queued"); ok {
		t.Fatal("queued must not sync a ticket")
	}
	if !StartMovesState("backlog") || !StartMovesState("unstarted") {
		t.Fatal("backlog/unstarted should move to progress")
	}
	if StartMovesState("started") || StartMovesState("completed") {
		t.Fatal("only backlog/unstarted move to progress")
	}
	if !ReviewMovesState("started") || !ReviewMovesState("backlog") {
		t.Fatal("unfinished tickets should move to review")
	}
	if ReviewMovesState("completed") || ReviewMovesState("cancelled") {
		t.Fatal("Done/Cancelled tickets must never be touched")
	}
	if patched, ok := FailedLabelPatch([]string{"a"}, "failed-id"); !ok || len(patched) != 2 {
		t.Fatalf("failed patch = %v ok=%v", patched, ok)
	}
	if _, ok := FailedLabelPatch([]string{"failed-id"}, "failed-id"); ok {
		t.Fatal("failed label already present must not patch")
	}
	if patched, ok := DropFailedLabelPatch([]string{"failed-id", "a"}, "failed-id"); !ok || len(patched) != 1 || patched[0] != "a" {
		t.Fatalf("drop patch = %v ok=%v", patched, ok)
	}
}

func TestAlerts(t *testing.T) {
	alert := NewPROpenedAlert("Om", "OMM-7", "https://plane/x/OMM-7/", "https://github.com/dan/repo/pull/1", "job_ab", "mobile", "http://ui/#/runs/job_ab", "2026-01-01T00:00:00Z", false)
	if alert.Type != "machinist.pr_opened" || alert.Supervisor != "Om" {
		t.Fatalf("unexpected alert %+v", alert)
	}
	if !strings.Contains(alert.Message, "do not merge without Daniel") {
		t.Fatalf("manual-merge message = %q", alert.Message)
	}
	auto := NewPROpenedAlert("Om", "OMM-7", "u", "pr", "job_ab", "mobile", "run", "ts", true)
	if strings.Contains(auto.Message, "do not merge without Daniel") {
		t.Fatalf("auto-merge message = %q", auto.Message)
	}
}

func TestRenderPromptAndComment(t *testing.T) {
	rendered := RenderPrompt("", "https://plane/x/MACH-1/", "MACH-1", "Fix it", "# body")
	for _, want := range []string{"https://plane/x/MACH-1/", "MACH-1 Fix it", "# body"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("prompt missing %q:\n%s", want, rendered)
		}
	}
	if !strings.HasPrefix(rendered, "You are the CODER") {
		t.Fatal("default prompt should be the coder template")
	}
	custom := RenderPrompt("Spec: {url} {key} {name}\n{spec}", "u", "k", "n", "s")
	if custom != "Spec: u k n\ns" {
		t.Fatalf("custom prompt = %q", custom)
	}
	comment := StartCommentHTML("MACH-1", "job_ab", "http://ui/#/runs/job_ab")
	if !strings.Contains(comment, "Machinist run started") || !strings.Contains(comment, "job_ab") {
		t.Fatalf("comment = %q", comment)
	}
	if RunURL("http://ui/", "job_ab") != "http://ui/#/runs/job_ab" {
		t.Fatal("run URL mismatch")
	}
}
