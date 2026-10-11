import assert from "node:assert/strict";
import test from "node:test";
import { activityRows } from "./run-activity.js";
import { relativeTime } from "./relative-time.js";

const now = new Date("2026-10-11T12:00:00Z");

const succeededJob = {
  state: "succeeded",
  created_at: "2026-10-11T09:00:00Z",
  state_history: [
    { state: "queued", changed_at: "2026-10-11T09:00:00Z" },
    { state: "running", changed_at: "2026-10-11T09:05:00Z" },
    { state: "succeeded", changed_at: "2026-10-11T10:59:00Z" },
  ],
  runs: [{ completed_at: "2026-10-11T10:59:00Z" }],
};

test("activity orders transitions oldest first with the current state last", () => {
  const rows = activityRows(succeededJob, now);
  assert.deepEqual(
    rows.map((row) => row.kind === "transition" ? `${row.from} → ${row.to}` : row.state),
    ["queued → running", "running → succeeded", "succeeded"],
  );
});

test("activity reports the duration spent in each exited state", () => {
  const rows = activityRows(succeededJob, now);
  assert.equal(rows[0].duration, "5m 0s");
  assert.equal(rows[1].duration, "1h 54m 0s");
  assert.equal(rows[2].duration, undefined);
});

test("activity formats relative times and carries them per row", () => {
  const rows = activityRows(succeededJob, now);
  assert.deepEqual(rows.map((row) => row.ago), ["2h ago", "1h ago", "1h ago"]);
  assert.equal(relativeTime("2026-10-11T11:59:55Z", now), "just now");
  assert.equal(relativeTime("2026-10-11T11:59:15Z", now), "45s ago");
  assert.equal(relativeTime("2026-10-11T11:58:00Z", now), "2m ago");
  assert.equal(relativeTime("2026-10-11T09:00:00Z", now), "3h ago");
  assert.equal(relativeTime("2026-10-09T12:00:00Z", now), "2d ago");
});

test("activity shows a live duration for the current state of in-progress runs", () => {
  const rows = activityRows(
    {
      state: "running",
      state_history: [
        { state: "queued", changed_at: "2026-10-11T11:58:00Z" },
        { state: "running", changed_at: "2026-10-11T11:59:00Z" },
      ],
    },
    now,
  );
  assert.deepEqual(rows.map((row) => row.state ?? row.to), ["running", "running"]);
  assert.equal(rows[0].duration, "1m 0s");
  assert.equal(rows[1].live, true);
  assert.equal(rows[1].duration, "1m 0s");
});

test("activity records timed-out runs as the run's own final status", () => {
  const rows = activityRows(
    {
      state: "failed",
      state_history: [
        { state: "queued", changed_at: "2026-10-11T10:00:00Z" },
        { state: "running", changed_at: "2026-10-11T10:01:00Z" },
        { state: "failed", changed_at: "2026-10-11T10:03:00Z" },
      ],
      runs: [{ state: "timed_out", completed_at: "2026-10-11T10:03:00Z" }],
    },
    now,
  );
  const final = rows.at(-1);
  assert.equal(final.kind, "state");
  assert.equal(final.live, false);
  assert.equal(final.duration, undefined);
});

test("activity falls back to stored timestamps for runs without history", () => {
  const rows = activityRows(
    {
      state: "succeeded",
      created_at: "2026-10-11T10:00:00Z",
      runs: [{ state: "timed_out", started_at: "2026-10-11T10:01:00Z", completed_at: "2026-10-11T10:02:00Z" }],
    },
    now,
  );
  assert.deepEqual(
    rows.map((row) => row.kind === "transition" ? `${row.from} → ${row.to}` : row.state),
    ["queued → running", "running → timed_out", "timed_out"],
  );
  assert.equal(rows[0].duration, "1m 0s");
});

test("activity tolerates a queued job with no timestamps and duplicate entries", () => {
  assert.deepEqual(activityRows(null, now), []);
  assert.deepEqual(activityRows({ state: "queued" }, now), []);
  const rows = activityRows(
    {
      state: "blocked",
      state_history: [
        { state: "queued", changed_at: "2026-10-11T11:00:00Z" },
        { state: "blocked", changed_at: "2026-10-11T11:00:05Z" },
        { state: "blocked", changed_at: "2026-10-11T11:00:06Z" },
      ],
    },
    now,
  );
  assert.deepEqual(
    rows.map((row) => row.kind === "transition" ? `${row.from} → ${row.to}` : row.state),
    ["queued → blocked", "blocked"],
  );
  assert.equal(rows[1].live, true);
});
