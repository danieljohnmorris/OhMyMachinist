import assert from "node:assert/strict";
import test from "node:test";
import { boardCardTimestamp, boardColumnForState, collapseIssueAttempts, filterJobs, githubIssueReference, groupJobsByBoardColumn, jobDisplayTitle, needsAttention, priorAttempts, supersededJob } from "./runs-board.js";

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

const issue = "https://github.com/example/repo/issues/7";

function issueJob(overrides) {
  return {
    repository: "machinist",
    trigger_subject: issue,
    created_at: "2026-10-11T10:00:00Z",
    ...overrides,
  };
}

test("requeued attempts collapse to one board card showing the latest attempt", () => {
  const jobs = [
    issueJob({ id: "attempt_1", state: "failed", attempt: 1, created_at: "2026-10-11T09:00:00Z", runs: [{ id: "run_1", state: "failed", exit_code: 1, error: "tests failed" }] }),
    issueJob({ id: "attempt_2", state: "running", attempt: 2, previous_job_id: "attempt_1", created_at: "2026-10-11T10:00:00Z", runs: [{ id: "run_2", state: "running" }] }),
  ];

  const collapsed = collapseIssueAttempts(jobs);
  assert.deepEqual(collapsed.map(({ id }) => id), ["attempt_2"], "the superseded failed card is gone");
  assert.equal(collapsed[0].attempt_count, 2);
  assert.equal(collapsed[0].prior_failed_count, 1);

  const grouped = groupJobsByBoardColumn(collapsed);
  assert.equal(grouped.finished.length, 0, "no duplicate Failed card for the same issue");
  assert.deepEqual(grouped.running.map(({ id }) => id), ["attempt_2"]);

  // The needs-attention view of the collapsed set has no superseded fail in it.
  assert.equal(filterJobs(collapsed, "failed").length, 0);
  assert.equal(grouped.attention.length, 0);
  assert.equal(needsAttention("failed"), true, "a latest-attempt fail still needs attention");
});

test("latest attempt selection prefers the highest attempt then the newest job", () => {
  const jobs = [
    issueJob({ id: "attempt_1", state: "failed", attempt: 1, created_at: "2026-10-11T08:00:00Z" }),
    issueJob({ id: "attempt_3", state: "succeeded", attempt: 3, previous_job_id: "attempt_2", created_at: "2026-10-11T11:00:00Z" }),
    issueJob({ id: "attempt_2", state: "failed", attempt: 2, previous_job_id: "attempt_1", superseded_by: "attempt_3", created_at: "2026-10-11T10:00:00Z" }),
  ];

  const collapsed = collapseIssueAttempts(jobs);
  assert.deepEqual(collapsed.map(({ id }) => id), ["attempt_3"]);
  assert.equal(collapsed[0].attempt_count, 3);
  assert.equal(collapsed[0].prior_failed_count, 2);
  assert.deepEqual(priorAttempts(jobs, collapsed[0]).map(({ id }) => id), ["attempt_1", "attempt_2"]);
  assert.equal(supersededJob(jobs, collapsed[0]), undefined);
  assert.equal(supersededJob(jobs, jobs[2])?.id, "attempt_3");
});

test("jobs without an issue source stay on their own cards", () => {
  const jobs = [
    { id: "plain_1", state: "failed", repository: "machinist", created_at: "2026-10-11T09:00:00Z" },
    issueJob({ id: "attempt_1", state: "failed", attempt: 1, created_at: "2026-10-11T09:00:00Z" }),
    issueJob({ id: "attempt_2", state: "queued", attempt: 2, previous_job_id: "attempt_1", task: { source_url: issue }, created_at: "2026-10-11T10:00:00Z" }),
  ];

  const collapsed = collapseIssueAttempts(jobs);
  assert.deepEqual(collapsed.map(({ id }) => id), ["plain_1", "attempt_2"]);
  const grouped = groupJobsByBoardColumn(collapsed);
  assert.equal(grouped.queued.length, 1);
  assert.equal(grouped.finished.length, 1);
  assert.deepEqual(grouped.finished.map(({ id }) => id), ["plain_1"]);
});
