import test from "node:test";
import assert from "node:assert/strict";
import { transcriptAttempts, transcriptEntries } from "./transcript.js";
import { inPlaceAttemptSummary } from "./runs-board.js";
import { activityRows } from "./run-activity.js";

const b64 = (text) => Buffer.from(text, "utf8").toString("base64");
const line = (event) => JSON.stringify(event);

test("splits the transcript into one section per process launch", () => {
  const events = [
    line({ type: "run.started", message: "command=build repository=lanes/a attempt=1" }),
    line({ type: "process.output", stream: "stdout", encoding: "base64", data: b64("first attempt\n") }),
    line({ type: "run.started", message: "command=build repository=lanes/a attempt=2" }),
    line({ type: "process.output", stream: "stdout", encoding: "base64", data: b64("second attempt\n") }),
  ].join("\n");
  const sections = transcriptAttempts(transcriptEntries(events));
  assert.equal(sections.length, 2);
  assert.deepEqual(sections.map((section) => section.number), [1, 2]);
  assert.match(sections[0].header, /attempt=1/);
  assert.deepEqual(sections[0].entries.map((entry) => entry.text), ["first attempt"]);
  assert.deepEqual(sections[1].entries.map((entry) => entry.text), ["second attempt"]);
});

test("treats a single launch as one section", () => {
  const events = [
    line({ type: "run.started", message: "command=build repository=lanes/a attempt=1" }),
    line({ type: "process.output", stream: "stdout", encoding: "base64", data: b64("only attempt\n") }),
  ].join("\n");
  const sections = transcriptAttempts(transcriptEntries(events));
  assert.equal(sections.length, 1);
  assert.equal(sections[0].number, 1);
});

const run = (attempts) => ({ runs: [{ attempts }] });

test("in-place attempt summary reports the count and the previous end reason", () => {
  const summary = inPlaceAttemptSummary(run([
    { number: 1, worker_name: "lane-a", started_at: "2026-10-01T00:00:00Z", end_reason: "lease expired" },
    { number: 2, worker_name: "lane-a", started_at: "2026-10-01T00:05:00Z" },
  ]));
  assert.deepEqual(summary, { number: 2, total: 2, previousEndReason: "lease expired" });
  assert.equal(inPlaceAttemptSummary(run([{ number: 1 }])), null);
  assert.equal(inPlaceAttemptSummary({ runs: [] }), null);

  // A finished run reports its own final reason, e.g. a relaunch-limit failure.
  const finished = inPlaceAttemptSummary(run([
    { number: 1, end_reason: "lease expired" },
    { number: 2, end_reason: "relaunch limit", ended_at: "2026-10-01T00:10:00Z" },
  ]));
  assert.equal(finished.previousEndReason, "relaunch limit");
});

test("activity logs a relaunch with its time, reason and lane", () => {
  const job = {
    state: "running",
    created_at: "2026-10-01T00:00:00Z",
    state_history: [
      { state: "queued", changed_at: "2026-10-01T00:00:00Z" },
      { state: "running", changed_at: "2026-10-01T00:01:00Z" },
    ],
    runs: [{
      started_at: "2026-10-01T00:01:00Z",
      attempts: [
        { number: 1, worker_name: "lane-a", started_at: "2026-10-01T00:01:00Z", end_reason: "lease expired" },
        { number: 2, worker_name: "lane-b", started_at: "2026-10-01T00:06:00Z" },
      ],
    }],
  };
  const rows = activityRows(job, new Date("2026-10-01T00:10:00Z"));
  const relaunch = rows.find((row) => row.kind === "relaunch");
  assert.ok(relaunch, "relaunch row rendered");
  assert.equal(relaunch.at, "2026-10-01T00:06:00Z");
  assert.equal(relaunch.reason, "lease expired");
  assert.equal(relaunch.worker, "lane-b");
  // Transitions still render and the final state row stays last.
  assert.equal(rows.at(-1).kind, "state");
});