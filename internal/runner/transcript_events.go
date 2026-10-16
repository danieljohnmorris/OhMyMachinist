package runner

import (
	"encoding/json"
	"strings"
)

// MACH-28: map the agent's file-edit and subagent tool calls onto typed
// transcript events. omp names its file tools edit, write and apply_patch (the
// Codex-compatible patch tool); its subagents run through the task tool. Codex
// itself is unaffected: its transcript is raw output on this branch, so only
// the omp JSON stream is mapped here.

// fileEditToolNames are the tool names whose calls become agent.file_edit.
var fileEditToolNames = map[string]bool{
	"edit":        true,
	"write":       true,
	"apply_patch": true,
	"multiedit":   true,
	"multi_edit":  true,
	"str_replace": true,
	"create_file": true,
	"delete_file": true,
}

// subagentToolNames are the tool names whose calls bracket a subagent run.
var subagentToolNames = map[string]bool{
	"task":     true,
	"subagent": true,
}

func isFileEditTool(name string) bool { return fileEditToolNames[strings.ToLower(name)] }
func isSubagentTool(name string) bool { return subagentToolNames[strings.ToLower(name)] }

// ompToolResult is the `result` object of a tool execution: the text parts the
// transcript already shows plus the structured `details` omp adds for edits.
type ompToolResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Details *ompToolDetails `json:"details"`
}

type ompToolDetails struct {
	Diff           string          `json:"diff"`
	Op             string          `json:"op"`
	Path           string          `json:"path"`
	ResolvedPath   string          `json:"resolvedPath"`
	Added          *int            `json:"added"`
	Removed        *int            `json:"removed"`
	PerFileResults []ompFileResult `json:"perFileResults"`
}

type ompFileResult struct {
	Path    string `json:"path"`
	Diff    string `json:"diff"`
	Op      string `json:"op"`
	Added   *int   `json:"added"`
	Removed *int   `json:"removed"`
}

// fileEditArguments is the subset of a file-edit tool's arguments that names
// the changed path and its operation.
type fileEditArguments struct {
	Path     string `json:"path"`
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
	Op       string `json:"op"`
	Edits    []struct {
		Path string `json:"path"`
		Op   string `json:"op"`
	} `json:"edits"`
}

// fileEditEvent builds the agent.file_edit event for one file-edit tool result,
// recovering the path, operation and added/removed line counts from the tool
// arguments and the structured result details when omp reports them.
func fileEditEvent(tool string, args json.RawMessage, result *ompToolResult) Event {
	var parsed fileEditArguments
	if len(args) > 0 {
		_ = json.Unmarshal(args, &parsed)
	}
	path := firstNonEmpty(parsed.Path, parsed.FilePath)
	operation := ""
	additions, deletions := 0, 0
	if result != nil && result.Details != nil {
		details := result.Details
		path = firstNonEmpty(path, details.Path, details.ResolvedPath)
		if len(details.PerFileResults) > 0 {
			path = firstNonEmpty(path, details.PerFileResults[0].Path)
			operation = fileOperation(details.PerFileResults[0].Op)
			for _, file := range details.PerFileResults {
				added, removed := fileResultCounts(file)
				additions += added
				deletions += removed
			}
		} else {
			operation = fileOperation(details.Op)
			additions, deletions = fileResultCounts(ompFileResult{Diff: details.Diff, Added: details.Added, Removed: details.Removed})
		}
	}
	if len(parsed.Edits) > 0 {
		path = firstNonEmpty(path, parsed.Edits[0].Path)
	}
	operation = firstNonEmpty(operation, fileOperation(parsed.Op), defaultFileOperation(tool))
	if tool == "write" && additions == 0 && deletions == 0 {
		additions = countContentLines(parsed.Content)
	}
	return Event{
		Type:      "agent.file_edit",
		Tool:      tool,
		Path:      strings.TrimSpace(path),
		Operation: operation,
		Additions: additions,
		Deletions: deletions,
		Message:   truncateBytes(redactSecrets(toolResultText(result)), ompToolResultBytes),
		Args:      truncateBytes(redactSecrets(string(args)), ompToolArgsBytes),
	}
}

