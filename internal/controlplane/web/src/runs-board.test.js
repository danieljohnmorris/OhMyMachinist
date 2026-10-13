import assert from "node:assert/strict";
import test from "node:test";
import { attemptDurationMillis, attemptStart, attemptTime, boardCardTimestamp, boardColumnForState, boardDateFilters, boardDateWindow, boardFiltersFromHash, boardFiltersFromParams, boardHash, boardParamsFromFilters, collapseIssueAttempts, collapseJobsByTicket, filterBoardJobs, filterJobs, filterJobsByDate, githubIssueReference, groupJobsByBoardColumn, jobCounts, jobDisplayTitle, jobTicketKey, needsAttention, priorAttempts, resolutionLabel, supersededJob, ticketAttempts } from "./runs-board.js";

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

test("resolved jobs land under Finished, show green and are counted with Succeeded", () => {
  assert.equal(boardColumnForState("resolved"), "finished");
  assert.equal(needsAttention("resolved"), false);

  const jobs = ["succeeded", "resolved", "failed"].map((state) => ({ id: state, state }));
  const grouped = groupJobsByBoardColumn(jobs);
  assert.deepEqual(grouped.finished.map(({ id }) => id), ["succeeded", "resolved", "failed"]);
  assert.deepEqual(filterJobs(jobs, "resolved").map(({ id }) => id), ["resolved"]);
  assert.deepEqual(filterJobs(jobs, "succeeded").map(({ id }) => id), ["succeeded"]);
  assert.deepEqual(filterJobs(jobs, "failed").map(({ id }) => id), ["failed"]);

  const counts = jobCounts(jobs);
  assert.equal(counts.all, 3);
  assert.equal(counts.succeeded, 2);
  assert.equal(counts.resolved, 1);
  assert.equal(counts.failed, 1);
});

