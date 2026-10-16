package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// omp is the coding agent the factory executors wrap. With `--mode json` it
// writes its session as JSONL, one event object per line, on stdout. This
// parser turns those lines into the typed transcript events the run page
// renders (agent.message, agent.thinking, agent.tool_call, agent.tool_result)
// and recovers the final assistant message and token usage the text mode used
// to carry.
const (
	// ompToolResultBytes caps one tool result in the transcript; the event
	// still records the full byte count.
	ompToolResultBytes = 2 << 10
	ompThinkingBytes   = 4 << 10
	ompToolArgsBytes   = 8 << 10
	ompMaxEventBytes   = 1 << 20
)

// IsOmpExecutor reports whether the resolved executor runs omp, directly or
// through the factory wrappers. Callers use it to choose the native skill
// injection over the prompt fallback.
func IsOmpExecutor(executor string, command []string) bool {
	return ompExecutor(executor, command)
}

// ompExecutor reports whether the executor runs omp, either directly or through
// the factory wrappers (omp-glm, omp-glm-pr) named after it.
func ompExecutor(executor string, command []string) bool {
	if executorNamed(executor, "omp") {
		return true
	}
	programIndex := wrappedProgramIndex(command)
	return programIndex >= 0 && executableName(command[programIndex]) == "omp"
}

// structuredOmpCommand forces JSON output on an omp command line so its agent
// events can be streamed. Wrappers that shell out to omp (and read their own
// first argument as an effort) are left alone; they opt in themselves.
func structuredOmpCommand(executor string, command []string) []string {
	programIndex := wrappedProgramIndex(command)
	if programIndex < 0 || executableName(command[programIndex]) != "omp" {
		return command
	}
	for _, argument := range command[programIndex+1:] {
		if argument == "--mode" || strings.HasPrefix(argument, "--mode=") {
			return command
		}
	}
	structured := make([]string, 0, len(command)+2)
	structured = append(structured, command[:programIndex+1]...)
	structured = append(structured, "--mode", "json")
	return append(structured, command[programIndex+1:]...)
}

type ompStreamParser struct {
	log          *eventLog
	buffer       []byte
	discarding   bool
	finalMessage string
	usage        *int64
}

func newOmpStreamParser(log *eventLog) *ompStreamParser {
	return &ompStreamParser{log: log}
}

func (parser *ompStreamParser) Write(data []byte) (int, error) {
	remaining := data
	for len(remaining) > 0 {
		newline := bytes.IndexByte(remaining, '\n')
		if newline < 0 {
			parser.appendFragment(remaining)
			break
		}
		parser.appendFragment(remaining[:newline])
		parser.parseLine(parser.buffer)
		parser.buffer = parser.buffer[:0]
		parser.discarding = false
		remaining = remaining[newline+1:]
	}
	return len(data), nil
}

// flush parses the trailing line when the process ended without a newline.
func (parser *ompStreamParser) flush() {
	if !parser.discarding && len(parser.buffer) > 0 {
		parser.parseLine(parser.buffer)
	}
	parser.buffer = parser.buffer[:0]
	parser.discarding = false
}

// lastMessage returns the text of the last assistant message, which is the run
// summary text mode used to print.
func (parser *ompStreamParser) lastMessage() string {
	parser.flush()
	return parser.finalMessage
}

func (parser *ompStreamParser) tokenUsage() *int64 {
	parser.flush()
	return parser.usage
}

func (parser *ompStreamParser) appendFragment(fragment []byte) {
	if parser.discarding {
		return
	}
	if len(fragment) > ompMaxEventBytes-len(parser.buffer) {
		parser.buffer = parser.buffer[:0]
		parser.discarding = true
		return
	}
	parser.buffer = append(parser.buffer, fragment...)
}

func (parser *ompStreamParser) parseLine(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var event ompEvent
	if err := json.Unmarshal(line, &event); err != nil {
		return
	}
	switch event.Type {
	case "message_end":
		parser.parseMessage(event)
	case "tool_execution_end":
		parser.parseToolResult(event)
	}
}

type ompEvent struct {
	Type    string `json:"type"`
	Message *struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"content"`
		Usage struct {
			TotalTokens *int64 `json:"totalTokens"`
		} `json:"usage"`
	} `json:"message"`
	ToolName string `json:"toolName"`
	Result   *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	IsError bool `json:"isError"`
}

func (parser *ompStreamParser) parseMessage(event ompEvent) {
	if event.Message == nil || event.Message.Role != "assistant" {
		return
	}
	if total := event.Message.Usage.TotalTokens; total != nil && *total >= 0 {
		usage := *total
		parser.usage = &usage
	}
	for _, item := range event.Message.Content {
		switch item.Type {
		case "text":
			if strings.TrimSpace(item.Text) == "" {
				continue
			}
			parser.finalMessage = item.Text
			parser.emit(Event{Type: "agent.message", Message: item.Text})
		case "thinking":
			if strings.TrimSpace(item.Thinking) == "" {
				continue
			}
			parser.emit(Event{Type: "agent.thinking", Message: truncateBytes(redactSecrets(item.Thinking), ompThinkingBytes)})
		case "toolCall":
			if item.Name == "" {
				continue
			}
			parser.emit(Event{
				Type:    "agent.tool_call",
				Message: item.Name,
				Tool:    item.Name,
				Args:    truncateBytes(redactSecrets(string(item.Arguments)), ompToolArgsBytes),
			})
		}
	}
}

func (parser *ompStreamParser) parseToolResult(event ompEvent) {
	if event.ToolName == "" {
		return
	}
	var text strings.Builder
	if event.Result != nil {
		for _, part := range event.Result.Content {
			if part.Type == "text" {
				text.WriteString(part.Text)
			}
		}
	}
	redacted := redactSecrets(text.String())
	parser.emit(Event{
		Type:    "agent.tool_result",
		Message: truncateBytes(redacted, ompToolResultBytes),
		Tool:    event.ToolName,
		Bytes:   len(redacted),
		Failed:  event.IsError,
	})
}

func (parser *ompStreamParser) emit(event Event) {
	if parser.log == nil {
		return
	}
	// Best effort, like live event streaming: a full log drops new transcript
	// events rather than failing the run.
	_ = parser.log.appendTyped(event)
}

// truncateBytes cuts text to at most limit bytes on a rune boundary and marks
// the cut so the transcript never shows a silently halved payload.
func truncateBytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return fmt.Sprintf("%s\n\n[truncated: %d of %d bytes]", text[:cut], cut, len(text))
}
