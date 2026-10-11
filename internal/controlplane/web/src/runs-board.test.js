import assert from "node:assert/strict";
import test from "node:test";
import { boardCardTimestamp, boardColumnForState, boardDateFilters, boardDateWindow, filterJobs, filterJobsByDate, githubIssueReference, groupJobsByBoardColumn, jobDisplayTitle, needsAttention } from "./runs-board.js";

test("board card timestamps prefer run completion for finished jobs and last update otherwise", () => {
  const finishedJob = {
    state: "succeeded",
    updated_at: "2026-10-11T10:00:00Z",
    runs: [
      { completed_at: "2026-10-11T08:00:00Z" },
      { completed_at: "2026-10-11T09:30:00Z" },
      {},
    ],
  };
  assert.equal(boardCardTimestamp(finishedJob), "2026-10-11T09:30:00Z");

  const finishedWithoutRuns = { state: "failed", updated_at: "2026-10-11T10:00:00Z", runs: [] };
  assert.equal(boardCardTimestamp(finishedWithoutRuns), "2026-10-11T10:00:00Z");

  const zeroTimes = { state: "succeeded", runs: [{ completed_at: "0001-01-01T00:00:00Z" }], updated_at: "2026-10-11T10:00:00Z" };
  assert.equal(boardCardTimestamp(zeroTimes), "2026-10-11T10:00:00Z");

  const runningJob = { state: "running", updated_at: "2026-10-11T11:00:00Z", created_at: "2026-10-11T07:00:00Z", runs: [{ completed_at: "2026-10-11T09:00:00Z" }] };
  assert.equal(boardCardTimestamp(runningJob), "2026-10-11T11:00:00Z");

  assert.equal(boardCardTimestamp({ state: "queued", created_at: "2026-10-11T07:00:00Z" }), "2026-10-11T07:00:00Z");
});

test("job states map to the three board columns without hiding attention states", () => {
  assert.equal(boardColumnForState("queued"), "queued");
  assert.equal(boardColumnForState("running"), "running");
  assert.equal(boardColumnForState("succeeded"), "finished");

  for (const state of ["failed", "timed_out", "cancelled", "unexpected_state"]) {
    assert.equal(boardColumnForState(state), "finished");
    assert.equal(needsAttention(state), true);
  }

  const jobs = ["queued", "running", "succeeded", "failed", "timed_out", "cancelled", "unexpected_state"]
    .map((state) => ({ id: state, state }));
  const grouped = groupJobsByBoardColumn(jobs);
  assert.deepEqual(grouped.queued.map(({ id }) => id), ["queued"]);
  assert.deepEqual(grouped.running.map(({ id }) => id), ["running"]);
  assert.deepEqual(grouped.finished.map(({ id }) => id), ["succeeded", "failed", "timed_out", "cancelled", "unexpected_state"]);
});

test("board and table filters use the same filtered job set", () => {
  const jobs = ["queued", "running", "succeeded", "failed", "timed_out", "cancelled", "unexpected_state"]
    .map((state) => ({ id: state, state }));

  assert.deepEqual(filterJobs(jobs, "all").map(({ id }) => id), jobs.map(({ id }) => id));
  assert.deepEqual(filterJobs(jobs, "active").map(({ id }) => id), ["queued", "running"]);
  assert.deepEqual(filterJobs(jobs, "failed").map(({ id }) => id), ["failed", "timed_out"]);
  assert.deepEqual(filterJobs(jobs, "succeeded").map(({ id }) => id), ["succeeded"]);

  const visibleJobs = filterJobs(jobs, "active");
  const grouped = groupJobsByBoardColumn(visibleJobs);
  assert.equal(grouped.queued.length + grouped.running.length + grouped.finished.length, visibleJobs.length);
});

