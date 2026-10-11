import { formatDurationMillis } from "./run-metrics.js";
import { relativeTime } from "./relative-time.js";

const zeroTime = "0001-01-01T00:00:00Z";
const terminalStates = new Set(["succeeded", "failed", "cancelled"]);

function validTimestamp(value) {
  return Boolean(value && value !== zeroTime && Number.isFinite(Date.parse(value)));
}

// Persisted state history recorded on every job state change. Consecutive
// duplicates are collapsed so unexpected repeated writes stay invisible.
function recordedHistory(job) {
  const entries = [];
  for (const entry of job.state_history || []) {
    if (!entry?.state || !validTimestamp(entry.changed_at)) continue;
    if (entries.length && entries.at(-1).state === entry.state) continue;
    entries.push({ state: entry.state, changed_at: entry.changed_at });
  }
  return entries;
}

// Older jobs predate the history. Show what timestamps still exist instead of
// an empty box: creation (queued), the run start (running) and its end.
function fallbackHistory(job) {
  const entries = [];
  if (validTimestamp(job.created_at)) entries.push({ state: "queued", changed_at: job.created_at });
  const run = job.runs?.at(-1);
  if (run && validTimestamp(run.started_at)) entries.push({ state: "running", changed_at: run.started_at });
  if (run && validTimestamp(run.completed_at)) entries.push({ state: run.state || job.state, changed_at: run.completed_at });
  return entries;
}

function row(kind, fields, at, now, durationMs) {
  return {
    kind,
    at,
    ago: relativeTime(at, now),
    ...(durationMs == null ? {} : { durationMs, duration: formatDurationMillis(durationMs) }),
    ...fields,
  };
}

// One row per state transition, oldest first, plus a final row for the state
// the job is still in. Each transition row carries the duration spent in the
// state it left; the final row carries the live duration while in progress.
export function activityRows(job, now = new Date()) {
  if (!job) return [];
  let history = recordedHistory(job);
  if (!history.length) history = fallbackHistory(job);
  if (!history.length) return [];

  const rows = [];
  for (let index = 1; index < history.length; index += 1) {
    const at = history[index].changed_at;
    const durationMs = Math.max(0, Date.parse(at) - Date.parse(history[index - 1].changed_at));
    rows.push(row("transition", { from: history[index - 1].state, to: history[index].state }, at, now, durationMs));
  }
  const current = history.at(-1);
  const terminal = terminalStates.has(current.state);
  const end = terminal
    ? (job.runs || []).map((run) => run.completed_at).findLast(validTimestamp)
    : null;
  const durationMs = !terminal && validTimestamp(current.changed_at)
    ? Math.max(0, now.getTime() - Date.parse(current.changed_at))
    : null;
  rows.push(row("state", { state: current.state, live: !terminal }, current.changed_at, now, durationMs));
  return rows;
}
