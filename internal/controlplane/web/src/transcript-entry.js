import { createElement } from "react";
import { Markdown } from "./markdown.js";

// Renders one transcript entry: markdown for assistant messages, a dimmed
// block for thinking, collapsible details for tool calls and results, and
// monospace output lines that stay red on stderr. Kept as a plain module so it
// can be rendered to markup in tests without a JSX transform.
export function TranscriptEntry({ entry }) {
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
  const className =
    entry.kind === "output"
      ? entry.stream === "stderr"
        ? "whitespace-pre-wrap break-words font-mono text-danger"
        : "whitespace-pre-wrap break-words font-mono"
      : "whitespace-pre-wrap break-words text-muted-foreground";
  return createElement("div", { className }, entry.text);
}