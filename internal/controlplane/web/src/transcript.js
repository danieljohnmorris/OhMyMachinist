// Turns a run's events.jsonl into readable transcript entries. Output chunks
// are base64 and may split lines, so they are joined per stream before
// splitting. Structured agent events (omp and other agents in JSON mode) become
// message, thinking, tool-call and tool-result entries the transcript renders
// readably instead of raw JSON.
export const transcriptPollMillis = 2000;
export const transcriptMaxEntries = 2000;

function decodeBase64(data) {
  try {
    const binary = atob(data);
    const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0));
    return new TextDecoder().decode(bytes);
  } catch {
    return "";
  }
}

function toolArguments(event) {
  const raw = event.tool_args || event.args || "";
  if (!raw) return "";
  try {
    return JSON.stringify(JSON.parse(raw), null, 2);
  } catch {
    return raw;
  }
}

// count returns a non-negative integer for a numeric field, so a missing or
// malformed count renders as zero rather than NaN.
function count(value) {
  return Number.isFinite(value) && value > 0 ? value : 0;
}

export function transcriptEntries(events, maxEntries = transcriptMaxEntries) {
  const entries = [];
  const pending = { stdout: "", stderr: "" };
  // Subagent markers group the entries nested between them, when the stream
  // carries the subagent's own events.
  let subagentDepth = 0;
  const push = (entry) => {
    entries.push(subagentDepth > 0 ? { ...entry, depth: subagentDepth } : entry);
  };
  const flush = (stream, final) => {
    const parts = pending[stream].split("\n");
    pending[stream] = final ? "" : parts.pop();
    for (const text of final ? parts.filter((part, index) => part !== "" || index < parts.length - 1) : parts) {
      push({ kind: "output", stream, text });
    }
  };
  for (const raw of String(events || "").split("\n")) {
    if (!raw.trim()) continue;
    let event;
    try {
      event = JSON.parse(raw);
    } catch {
      // Raw (non-JSONL) log content is real output; show it rather than
      // silently hiding it behind "Waiting for output…".
      push({ kind: "output", stream: "stdout", text: raw });
      continue;
    }
    switch (event.type) {
      case "process.output": {
        const stream = event.stream === "stderr" ? "stderr" : "stdout";
        pending[stream] += event.encoding === "base64" ? decodeBase64(event.data || "") : event.data || "";
        flush(stream, false);
        continue;
      }
      case "agent.message":
        if (event.message) push({ kind: "message", text: event.message });
        continue;
      case "agent.thinking":
        if (event.message) push({ kind: "thinking", text: event.message });
        continue;
      case "agent.tool_call":
        push({ kind: "tool-call", tool: event.tool || "tool", args: toolArguments(event), text: event.message || "" });
        continue;
      case "agent.tool_result":
        push({
          kind: "tool-result",
          tool: event.tool || "tool",
          text: event.message || "",
          bytes: Number.isFinite(event.bytes) ? event.bytes : 0,
          failed: Boolean(event.failed),
        });
        continue;
      case "agent.file_edit":
        push({
          kind: "file-edit",
          path: event.path || "",
          operation: event.operation || "",
          additions: count(event.additions),
          deletions: count(event.deletions),
          tool: event.tool || "edit",
          args: toolArguments(event),
          text: event.message || "",
        });
        continue;
      case "agent.subagent_start":
        push({ kind: "subagent-start", subagent: event.subagent || event.tool || "subagent", text: event.message || "" });
        subagentDepth++;
        continue;
      case "agent.subagent_end":
        subagentDepth = Math.max(0, subagentDepth - 1);
        push({
          kind: "subagent-end",
          subagent: event.subagent || event.tool || "subagent",
          status: event.status || "",
          text: event.message || "",
        });
        continue;
      default:
        push({ kind: "event", text: event.message ? `${event.type}: ${event.message}` : event.type || "" });
    }
  }
  flush("stdout", true);
  flush("stderr", true);
  return entries.length > maxEntries ? entries.slice(-maxEntries) : entries;
}

export function transcriptIsLive(state) {
  return state === "running" || state === "queued";
}