import { createElement } from "react";
import { Markdown } from "./markdown.js";

// Renders one transcript entry: markdown for assistant messages, a dimmed
// block for thinking, collapsible details for tool calls and results,
// file-edit chips and subagent markers, and monospace output lines that stay
// red on stderr. Kept as a plain module so it can be rendered to markup in
// tests without a JSX transform.
export function TranscriptEntry({ entry }) {
  const body = transcriptBody(entry);
  // Entries nested between a subagent's start and end markers are indented.
  if (entry.depth > 0) {
    return createElement("div", { style: { marginLeft: `${entry.depth * 1}rem` } }, body);
  }
  return body;
}

function transcriptBody(entry) {
  if (entry.kind === "message") {
    return createElement(Markdown, { source: entry.text, className: "text-xs" });
  }
  if (entry.kind === "thinking") {
    return createElement(
      "div",
      { className: "text-muted-foreground" },
      createElement("span", { className: "font-medium opacity-70" }, "Thinking"),
      createElement("div", { className: "whitespace-pre-wrap break-words opacity-70" }, entry.text),
    );
  }
  if (entry.kind === "tool-call") {
    return createElement(
      "details",
      { className: "rounded border border-border/60" },
      createElement("summary", { className: "cursor-pointer select-none px-2 py-1 font-medium" }, `Tool call · ${entry.tool}`),
      createElement(
        "pre",
        { className: "overflow-x-auto whitespace-pre-wrap break-words border-t border-border/60 px-2 py-1 font-mono" },
        entry.args || entry.text || "(no arguments)",
      ),
    );
  }
  if (entry.kind === "tool-result") {
    const size = entry.bytes ? ` · ${entry.bytes} bytes` : "";
    return createElement(
      "details",
      { className: "rounded border border-border/60" },
      createElement(
        "summary",
        { className: `cursor-pointer select-none px-2 py-1 font-medium ${entry.failed ? "text-danger" : ""}` },
        `${entry.failed ? "Tool error" : "Tool result"} · ${entry.tool}${size}`,
      ),
      createElement(
        "pre",
        { className: "overflow-x-auto whitespace-pre-wrap break-words border-t border-border/60 px-2 py-1 font-mono" },
        entry.text || "(no output)",
      ),
    );
  }
  if (entry.kind === "file-edit") {
    const counts = [];
    if (entry.additions) counts.push(createElement("span", { key: "add", className: "text-emerald-600" }, `+${entry.additions}`));
    if (entry.deletions) counts.push(createElement("span", { key: "del", className: "text-danger" }, `-${entry.deletions}`));
    return createElement(
      "details",
      { className: "rounded border border-border/60" },
      createElement(
        "summary",
        { className: "flex cursor-pointer select-none items-center gap-2 px-2 py-1 font-medium" },
        createElement("span", { className: "font-mono" }, entry.path || "(unknown path)"),
        entry.operation && createElement("span", { className: "text-xs text-muted-foreground" }, entry.operation),
        counts.length ? createElement("span", { className: "ml-auto flex gap-2 font-mono text-xs" }, counts) : null,
      ),
      createElement(
        "div",
        { className: "space-y-1 border-t border-border/60 px-2 py-1" },
        entry.args && createElement("pre", { className: "overflow-x-auto whitespace-pre-wrap break-words font-mono text-muted-foreground" }, entry.args),
        entry.text && createElement("pre", { className: "overflow-x-auto whitespace-pre-wrap break-words font-mono" }, entry.text),
        !entry.args && !entry.text && createElement("pre", { className: "font-mono text-muted-foreground" }, "(no details)"),
      ),
    );
  }
  if (entry.kind === "subagent-start") {
    return createElement(
      "div",
      { className: "rounded border border-dashed border-border/60 px-2 py-1 font-medium" },
      `Subagent · ${entry.subagent}`,
      entry.text ? createElement("div", { className: "text-xs font-normal text-muted-foreground" }, entry.text) : null,
    );
  }
  if (entry.kind === "subagent-end") {
    const label = entry.status === "failed" ? "Subagent failed" : "Subagent finished";
    return createElement(
      "div",
      { className: `rounded border border-dashed border-border/60 px-2 py-1 font-medium ${entry.status === "failed" ? "text-danger" : ""}` },
      `${label} · ${entry.subagent}`,
      entry.text ? createElement("div", { className: "text-xs font-normal text-muted-foreground" }, entry.text) : null,
    );
  }
  const className =
    entry.kind === "output"
      ? entry.stream === "stderr"
        ? "whitespace-pre-wrap break-words font-mono text-danger"
        : "whitespace-pre-wrap break-words font-mono"
      : "whitespace-pre-wrap break-words text-muted-foreground";
  return createElement("div", { className }, entry.text);
}