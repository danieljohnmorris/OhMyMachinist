// Sidebar concurrency label. `limit` is the configured max_concurrent_jobs
// (null/undefined/0 = unlimited); `runningJobs` is the current running count.
export function concurrencyLabel(limit, runningJobs) {
  if (limit == null || limit <= 0) return "No job limit";
  if (runningJobs > limit) return `${runningJobs} running (limit ${limit}, draining)`;
  return `${limit} job${limit === 1 ? "" : "s"} at a time`;
}

// capacityLabel is the single sidebar line: configured limit plus live
// running/ready counts, so the setting and the state share one row.
export function capacityLabel(limit, runningJobs, readyLanes) {
  const live = `${runningJobs} running / ${readyLanes} ready`;
  if (limit == null || limit <= 0) return `No job limit · ${live}`;
  return `${limit} job${limit === 1 ? "" : "s"} at a time · ${live}${runningJobs > limit ? " (draining)" : ""}`;
}
