export const boardColumns = [
  { id: "queued", title: "Queued", description: "Waiting to start" },
  { id: "running", title: "In progress", description: "Work underway" },
  { id: "attention", title: "Needs attention", description: "Approval or input needed" },
  { id: "finished", title: "Finished", description: "Completed or stopped" },
];

const activeStates = new Set(["queued", "running"]);
const failedStates = new Set(["failed", "timed_out"]);
const succeededStates = new Set(["succeeded", "resolved"]);

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
  return !activeStates.has(state) && !succeededStates.has(state);
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
  if (chain.length) return chain.reverse();
  // Plane-intake jobs often lack previous_job_id; fall back to same ticket key.
  const key = jobTicketKey(job);
  if (!key) return [];
  return jobs
    .filter((candidate) => candidate.id !== job.id && jobTicketKey(candidate) === key
      && Date.parse(attemptTime(candidate) || 0) < Date.parse(attemptTime(job) || 0))
    .sort((left, right) => Date.parse(attemptTime(left) || 0) - Date.parse(attemptTime(right) || 0));
}

export function supersededJob(jobs, job) {
  if (!job.superseded_by) return undefined;
  return jobs.find((candidate) => candidate.id === job.superseded_by);
}

const terminalAttemptStates = new Set(["succeeded", "failed", "cancelled", "timed_out"]);

function validAttemptTimestamp(value) {
  return Boolean(value && value !== zeroTime && Number.isFinite(Date.parse(value)));
}

// ticketAttempts lists every attempt for the same Plane ticket as job, oldest
// first, including job itself. Attempts are matched through previous_job_id
// chains in both directions and by ticket key, so retitled or half-deleted
// chains still resolve. Jobs deleted since a link was made are skipped.
export function ticketAttempts(jobs, job) {
  const group = new Map([[job.id, job]]);
  let changed = true;
  while (changed) {
    changed = false;
    for (const candidate of jobs) {
      if (group.has(candidate.id)) continue;
      const linked =
        (candidate.previous_job_id && group.has(candidate.previous_job_id)) ||
        [...group.values()].some((member) => member.previous_job_id === candidate.id);
      if (linked) {
        group.set(candidate.id, candidate);
        changed = true;
      }
    }
  }
  const key = jobTicketKey(job);
  if (key) {
    for (const candidate of jobs) {
      if (candidate.id !== job.id && jobTicketKey(candidate) === key) group.set(candidate.id, candidate);
    }
  }
  return [...group.values()].sort((left, right) => {
    const leftRank = attemptRank(left);
    const rightRank = attemptRank(right);
    return leftRank < rightRank ? -1 : leftRank > rightRank ? 1 : 0;
  });
}

// Start of an attempt: its earliest run start, falling back to job creation.
export function attemptStart(job) {
  const starts = (job.runs || []).map((run) => run.started_at).filter(validAttemptTimestamp);
  if (!starts.length) return validAttemptTimestamp(job.created_at) ? job.created_at : "";
  return starts.reduce((earliest, value) => (Date.parse(value) < Date.parse(earliest) ? value : earliest));
}

// Wall-clock duration of an attempt: from its start to the latest completed
// run; attempts still in progress measure elapsed time to `now`.
export function attemptDurationMillis(job, now = new Date()) {
  const start = Date.parse(attemptStart(job));
  if (!Number.isFinite(start)) return undefined;
  if (!terminalAttemptStates.has(job.state)) return Math.max(0, now.getTime() - start);
  const ends = (job.runs || []).map((run) => run.completed_at).filter(validAttemptTimestamp);
  if (!ends.length) return undefined;
  return Math.max(0, Math.max(...ends.map((value) => Date.parse(value))) - start);
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
    if (filter === "resolved") return job.state === "resolved";
    return true;
  });
}

// Board membership: active and failed jobs by default. The "Show succeeded"
// tickbox additionally includes succeeded and resolved tickets.
export function filterBoardJobs(jobs, showSucceeded) {
  if (showSucceeded) return jobs;
  return jobs.filter((job) => activeStates.has(job.state) || failedStates.has(job.state));
}

// Board filters round-trip through the URL hash query (e.g. #/runs?succeeded=1&range=last7)
// so refresh and browser back/forward restore them. Defaults are omitted.
export function boardFiltersFromParams(params) {
  return {
    showSucceeded: params.get("succeeded") === "1",
    runsView: params.get("view") === "list" ? "table" : "board",
    dateFilter: boardDateFilters.some(({ id }) => id === params.get("range")) ? params.get("range") : "all",
    boardLayout: params.get("group") === "projects" ? "projects" : "all",
  };
}

