import test from "node:test";
import assert from "node:assert/strict";
import { transcriptEntries, transcriptIsLive } from "./transcript.js";

const b64 = (text) => Buffer.from(text, "utf8").toString("base64");
const line = (event) => JSON.stringify(event);

test("joins base64 output chunks into whole lines per stream", () => {
  const events = [
    line({ type: "run.started", message: "command=build" }),
    line({ type: "process.output", stream: "stdout", encoding: "base64", data: b64("hel") }),
    line({ type: "process.output", stream: "stdout", encoding: "base64", data: b64("lo\nwor") }),
    line({ type: "process.output", stream: "stderr", encoding: "base64", data: b64("warn\n") }),
    line({ type: "run.completed", message: "state=succeeded" }),
  ].join("\n");
  assert.deepEqual(transcriptEntries(events), [
    { kind: "event", text: "run.started: command=build" },
    { kind: "output", stream: "stdout", text: "hello" },
    { kind: "output", stream: "stderr", text: "warn" },
    { kind: "event", text: "run.completed: state=succeeded" },
    { kind: "output", stream: "stdout", text: "wor" },
  ]);
});

test("turns structured agent events into readable transcript entries", () => {
  const events = [
    line({ type: "agent.thinking", message: "considering\n" }),
    line({ type: "agent.tool_call", tool: "bash", tool_args: '{"command":"echo hi"}' }),
    line({ type: "agent.tool_result", tool: "bash", message: "hi\n", bytes: 3 }),
    line({ type: "agent.tool_result", tool: "bash", message: "boom", bytes: 4, failed: true }),
    line({ type: "agent.message", message: "**Done**." }),
  ].join("\n");
  assert.deepEqual(transcriptEntries(events), [
    { kind: "thinking", text: "considering\n" },
    { kind: "tool-call", tool: "bash", args: '{\n  "command": "echo hi"\n}', text: "" },
    { kind: "tool-result", tool: "bash", text: "hi\n", bytes: 3, failed: false },
    { kind: "tool-result", tool: "bash", text: "boom", bytes: 4, failed: true },
    { kind: "message", text: "**Done**." },
  ]);
});

test("shows malformed lines as raw output and keeps only the newest entries", () => {
  const events = ["not json", ...Array.from({ length: 5 }, (_, i) => line({ type: "tick", message: String(i) }))].join("\n");
  assert.deepEqual(transcriptEntries(events, 6).map((entry) => entry.text), ["not json", "tick: 0", "tick: 1", "tick: 2", "tick: 3", "tick: 4"]);
  assert.deepEqual(transcriptEntries(""), []);
});

test("polls only while the run is active", () => {
  assert.equal(transcriptIsLive("running"), true);
  assert.equal(transcriptIsLive("succeeded"), false);
});
test("maps file edits and subagents to chips and markers", () => {
  const events = [
    line({ type: "agent.file_edit", tool: "edit", path: "docs/api.md", operation: "edit", additions: 3, deletions: 1, tool_args: '{"path":"docs/api.md"}', message: "[docs/api.md]\n1:x" }),
    line({ type: "agent.subagent_start", tool: "task", subagent: "Reviewer", message: "Review the diff" }),
    line({ type: "agent.message", message: "inside" }),
    line({ type: "agent.subagent_end", tool: "task", subagent: "Reviewer", status: "completed", message: "done" }),
  ].join("\n");
  assert.deepEqual(transcriptEntries(events), [
    {
      kind: "file-edit",
      path: "docs/api.md",
      operation: "edit",
      additions: 3,
      deletions: 1,
      tool: "edit",
      args: '{\n  "path": "docs/api.md"\n}',
      text: "[docs/api.md]\n1:x",
    },
    { kind: "subagent-start", subagent: "Reviewer", text: "Review the diff" },
    { kind: "message", text: "inside", depth: 1 },
    { kind: "subagent-end", subagent: "Reviewer", status: "completed", text: "done" },
  ]);
});

test("defaults missing file edit counts to zero", () => {
  const events = line({ type: "agent.file_edit", tool: "write", path: "a.txt" });
  assert.deepEqual(transcriptEntries(events), [
    { kind: "file-edit", path: "a.txt", operation: "", additions: 0, deletions: 0, tool: "write", args: "", text: "" },
  ]);
});
