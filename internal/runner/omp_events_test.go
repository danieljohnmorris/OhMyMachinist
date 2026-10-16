package runner

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/owainlewis/machinist/internal/config"
)

// ompStreamJSONL is a trimmed capture of real `omp --mode json` output: one
// thinking+tool-call message, the tool result and the final assistant message.
const ompStreamJSONL = `{"type":"session","version":3,"id":"abc","cwd":"/tmp"}
{"type":"agent_start"}
{"type":"turn_start"}
{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"ignored"}}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"thinking","thinking":"Let me run echo."},{"type":"toolCall","id":"call_1","name":"bash","arguments":{"command":"echo hello","i":"Running echo"}}],"usage":{"totalTokens":7}}}
{"type":"tool_execution_start","toolCallId":"call_1","toolName":"bash","args":{"command":"echo hello"}}
{"type":"tool_execution_end","toolCallId":"call_1","toolName":"bash","result":{"content":[{"type":"text","text":"hello\n"}]},"isError":false}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"usage":{"totalTokens":42}}}
{"type":"turn_end","toolResults":[]}
{"type":"agent_end","isTerminal":true,"yielded":true}`

func TestOmpStreamParserEmitsTypedTranscriptEvents(t *testing.T) {
	log, path := newTestEventLog(t)
	parser := newOmpStreamParser(log)
	if _, err := parser.Write([]byte(ompStreamJSONL)); err != nil {
		t.Fatal(err)
	}
	if err := log.close(); err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, path)
	var kinds []string
	var toolCall, toolResult Event
	for _, event := range events {
		switch event.Type {
		case "agent.message", "agent.thinking", "agent.tool_call", "agent.tool_result":
			kinds = append(kinds, event.Type)
			if event.Type == "agent.tool_call" {
				toolCall = event
			}
			if event.Type == "agent.tool_result" {
				toolResult = event
			}
		}
	}
	want := []string{"agent.thinking", "agent.tool_call", "agent.tool_result", "agent.message"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v", kinds)
	}
	if toolCall.Tool != "bash" || !strings.Contains(toolCall.Args, `"command":"echo hello"`) {
		t.Fatalf("tool call = %#v", toolCall)
	}
	if toolResult.Tool != "bash" || toolResult.Bytes != len("hello\n") || toolResult.Failed {
		t.Fatalf("tool result = %#v", toolResult)
	}
	if parser.lastMessage() != "Done." {
		t.Fatalf("final message = %q", parser.lastMessage())
	}
	usage := parser.tokenUsage()
	if usage == nil || *usage != 42 {
		t.Fatalf("token usage = %v", usage)
	}
}

// MACH-69: a failed omp retry cycle with a connection-type finalError, and a
// session that produced no output tokens, land in the transcript for the
// controlplane to classify as llm_unavailable.
func TestOmpStreamParserEmitsLLMUnavailableSignals(t *testing.T) {
	log, path := newTestEventLog(t)
	parser := newOmpStreamParser(log)
	stream := `{"type":"session","version":3,"id":"abc","cwd":"/tmp"}
{"type":"agent_start"}
{"type":"auto_retry_end","success":false,"attempt":10,"finalError":"Unable to connect. Is the computer able to access the url?"}
{"type":"agent_end","isTerminal":true,"yielded":true}
`
	if _, err := parser.Write([]byte(stream)); err != nil {
		t.Fatal(err)
	}
	if err := log.close(); err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, path)
	var retryEnd, agentEnd Event
	for _, event := range events {
		switch event.Type {
		case "agent.auto_retry_end":
			retryEnd = event
		case "agent.agent_end":
			agentEnd = event
		}
	}
	if !retryEnd.Failed || !strings.Contains(retryEnd.Message, "Unable to connect") {
		t.Fatalf("auto_retry_end = %#v", retryEnd)
	}
	if !agentEnd.Failed || agentEnd.Message != "output_tokens=0" {
		t.Fatalf("agent_end = %#v", agentEnd)
	}
}

// A successful retry cycle and a session with output tokens emit nothing.
func TestOmpStreamParserSkipsHealthyRetryCycle(t *testing.T) {
	log, path := newTestEventLog(t)
	parser := newOmpStreamParser(log)
	stream := `{"type":"session","version":3,"id":"abc","cwd":"/tmp"}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"Done."}],"usage":{"totalTokens":42}}}
{"type":"auto_retry_end","success":true,"attempt":2}
{"type":"agent_end","isTerminal":true,"yielded":true}
`
	if _, err := parser.Write([]byte(stream)); err != nil {
		t.Fatal(err)
	}
	if err := log.close(); err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, path)
	for _, event := range events {
		if event.Type == "agent.auto_retry_end" || event.Type == "agent.agent_end" {
			t.Fatalf("unexpected signal event %#v", event)
		}
	}
}

