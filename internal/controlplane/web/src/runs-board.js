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

export function canCancelJob(job) {
  return !job.workflow && job.state === "queued";
}

export function runProgress(runs) {
  const completeStates = new Set(["succeeded", "failed", "timed_out", "cancelled"]);
  return { completed: runs.filter((run) => completeStates.has(run.state)).length, total: runs.length };
}