test("board date filters offer exactly today, yesterday, last 7 days and all", () => {
  assert.deepEqual(boardDateFilters, [
    { id: "today", label: "Today" },
    { id: "yesterday", label: "Yesterday" },
    { id: "last7", label: "Last 7 days" },
    { id: "all", label: "All" },
  ]);
});

test("board date windows use local calendar days and include today in the 7-day window", () => {
  const now = new Date(2026, 5, 15, 12, 0, 0);

  const today = boardDateWindow(now, "today");
  assert.equal(today.start.getTime(), new Date(2026, 5, 15, 0, 0, 0).getTime());
  assert.equal(today.end.getTime(), new Date(2026, 5, 16, 0, 0, 0).getTime());

  const yesterday = boardDateWindow(now, "yesterday");
  assert.equal(yesterday.start.getTime(), new Date(2026, 5, 14, 0, 0, 0).getTime());
  assert.equal(yesterday.end.getTime(), new Date(2026, 5, 15, 0, 0, 0).getTime());

  const last7 = boardDateWindow(now, "last7");
  assert.equal(last7.start.getTime(), new Date(2026, 5, 9, 0, 0, 0).getTime());
  assert.equal(last7.end.getTime(), new Date(2026, 5, 16, 0, 0, 0).getTime());

  assert.equal(boardDateWindow(now, "all"), null);
  assert.equal(boardDateWindow(now, ""), null);
});

test("date filters keep jobs whose board timestamp falls inside the window", () => {
  const now = new Date(2026, 5, 15, 12, 0, 0);
  const jobs = [
    { id: "today", state: "running", updated_at: new Date(2026, 5, 15, 9, 0, 0).toISOString(), runs: [] },
    { id: "yesterday", state: "succeeded", updated_at: new Date(2026, 5, 14, 23, 0, 0).toISOString(), runs: [] },
    { id: "boundary", state: "succeeded", updated_at: new Date(2026, 5, 9, 0, 0, 0).toISOString(), runs: [] },
    { id: "too_old", state: "succeeded", updated_at: new Date(2026, 5, 8, 23, 59, 0).toISOString(), runs: [] },
    { id: "future", state: "queued", updated_at: new Date(2026, 5, 16, 1, 0, 0).toISOString(), runs: [] },
    { id: "no_timestamp", state: "queued", runs: [] },
  ];
  const allIds = ["today", "yesterday", "boundary", "too_old", "future", "no_timestamp"];

  assert.deepEqual(filterJobsByDate(jobs, "all", now).map(({ id }) => id), allIds);
  assert.deepEqual(filterJobsByDate(jobs, "today", now).map(({ id }) => id), ["today"]);
  assert.deepEqual(filterJobsByDate(jobs, "yesterday", now).map(({ id }) => id), ["yesterday"]);
  assert.deepEqual(filterJobsByDate(jobs, "last7", now).map(({ id }) => id), ["today", "yesterday", "boundary"]);
  assert.deepEqual(filterJobsByDate(jobs, "unknown", now).map(({ id }) => id), allIds);
});

test("GitHub issue titles are preferred over prompts and hashes", () => {
  const job = { id: "job_12345678", prompt: "Complete https://github.com/o/r/issues/7", github_issue_title: "Make cards readable", trigger_subject: "https://github.com/o/r/issues/7" };
  assert.equal(jobDisplayTitle(job), "Make cards readable");
  assert.equal(githubIssueReference(job), "#7");
  assert.equal(jobDisplayTitle({ id: "job_12345678", prompt: "Run an audit" }), "Run an audit");
});

test("stored job titles are preferred over task titles and coder prompts", () => {
  const job = {
    id: "job_12345678",
    title: "OMM-1: Local build step timings and ETAs",
    task: { title: "Wrong task title", spec: "You are the CODER in a spec-driven software factory." },
    prompt: "You are the CODER in a spec-driven software factory.",
  };
  assert.equal(jobDisplayTitle(job), "OMM-1: Local build step timings and ETAs");
});
