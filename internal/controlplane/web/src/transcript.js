// Turns a run's events.jsonl into readable transcript lines. Output chunks are
// base64 and may split lines, so they are joined per stream before splitting.
export const transcriptPollMillis = 2000;
export const transcriptMaxLines = 2000;

function decodeBase64(data) {
  try {
    const binary = atob(data);
    const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0));
    return new TextDecoder().decode(bytes);
  } catch {
    return "";
  }
}

export function transcriptLines(events, maxLines = transcriptMaxLines) {
  const lines = [];
  const pending = { stdout: "", stderr: "" };
  const flush = (stream, final) => {
    const parts = pending[stream].split("\n");
    pending[stream] = final ? "" : parts.pop();
    for (const text of final ? parts.filter((part, index) => part !== "" || index < parts.length - 1) : parts) {
      lines.push({ stream, text });
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
      lines.push({ stream: "stdout", text: raw });
      continue;
    }
    if (event.type === "process.output") {
      const stream = event.stream === "stderr" ? "stderr" : "stdout";
      pending[stream] += event.encoding === "base64" ? decodeBase64(event.data || "") : event.data || "";
      flush(stream, false);
      continue;
    }
    lines.push({ stream: "event", text: event.message ? `${event.type}: ${event.message}` : event.type || "" });
  }
  flush("stdout", true);
  flush("stderr", true);
  return lines.length > maxLines ? lines.slice(-maxLines) : lines;
}

export function transcriptIsLive(state) {
  return state === "running" || state === "queued";
}