func fileResultCounts(file ompFileResult) (int, int) {
	if file.Added != nil || file.Removed != nil {
		return derefInt(file.Added), derefInt(file.Removed)
	}
	return countDiffLines(file.Diff)
}

func derefInt(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

// fileOperation normalizes the operation names omp and Codex use onto the
// transcript's edit, write, create and delete set.
func fileOperation(op string) string {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case "create", "add", "added", "new":
		return "create"
	case "delete", "remove", "removed", "deleted":
		return "delete"
	case "update", "edit", "modify", "modified", "move", "rename", "moved", "renamed":
		return "edit"
	}
	return ""
}

func defaultFileOperation(tool string) string {
	switch strings.ToLower(tool) {
	case "write":
		return "write"
	case "create_file":
		return "create"
	case "delete_file":
		return "delete"
	}
	return "edit"
}

// countDiffLines counts added and removed lines in omp's `-N|text`/`+N|text`
// diff form and in unified diffs, ignoring the file headers.
func countDiffLines(diff string) (int, int) {
	additions, deletions := 0, 0
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			continue
		case strings.HasPrefix(line, "+"):
			additions++
		case strings.HasPrefix(line, "-"):
			deletions++
		}
	}
	return additions, deletions
}

// countContentLines counts the lines a write tool created.
func countContentLines(content string) int {
	if content == "" {
		return 0
	}
	count := strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		count++
	}
	return count
}

// subagentArguments is the subset of the task tool's arguments that names the
// subagent and describes what it was asked to do.
type subagentArguments struct {
	Context string `json:"context"`
	Tasks   []struct {
		Name  string `json:"name"`
		Agent string `json:"agent"`
		Task  string `json:"task"`
	} `json:"tasks"`
}

// subagentStartEvent builds the agent.subagent_start marker for a subagent tool
// call: the subagent's name and a one-line summary of its task.
func subagentStartEvent(tool string, args json.RawMessage, intent string) Event {
	name, summary := subagentIdentity(args, intent)
	return Event{
		Type:     "agent.subagent_start",
		Tool:     tool,
		Subagent: name,
		Message:  truncateBytes(summary, subagentSummaryBytes),
		Args:     truncateBytes(redactSecrets(string(args)), ompToolArgsBytes),
	}
}

// subagentIdentity returns the subagent's name and a one-line summary of its
// task from the task tool arguments, falling back to the tool-call intent.
func subagentIdentity(args json.RawMessage, intent string) (string, string) {
	var parsed subagentArguments
	if len(args) > 0 {
		_ = json.Unmarshal(args, &parsed)
	}
	name := "subagent"
	summary := strings.TrimSpace(intent)
	if len(parsed.Tasks) > 0 {
		name = firstNonEmpty(parsed.Tasks[0].Name, parsed.Tasks[0].Agent, name)
		if summary == "" {
			summary = firstLine(parsed.Tasks[0].Task)
		}
	}
	return name, summary
}

// subagentEndEvent builds the agent.subagent_end marker: how the subagent
// finished and a short result.
func subagentEndEvent(tool string, name string, result *ompToolResult, failed bool) Event {
	status := "completed"
	if failed {
		status = "failed"
	}
	summary := firstLine(toolResultText(result))
	return Event{
		Type:     "agent.subagent_end",
		Tool:     tool,
		Subagent: name,
		Status:   status,
		Message:  truncateBytes(redactSecrets(summary), subagentResultBytes),
	}
}

// toolResultText joins the text parts of a tool result the way agent.tool_result
// already does.
func toolResultText(result *ompToolResult) string {
	if result == nil {
		return ""
	}
	var text strings.Builder
	for _, part := range result.Content {
		if part.Type == "text" {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
