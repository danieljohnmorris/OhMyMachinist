package controlplane

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/owainlewis/machinist/internal/runner"
)

// Requeued jobs carry their prior attempt's transcript in the coder prompt so the
// agent can diagnose the failure without leaving the factory. Budgets keep a huge
// prior run from crowding out the task itself.
const (
	maxPriorErrorBytes      = 2 << 10
	maxPriorResultBytes     = 2 << 10
	maxPriorRunTranscript   = 16 << 10
	maxPriorTranscriptBytes = 32 << 10
	// protocol.Task.Validate rejects specs over 256KiB; injected context must
	// still fit under that limit when appended to an existing spec.
	maxTaskSpecBytes = 256 << 10
)

const priorAttemptHeader = `## Prior attempt

This job is a requeue of a failed attempt on the same source. Diagnose the prior
failure from its transcript and error below, then fix the underlying problem before
completing the task.`

// priorAttemptContext builds a prompt block describing a prior job's runs: one
// section per run with its outcome, error, result summary, and decoded transcript.
// The block is bounded so a noisy prior run cannot dominate the new prompt.
func priorAttemptContext(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, previousJobID string) (string, error) {
	rows, err := q.QueryContext(ctx, `SELECT COALESCE(command,''),state,COALESCE(error,''),COALESCE(result,''),COALESCE(events,'') FROM runs WHERE job_id=? ORDER BY rowid`, previousJobID)
	if err != nil {
		return "", fmt.Errorf("read prior attempt runs: %w", err)
	}
	defer rows.Close()

	var sections []string
	remaining := maxPriorTranscriptBytes
	for rows.Next() {
		var command, state, runError, result, events string
		if err := rows.Scan(&command, &state, &runError, &result, &events); err != nil {
			return "", fmt.Errorf("scan prior attempt run: %w", err)
		}
		var summary strings.Builder
		summary.WriteString("### Prior run")
		if command != "" {
			summary.WriteString(" `" + command + "`")
		}
		summary.WriteString(" — " + state)
		if state == "timed_out" {
			summary.WriteString(" (hit its timeout)")
		}
		if runError != "" {
			summary.WriteString("\nError: " + bound(runError, maxPriorErrorBytes))
		}
		if trimmed := strings.TrimSpace(result); trimmed != "" {
			summary.WriteString("\nResult: " + bound(trimmed, maxPriorResultBytes))
		}
		if transcript := strings.Join(decodeEventTranscript(events), "\n"); transcript != "" {
			if len(transcript) > remaining {
				transcript = bound(transcript, remaining)
			} else {
				remaining -= len(transcript)
			}
			summary.WriteString("\n\nTranscript:\n```\n" + transcript + "\n```")
		}
		sections = append(sections, summary.String())
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read prior attempt runs: %w", err)
	}
	if len(sections) == 0 {
		sections = []string{"No run transcript was recorded for the prior attempt."}
	}
	return priorAttemptHeader + "\n\n" + strings.Join(sections, "\n\n"), nil
}

// appendPriorContext appends the prior-attempt block to the job prompt and the task
// spec. The prompt always carries the full block; the spec keeps it only while the
// combined spec stays inside protocol.Task.Validate's size limit.
func appendPriorContext(prompt, spec, block string) (string, string) {
	if block == "" {
		return prompt, spec
	}
	prompt += "\n\n" + block
	if spec != "" && len(spec)+len(block)+2 <= maxTaskSpecBytes {
		spec += "\n\n" + block
	}
	return prompt, spec
}

// bound trims text to at most limit bytes, keeping the tail: the end of a failed
// run holds the error; its start repeats the prompt.
func bound(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	return "[…truncated…]\n" + text[len(text)-limit:]
}

// decodeEventTranscript renders a run's stored event log as readable lines,
// mirroring the board's transcript view: output chunks are base64 and may split
// lines, so they are joined per stream before splitting.
func decodeEventTranscript(events string) []string {
	var lines []string
	pending := map[string]string{}
	flush := func(stream string, final bool) {
		parts := strings.Split(pending[stream], "\n")
		if final {
			pending[stream] = ""
			if n := len(parts); n > 0 && parts[n-1] == "" {
				parts = parts[:n-1]
			}
		} else {
			pending[stream] = parts[len(parts)-1]
			parts = parts[:len(parts)-1]
		}
		for _, text := range parts {
			if stream == "stderr" {
				lines = append(lines, "stderr: "+text)
				continue
			}
			lines = append(lines, text)
		}
	}
	for _, raw := range strings.Split(events, "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var event runner.Event
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			// Raw (non-JSONL) log content is real output; keep it.
			lines = append(lines, raw)
			continue
		}
		if event.Type == "process.output" {
			stream := "stdout"
			if event.Stream == "stderr" {
				stream = "stderr"
			}
			data := event.Data
			if event.Encoding == "base64" {
				decoded, err := base64.StdEncoding.DecodeString(event.Data)
				if err != nil {
					data = ""
				} else {
					data = string(decoded)
				}
			}
			pending[stream] += data
			flush(stream, false)
			continue
		}
		if event.Message != "" {
			lines = append(lines, event.Type+": "+event.Message)
			continue
		}
		lines = append(lines, event.Type)
	}
	flush("stdout", true)
	flush("stderr", true)
	return lines
}