func TestOmpStreamParserTruncatesAndRedactsToolResults(t *testing.T) {
	log, path := newTestEventLog(t)
	parser := newOmpStreamParser(log)
	secret := "ghp_" + strings.Repeat("a", 36)
	body := strings.Repeat("x", ompToolResultBytes+500) + "\\n" + secret
	line := `{"type":"tool_execution_end","toolName":"env","result":{"content":[{"type":"text","text":"` + body + `"}]},"isError":true}`
	if _, err := parser.Write([]byte(line + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := log.close(); err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, path)
	if len(events) != 1 {
		t.Fatalf("events = %#v", events)
	}
	result := events[0]
	wantBytes := ompToolResultBytes + 500 + 1 + len("[REDACTED]")
	if !result.Failed || result.Bytes != wantBytes {
		t.Fatalf("result = %#v", result)
	}
	if !strings.Contains(result.Message, "[truncated:") {
		t.Fatalf("missing truncation marker: %q", result.Message)
	}
	if strings.Contains(result.Message, secret) {
		t.Fatalf("secret leaked: %q", result.Message)
	}
}

func TestOmpStreamParserHandlesPartialAndOversizedLines(t *testing.T) {
	log, path := newTestEventLog(t)
	parser := newOmpStreamParser(log)
	message := `{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"split"}]}}`
	half := len(message) / 2
	if _, err := parser.Write([]byte(message[:half])); err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Write([]byte(message[half:] + "\n" + strings.Repeat("y", ompMaxEventBytes+1) + "\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Write([]byte(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"after"}]}}`)); err != nil {
		t.Fatal(err)
	}
	parser.flush()
	if err := log.close(); err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, path)
	if len(events) != 2 || events[0].Message != "split" || events[1].Message != "after" {
		t.Fatalf("events = %#v", events)
	}
}

func TestStructuredOmpCommandInjectsJSONMode(t *testing.T) {
	direct := []string{"omp", "-p", "--model", "sparks/x", "--auto-approve"}
	want := []string{"omp", "--mode", "json", "-p", "--model", "sparks/x", "--auto-approve"}
	if got := structuredOmpCommand("omp", direct); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("command = %v", got)
	}
	existing := []string{"omp", "-p", "--mode", "rpc"}
	if got := structuredOmpCommand("omp", existing); strings.Join(got, " ") != strings.Join(existing, " ") {
		t.Fatalf("command = %v", got)
	}
	// Wrappers run omp themselves and read their own arguments, so the runner
	// must not inject flags into them.
	wrapper := []string{"/usr/local/lib/machinist-factory/omp-glm", "medium"}
	if got := structuredOmpCommand("omp-glm", wrapper); strings.Join(got, " ") != strings.Join(wrapper, " ") {
		t.Fatalf("command = %v", got)
	}
	if !ompExecutor("omp-glm-pr", wrapper) || !ompExecutor("omp", direct) {
		t.Fatal("omp executors not recognized")
	}
	if ompExecutor("codex", []string{"codex", "exec", "-"}) {
		t.Fatal("codex treated as omp")
	}
}

func TestExecuteStreamsOmpAgentEventsAndSuppressesRawJSON(t *testing.T) {
	repository := newGitRepository(t)
	script := "cat >/dev/null; printf '%s\\n' " + shellSingleQuotedLines(ompStreamJSONL)
	command := config.ResolvedCommand{
		Executor:   "omp-glm-pr",
		Name:       "build-pr",
		Command:    []string{"/bin/sh", "-c", script},
		Prompt:     "go\n",
		Timeout:    5 * time.Second,
		Definition: "/definition/config.toml",
		Hash:       "test-hash",
	}
	var stdout strings.Builder
	var stderr strings.Builder
	result, err := Execute(context.Background(), Options{
		Command:       command,
		Repository:    repository,
		DataDirectory: t.TempDir(),
		Stdout:        &stdout,
		Stderr:        &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != StateSucceeded {
		t.Fatalf("result = %#v", result)
	}
	if result.FinalMessage != "Done." {
		t.Fatalf("final message = %q", result.FinalMessage)
	}
	if result.TokenUsage == nil || *result.TokenUsage != 42 {
		t.Fatalf("token usage = %v", result.TokenUsage)
	}
	events := readEvents(t, result.EventsPath)
	if outputFor(t, events, "stdout") != "" {
		t.Fatalf("raw JSON stdout was recorded: %q", outputFor(t, events, "stdout"))
	}
	if !strings.Contains(stdout.String(), `"type":"message_end"`) {
		t.Fatalf("executor stdout was not streamed to the worker: %q", stdout.String())
	}
	counts := map[string]int{}
	for _, event := range events {
		counts[event.Type]++
	}
	for _, kind := range []string{"agent.message", "agent.thinking", "agent.tool_call", "agent.tool_result"} {
		if counts[kind] == 0 {
			t.Fatalf("missing %s in %#v", kind, events)
		}
	}
}

// shellSingleQuotedLines renders each line as a single-quoted shell argument.
func shellSingleQuotedLines(text string) string {
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	quoted := make([]string, 0, len(lines))
	for _, line := range lines {
		quoted = append(quoted, "'"+strings.ReplaceAll(line, "'", `'\''`)+"'")
	}
	return strings.Join(quoted, " ")
}

func newTestEventLog(t *testing.T) (*eventLog, string) {
	t.Helper()
	path := t.TempDir() + "/events.jsonl"
	log, err := newEventLog(path)
	if err != nil {
		t.Fatal(err)
	}
	return log, path
}