export function boardFiltersFromHash(hash) {
  const value = hash.replace(/^#\/runs\b/, "");
  const queryStart = value.indexOf("?");
  const query = queryStart === -1 ? "" : value.slice(queryStart + 1);
  return boardFiltersFromParams(new URLSearchParams(query));
}

export function boardParamsFromFilters(filters) {
  const params = new URLSearchParams();
  if (filters.showSucceeded) params.set("succeeded", "1");
  if (filters.runsView === "table") params.set("view", "list");
  if (filters.dateFilter && filters.dateFilter !== "all") params.set("range", filters.dateFilter);
  if (filters.boardLayout && filters.boardLayout !== "all") params.set("group", filters.boardLayout);
  return params;
}

export function boardHash(filters) {
  const params = boardParamsFromFilters(filters).toString();
  return `#/runs${params ? `?${params}` : ""}`;
}

export const boardDateFilters = [
  { id: "today", label: "Today" },
  { id: "yesterday", label: "Yesterday" },
  { id: "last7", label: "Last 7 days" },
  { id: "all", label: "All" },
];

function startOfLocalDay(date) {
  const day = new Date(date);
  day.setHours(0, 0, 0, 0);
  return day;
}

// Date filters use local calendar days, not a rolling 168-hour window:
// "Last 7 days" is today plus the previous six calendar days.
export function boardDateWindow(now, filter) {
  const todayStart = startOfLocalDay(now);
  const tomorrowStart = startOfLocalDay(now);
  tomorrowStart.setDate(tomorrowStart.getDate() + 1);
  if (filter === "today") return { start: todayStart, end: tomorrowStart };
  if (filter === "yesterday") {
    const start = startOfLocalDay(now);
    start.setDate(start.getDate() - 1);
    return { start, end: todayStart };
  }
  if (filter === "last7") {
    const start = startOfLocalDay(now);
    start.setDate(start.getDate() - 6);
    return { start, end: tomorrowStart };
  }
  return null;
}

export function filterJobsByDate(jobs, filter, now = new Date()) {
  const window = boardDateWindow(now, filter);
  if (!window) return jobs;
  return jobs.filter((job) => {
    const timestamp = Date.parse(boardCardTimestamp(job));
    return Number.isFinite(timestamp) && timestamp >= window.start.getTime() && timestamp < window.end.getTime();
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
    if (succeededStates.has(job.state)) result.succeeded += 1;
    if (job.state === "resolved") result.resolved += 1;
    return result;
  }, { all: 0, active: 0, failed: 0, succeeded: 0, resolved: 0 });
}

// resolutionLabel describes how a resolved ticket was closed, e.g.
// "Resolved via PR #12 (merged by dan)" or "Resolved via Plane Done".
export function resolutionLabel(job) {
  const detail = typeof job?.resolution_detail === "string" ? job.resolution_detail.trim() : "";
  return job?.state === "resolved" && detail ? `Resolved via ${detail}` : "";
}

export function currentRun(job) {
  if (job.workflow) return job.runs.at(-1);
  return [...job.runs].reverse().find((run) => run.state !== "queued") || job.runs[0];
}


// Attempts are ordered by when they were created: a cancel or late status
// change must not make an older attempt look like the latest.
export function attemptTime(job) {
  return job.created_at || job.updated_at || "";
}

const ticketKeyPattern = /^([A-Z][A-Z0-9]{1,9}-\d+)\b/;

// Collapse key for one Plane ticket: the issue key leading the displayed title
// (e.g. SPARK-6), falling back to the persisted source field that identifies
// the ticket. Empty when the job is not tied to a ticket.
export function jobTicketKey(job) {
  const title = jobDisplayTitle(job);
  const fromTitle = typeof title === "string" ? title.trim().match(ticketKeyPattern)?.[1] : undefined;
  if (fromTitle) return `key:${fromTitle}`;
  for (const source of [job.metadata?.issue_url, job.task?.source_url]) {
    if (typeof source === "string" && source.trim()) return `url:${source.trim()}`;
  }
  return "";
}

// One entry per Plane ticket: the latest attempt as the card and every older
// attempt linked from it. Jobs without a ticket key stay as their own entry.
export function collapseJobsByTicket(jobs) {
  const groups = new Map();
  const entries = [];
  for (const job of jobs) {
    const key = jobTicketKey(job);
    const group = key ? groups.get(key) : undefined;
    if (!group) {
      const entry = { latest: job, priors: [] };
      if (key) groups.set(key, entry);
      entries.push(entry);
      continue;
    }
    if (Date.parse(attemptTime(job)) > Date.parse(attemptTime(group.latest))) {
      group.priors.push(group.latest);
      group.latest = job;
    } else {
      group.priors.push(job);
    }
  }
  return entries;
}

export function groupEntriesByBoardColumn(entries) {
  const groups = { queued: [], running: [], attention: [], finished: [] };
  for (const entry of entries) groups[boardColumnForState(entry.latest.state)].push(entry);
  return groups;
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
