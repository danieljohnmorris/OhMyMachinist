export const boardColumns = [
  { id: "queued", title: "Queued", description: "Waiting to start" },
  { id: "running", title: "In progress", description: "Work underway" },
  { id: "attention", title: "Needs attention", description: "Approval or input needed" },
  { id: "finished", title: "Finished", description: "Completed or stopped" },
];

const activeStates = new Set(["queued", "running"]);
const failedStates = new Set(["failed", "timed_out"]);

export function boardColumnForState(state) {
  if (state === "queued") return "queued";
  if (state === "running") return "running";
  if (["blocked", "awaiting_approval", "interrupted"].includes(state)) return "attention";
  return "finished";
}

const zeroTime = "0001-01-01T00:00:00Z";

export function boardCardTimestamp(job) {
  if (boardColumnForState(job.state) === "finished") {
    const completedAt = (job.runs || [])
      .filter((run) => run.completed_at && run.completed_at !== zeroTime && Number.isFinite(Date.parse(run.completed_at)))
      .map((run) => run.completed_at)
      .sort((left, right) => Date.parse(right) - Date.parse(left));
    if (completedAt.length) return completedAt[0];
  }
  return job.updated_at || job.created_at || "";
}

export function needsAttention(state) {
  return !activeStates.has(state) && state !== "succeeded";
}

export function issueKey(job) {
  const subject = typeof job.trigger_subject === "string" ? job.trigger_subject.trim() : "";
  const source = job.task?.source_url;
  return subject || (typeof source === "string" ? source.trim() : "");
}

function attemptRank(job) {
  return `${String(job.attempt || 1).padStart(10, "0")}\n${job.created_at || ""}`;
}

// priorAttempts walks the previous_job_id chain backwards, oldest attempt first.
// Jobs deleted since the link was made are skipped; the chain still renders.
export function priorAttempts(jobs, job) {
  const byID = new Map(jobs.map((candidate) => [candidate.id, candidate]));
  const chain = [];
  const seen = new Set([job.id]);
  let current = job;
  while (current?.previous_job_id && !seen.has(current.previous_job_id)) {
    seen.add(current.previous_job_id);
    current = byID.get(current.previous_job_id);
    if (current) chain.push(current);
  }
  return chain.reverse();
}

export function supersededJob(jobs, job) {
  if (!job.superseded_by) return undefined;
  return jobs.find((candidate) => candidate.id === job.superseded_by);
}

// collapseIssueAttempts reduces jobs to one entry per source issue: the latest
// attempt, annotated with how many prior attempts failed. Jobs without an issue
// key, and issues with a single attempt, pass through unchanged. Input order is
// preserved, so the board keeps its newest-first listing.
export function collapseIssueAttempts(jobs) {
  const latest = new Map();
  for (const job of jobs) {
    const key = issueKey(job);
    if (!key) continue;
    const group = `${job.repository}\n${key}`;
    const incumbent = latest.get(group);
    if (!incumbent || attemptRank(job) > attemptRank(incumbent)) latest.set(group, job);
  }
  return jobs.flatMap((job) => {
    const key = issueKey(job);
    if (!key) return [job];
    if (latest.get(`${job.repository}\n${key}`)?.id !== job.id) return [];
    const chain = priorAttempts(jobs, job);
    if (!chain.length) return [job];
    return [
      {
        ...job,
        attempt_count: chain.length + 1,
        prior_failed_count: chain.filter((attempt) => failedStates.has(attempt.state)).length,
      },
    ];
  });
}

export function filterJobs(jobs, filter) {
  return jobs.filter((job) => {
    if (filter === "active") return activeStates.has(job.state);
    if (filter === "failed") return failedStates.has(job.state);
    if (filter === "succeeded") return job.state === "succeeded";
    return true;
  });
}

export function groupJobsByBoardColumn(jobs) {
  const groups = { queued: [], running: [], attention: [], finished: [] };
  for (const job of jobs) groups[boardColumnForState(job.state)].push(job);
  return groups;
}

export function jobCounts(jobs) {
  return jobs.reduce((result, job) => {
    result.all += 1;
    if (activeStates.has(job.state)) result.active += 1;
    if (failedStates.has(job.state)) result.failed += 1;
    if (job.state === "succeeded") result.succeeded += 1;
    return result;
  }, { all: 0, active: 0, failed: 0, succeeded: 0 });
}

export function currentRun(job) {
  if (job.workflow) return job.runs.at(-1);
  return [...job.runs].reverse().find((run) => run.state !== "queued") || job.runs[0];
}

export function jobDisplayTitle(job) {
  const storedTitle = typeof job.title === "string" ? job.title.trim() : "";
  const title = typeof job.github_issue_title === "string" ? job.github_issue_title.trim() : "";
  return storedTitle || job.task?.title || title || job.task?.spec || job.task?.source_url || job.prompt || job.id;
}

export function githubIssueReference(job) {
  const match = typeof job.trigger_subject === "string" ? job.trigger_subject.match(/\/issues\/(\d+)\/?$/) : null;
  return match ? `#${match[1]}` : "";
}

export function runProgress(runs) {
  const completeStates = new Set(["succeeded", "failed", "timed_out", "cancelled"]);
  return { completed: runs.filter((run) => completeStates.has(run.state)).length, total: runs.length };
}
