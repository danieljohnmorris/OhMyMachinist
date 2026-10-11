// Sidebar concurrency label. `limit` is the configured max_concurrent_jobs
// (null/undefined/0 = unlimited); `runningJobs` is the current running count.
export function concurrencyLabel(limit, runningJobs) {
  if (limit == null || limit <= 0) return "No job limit";
  if (runningJobs > limit) return `${runningJobs} running (limit ${limit}, draining)`;
  return `${limit} job${limit === 1 ? "" : "s"} at a time`;
}
