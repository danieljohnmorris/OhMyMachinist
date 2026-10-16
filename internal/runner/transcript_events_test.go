package runner

import (
	"strings"
	"testing"
)

// ompEditSubagentJSONL is a trimmed capture of real `omp --mode json` output for
// a write, an edit and a subagent task tool call.
const ompEditSubagentJSONL = `{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"write","intent":"Creating hello.txt","arguments":{"path":"hello.txt","content":"one\ntwo\n"}}]}}
{"type":"tool_execution_end","toolCallId":"c1","toolName":"write","result":{"content":[{"type":"text","text":"Successfully wrote 8 bytes to hello.txt"}],"details":{"resolvedPath":"/tmp/hello.txt"}},"isError":false}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"c2","name":"edit","intent":"Replacing one","arguments":{"path":"hello.txt","old_string":"one","new_string":"three"}}]}}
{"type":"tool_execution_end","toolCallId":"c2","toolName":"edit","result":{"content":[{"type":"text","text":"[hello.txt]\n1:three"}],"details":{"diff":"-1|one\n+1|three","firstChangedLine":1,"op":"update","path":"/tmp/hello.txt"}},"isError":false}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"c3","name":"task","intent":"Launching subagent to report the date","arguments":{"context":"ctx","tasks":[{"name":"ReportDate","agent":"task","task":"Run date\nand report"}]}}]}}
{"type":"tool_execution_end","toolCallId":"c3","toolName":"task","result":{"content":[{"type":"text","text":"Spawned agent ` + "`ReportDate`" + ` (job ` + "`ReportDate`" + `).\n\nResults auto-deliver."}]},"isError":false}
{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"c4","name":"task","intent":"Launching subagent to fail","arguments":{"tasks":[{"name":"FailFast","agent":"task","task":"Do the thing"}]}}]}}
{"type":"tool_execution_end","toolCallId":"c4","toolName":"task","result":{"content":[{"type":"text","text":"Subagent failed: boom"}]},"isError":true}`

func TestOmpStreamParserMapsFileEditsAndSubagents(t *testing.T) {
	log, path := newTestEventLog(t)
	parser := newOmpStreamParser(log)
	if _, err := parser.Write([]byte(ompEditSubagentJSONL + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := log.close(); err != nil {
		t.Fatal(err)
	}
	events := readEvents(t, path)
	var kinds []string
	for _, event := range events {
		kinds = append(kinds, event.Type)
	}
	want := []string{"agent.file_edit", "agent.file_edit", "agent.subagent_start", "agent.subagent_end", "agent.subagent_start", "agent.subagent_end"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("kinds = %v", kinds)
	}

	write := events[0]
	if write.Path != "hello.txt" || write.Operation != "write" || write.Additions != 2 || write.Deletions != 0 {
		t.Fatalf("write chip = %#v", write)
	}
	if !strings.Contains(write.Message, "Successfully wrote 8 bytes") || !strings.Contains(write.Args, `"path":"hello.txt"`) {
		t.Fatalf("write details = %#v", write)
	}

	edit := events[1]
	if edit.Path != "hello.txt" || edit.Operation != "edit" || edit.Additions != 1 || edit.Deletions != 1 {
		t.Fatalf("edit chip = %#v", edit)
	}

	start := events[2]
	if start.Subagent != "ReportDate" || start.Message != "Launching subagent to report the date" {
		t.Fatalf("subagent start = %#v", start)
	}
	end := events[3]
	if end.Subagent != "ReportDate" || end.Status != "completed" || !strings.HasPrefix(end.Message, "Spawned agent") {
		t.Fatalf("subagent end = %#v", end)
	}
	failed := events[5]
	if failed.Subagent != "FailFast" || failed.Status != "failed" {
		t.Fatalf("failed subagent end = %#v", failed)
	}
}

func TestFileEditEventUsesPerFileResultsAndPatchArguments(t *testing.T) {
	event := fileEditEvent("apply_patch", []byte(`{"input":"*** Update File: a.go"}`), &ompToolResult{
		Details: &ompToolDetails{
			PerFileResults: []ompFileResult{
				{Path: "a.go", Op: "create", Added: new(4), Removed: new(1)},
				{Path: "b.go", Op: "delete"},
			},
		},
	})
	if event.Path != "a.go" || event.Operation != "create" || event.Additions != 4 || event.Deletions != 1 {
		t.Fatalf("event = %#v", event)
	}
}

func TestFileOperationNormalizesOpNames(t *testing.T) {
	for input, want := range map[string]string{
		"create": "create", "add": "create", "delete": "delete", "removed": "delete",
		"update": "edit", "rename": "edit", "move": "edit", "": "",
	} {
		if got := fileOperation(input); got != want {
			t.Fatalf("fileOperation(%q) = %q want %q", input, got, want)
		}
	}
	secret := "ghp_" + strings.Repeat("a", 36)
	event := fileEditEvent("edit", []byte(`{"path":"x","old_string":"`+secret+`"}`), nil)
	if strings.Contains(event.Args, secret) {
		t.Fatalf("secret leaked in args: %q", event.Args)
	}
}
