import test from "node:test";
import assert from "node:assert/strict";
import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { TranscriptEntry } from "./transcript-entry.js";

const render = (entry) => renderToStaticMarkup(createElement(TranscriptEntry, { entry }));

test("assistant messages render as markdown", () => {
  const html = render({ kind: "message", text: "**Done** with `code`." });
  assert.match(html, /markdown-body/);
  assert.match(html, /<strong>Done<\/strong>/);
  assert.match(html, /<code>code<\/code>/);
});

test("thinking renders dimmed and labelled", () => {
  const html = render({ kind: "thinking", text: "considering the request" });
  assert.match(html, /Thinking/);
  assert.match(html, /text-muted-foreground/);
  assert.match(html, /opacity-70/);
});

test("tool calls and results collapse and show the byte count", () => {
  const call = render({ kind: "tool-call", tool: "bash", args: '{"command":"ls"}', text: "" });
  assert.match(call, /<details/);
  assert.match(call, /Tool call · bash/);
  assert.match(call, /&quot;command&quot;/);

  const result = render({ kind: "tool-result", tool: "bash", text: "hi", bytes: 38, failed: false });
  assert.match(result, /<details/);
  assert.match(result, /Tool result · bash/);
  assert.match(result, /38 bytes/);

  const failed = render({ kind: "tool-result", tool: "bash", text: "boom", bytes: 4, failed: true });
  assert.match(failed, /Tool error · bash/);
  assert.match(failed, /text-danger/);
});

test("stderr output keeps the danger styling", () => {
  const html = render({ kind: "output", stream: "stderr", text: "deliver-pr: failed" });
  assert.match(html, /text-danger/);
  assert.match(html, /deliver-pr: failed/);
});
test("file edit chips show the path and change counts", () => {
  const html = render({ kind: "file-edit", path: "docs/api.md", operation: "edit", additions: 3, deletions: 1, tool: "edit", args: '{"path":"docs/api.md"}', text: "[docs/api.md]" });
  assert.match(html, /<details/);
  assert.match(html, /docs\/api.md/);
  assert.match(html, /\+3/);
  assert.match(html, /-1/);
  assert.match(html, /edit/);
});

test("subagent markers name the subagent and its task", () => {
  const start = render({ kind: "subagent-start", subagent: "Reviewer", text: "Review the diff" });
  assert.match(start, /Subagent · Reviewer/);
  assert.match(start, /Review the diff/);

  const end = render({ kind: "subagent-end", subagent: "Reviewer", status: "completed", text: "done" });
  assert.match(end, /Subagent finished · Reviewer/);

  const failed = render({ kind: "subagent-end", subagent: "Reviewer", status: "failed", text: "boom" });
  assert.match(failed, /Subagent failed · Reviewer/);
  assert.match(failed, /text-danger/);
});