test("resolution labels describe how the ticket was closed", () => {
  assert.equal(resolutionLabel({ state: "resolved", resolution_kind: "pr", resolution_detail: "PR #12 (merged by dan)" }), "Resolved via PR #12 (merged by dan)");
  assert.equal(resolutionLabel({ state: "resolved", resolution_kind: "plane", resolution_detail: "Plane Done" }), "Resolved via Plane Done");
  assert.equal(resolutionLabel({ state: "succeeded", resolution_kind: "pr", resolution_detail: "PR #12 (merged by dan)" }), "");
  assert.equal(resolutionLabel({ state: "resolved", resolution_kind: "pr", resolution_detail: "  " }), "");
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

test("board shows active and failed by default; Show succeeded adds succeeded and resolved", () => {
  const jobs = ["queued", "running", "failed", "timed_out", "succeeded", "resolved", "cancelled"]
    .map((state) => ({ id: state, state }));

  assert.deepEqual(filterBoardJobs(jobs, false).map(({ id }) => id), ["queued", "running", "failed", "timed_out"]);
  assert.deepEqual(filterBoardJobs(jobs, true).map(({ id }) => id), ["queued", "running", "failed", "timed_out", "succeeded", "resolved", "cancelled"]);
});

test("board filters parse from and serialize to the URL hash, omitting defaults", () => {
  const defaults = { showSucceeded: false, runsView: "board", dateFilter: "all", boardLayout: "all" };

  assert.equal(boardHash(defaults), "#/runs");
  assert.deepEqual(boardFiltersFromHash("#/runs"), defaults);
  assert.deepEqual(boardFiltersFromHash(""), defaults);

  const all = { showSucceeded: true, runsView: "table", dateFilter: "last7", boardLayout: "projects" };
  assert.equal(boardHash(all), "#/runs?succeeded=1&view=list&range=last7&group=projects");
  assert.deepEqual(boardFiltersFromHash("#/runs?succeeded=1&view=list&range=last7&group=projects"), all);

  // Only non-defaults serialize; each flag can appear alone.
  assert.deepEqual([...boardParamsFromFilters({ ...all, showSucceeded: false, runsView: "board", boardLayout: "all" }).entries()], [["range", "last7"]]);
  assert.deepEqual(boardFiltersFromHash("#/runs?succeeded=1"), { ...defaults, showSucceeded: true });
  assert.deepEqual(boardFiltersFromHash("#/runs?view=list"), { ...defaults, runsView: "table" });
  assert.deepEqual(boardFiltersFromHash("#/runs?range=last7"), { ...defaults, dateFilter: "last7" });
  assert.deepEqual(boardFiltersFromHash("#/runs?group=projects"), { ...defaults, boardLayout: "projects" });

  // Invalid values fall back to defaults, and task hashes carry no board filters.
  assert.deepEqual(boardFiltersFromHash("#/runs?view=bogus&range=nope&group=wat"), defaults);
  assert.deepEqual(boardFiltersFromHash("#/runs/job_1?tab=history"), defaults);
  assert.deepEqual(boardFiltersFromParams(new URLSearchParams("succeeded=0")), defaults);
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

test("ticket keys come from the title first, then the persisted source field", () => {
  assert.equal(jobTicketKey({ title: "SPARK-6: Board collapse", prompt: "ignored" }), "key:SPARK-6");
  assert.equal(jobTicketKey({ github_issue_title: "MACH-13: One card per ticket" }), "key:MACH-13");
  assert.equal(jobTicketKey({ prompt: "no key", metadata: { issue_url: "https://plane.example/tickets/SPARK-6/" } }), "url:https://plane.example/tickets/SPARK-6/");
  assert.equal(jobTicketKey({ prompt: "no key", task: { source_url: "https://plane.example/tickets/SPARK-6/" } }), "url:https://plane.example/tickets/SPARK-6/");
  assert.equal(jobTicketKey({ prompt: "unticketed work", id: "job_1" }), "");
});

test("attempts of one ticket collapse to the latest with priors linked", () => {
  const spark = (id, state, updated) => ({ id, state, title: "SPARK-6: Board collapse", created_at: "2026-10-01T00:00:00Z", updated_at: updated, runs: [] });
  const entries = collapseJobsByTicket([
    spark("spark_cancelled", "cancelled", "2026-10-02T00:00:00Z"),
    spark("spark_latest", "succeeded", "2026-10-11T00:00:00Z"),
    spark("spark_failed", "failed", "2026-10-05T00:00:00Z"),
    { id: "unticketed", state: "running", prompt: "no key", created_at: "2026-10-06T00:00:00Z", runs: [] },
  ]);
  assert.equal(entries.length, 2);
  assert.equal(entries[0].latest.id, "spark_latest");
  assert.deepEqual(entries[0].priors.map(({ id }) => id), ["spark_cancelled", "spark_failed"]);
  assert.equal(entries[1].latest.id, "unticketed");
  assert.equal(entries[1].priors.length, 0);
  assert.deepEqual(entries.map(({ latest }) => attemptTime(latest)), ["2026-10-11T00:00:00Z", "2026-10-06T00:00:00Z"]);
});

test("collapse groups by the persisted source field when titles have no key", () => {
  const source = { task: { source_url: "https://plane.example/tickets/SPARK-6/" }, prompt: "retry", created_at: "2026-10-01T00:00:00Z", runs: [] };
  const entries = collapseJobsByTicket([
    { ...source, id: "old", updated_at: "2026-10-02T00:00:00Z" },
    { ...source, id: "new", updated_at: "2026-10-09T00:00:00Z" },
  ]);
  assert.equal(entries.length, 1);
  assert.equal(entries[0].latest.id, "new");
  assert.deepEqual(entries[0].priors.map(({ id }) => id), ["old"]);
});

test("attempt time prefers the last update and falls back to creation", () => {
  assert.equal(attemptTime({ updated_at: "2026-10-11T00:00:00Z", created_at: "2026-10-01T00:00:00Z" }), "2026-10-11T00:00:00Z");
  assert.equal(attemptTime({ created_at: "2026-10-01T00:00:00Z" }), "2026-10-01T00:00:00Z");
  assert.equal(attemptTime({}), "");
});

test("priorAttempts falls back to same Plane ticket when previous_job_id is absent", () => {
  const jobs = [
    { id: "a", title: "SPARK-6: x", state: "failed", updated_at: "2026-10-02T00:00:00Z", created_at: "2026-10-01T00:00:00Z", runs: [] },
    { id: "b", title: "SPARK-6: x", state: "succeeded", updated_at: "2026-10-11T00:00:00Z", created_at: "2026-10-10T00:00:00Z", runs: [] },
  ];
  assert.deepEqual(priorAttempts(jobs, jobs[1]).map(({ id }) => id), ["a"]);
});

test("ticketAttempts lists every ticket attempt oldest first from any member", () => {
  const spark = (id, attempt, created, extra = {}) => ({
    id,
    attempt,
    title: "SPARK-6: Board collapse",
    created_at: created,
    updated_at: created,
    runs: [],
    ...extra,
  });
  const jobs = [
    spark("newest", 3, "2026-10-10T00:00:00Z"),
    spark("oldest", 1, "2026-10-01T00:00:00Z"),
    spark("middle", 2, "2026-10-05T00:00:00Z"),
    { id: "unticketed", prompt: "no key", created_at: "2026-10-06T00:00:00Z", runs: [] },
  ];
  for (const member of jobs.slice(0, 3)) {
    assert.deepEqual(ticketAttempts(jobs, member).map(({ id }) => id), ["oldest", "middle", "newest"]);
  }
  assert.deepEqual(ticketAttempts(jobs, jobs[3]).map(({ id }) => id), ["unticketed"]);
});

test("ticketAttempts follows previous_job_id chains without a ticket key", () => {
  const jobs = [
    { id: "first", state: "failed", created_at: "2026-10-01T00:00:00Z", runs: [] },
    { id: "second", previous_job_id: "first", state: "failed", created_at: "2026-10-02T00:00:00Z", runs: [] },
    { id: "third", previous_job_id: "second", state: "running", created_at: "2026-10-03T00:00:00Z", runs: [] },
    { id: "unrelated", state: "succeeded", created_at: "2026-10-02T00:00:00Z", runs: [] },
  ];
  assert.deepEqual(ticketAttempts(jobs, jobs[0]).map(({ id }) => id), ["first", "second", "third"]);
  assert.deepEqual(ticketAttempts(jobs, jobs[2]).map(({ id }) => id), ["first", "second", "third"]);
  assert.deepEqual(ticketAttempts(jobs, jobs[3]).map(({ id }) => id), ["unrelated"]);
});

test("attemptStart prefers the earliest run start and falls back to creation", () => {
  assert.equal(
    attemptStart({ created_at: "2026-10-01T00:00:00Z", runs: [{ started_at: "2026-10-02T00:00:00Z" }, { started_at: "2026-10-01T12:00:00Z" }] }),
    "2026-10-01T12:00:00Z",
  );
  assert.equal(attemptStart({ created_at: "2026-10-01T00:00:00Z", runs: [{}] }), "2026-10-01T00:00:00Z");
  assert.equal(attemptStart({ runs: [] }), "");
});

test("attemptDurationMillis measures wall clock, elapsed for in-progress attempts", () => {
  const now = new Date("2026-10-02T00:00:00Z");
  assert.equal(
    attemptDurationMillis({ state: "succeeded", created_at: "2026-10-01T00:00:00Z", runs: [{ started_at: "2026-10-01T00:10:00Z", completed_at: "2026-10-01T00:40:00Z" }, { started_at: "2026-10-01T01:00:00Z", completed_at: "2026-10-01T01:30:00Z" }] }, now),
    80 * 60 * 1000,
    "end is the latest completed run, start is the earliest run start",
  );
  assert.equal(
    attemptDurationMillis({ state: "running", created_at: "2026-10-01T00:00:00Z", runs: [{ started_at: "2026-10-01T00:10:00Z" }] }, now),
    23 * 60 * 60 * 1000 + 50 * 60 * 1000,
    "running attempts show elapsed time to now",
  );
  assert.equal(
    attemptDurationMillis({ state: "queued", created_at: "2026-10-01T00:00:00Z", runs: [] }, now),
    24 * 60 * 60 * 1000,
    "queued attempts elapse from creation",
  );
  assert.equal(attemptDurationMillis({ state: "failed", runs: [{ started_at: "2026-10-01T00:00:00Z" }] }, now), undefined, "terminal without completion times has no duration");
  assert.equal(attemptDurationMillis({ state: "queued", runs: [] }, now), undefined, "no timestamps at all has no duration");
});
