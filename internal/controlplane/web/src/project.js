export const projectKeyPattern = /^([A-Z][A-Z0-9]+)-/i;

export function resolveJobProject(job) {
  const explicit = typeof job?.metadata?.project === "string" ? job.metadata.project.trim() : "";
  if (explicit) return explicit;
  const title = jobDisplayTitle(job);
  const match = title.match(projectKeyPattern)?.[1];
  return match ? match.toUpperCase() : "Other";
}

export function groupJobsByProject(jobs) {
  const groups = new Map();
  for (const job of jobs) {
    const project = resolveJobProject(job);
    if (!groups.has(project)) groups.set(project, { jobs: [], activity: "" });
    const group = groups.get(project);
    group.jobs.push(job);
    const activity = job.updated_at || job.created_at || "";
    if (!group.activity || Date.parse(activity) > Date.parse(group.activity)) {
      group.activity = activity;
    }
  }
  return [...groups.entries()]
    .sort(([, left], [, right]) => Date.parse(right.activity) - Date.parse(left.activity))
    .map(([project, group]) => [project, group.jobs]);
}

function jobDisplayTitle(job) {
  const storedTitle = typeof job?.title === "string" ? job.title.trim() : "";
  const title = typeof job?.github_issue_title === "string" ? job.github_issue_title.trim() : "";
  return storedTitle || job.task?.title || title || job.task?.spec || job.task?.source_url || job.prompt || job.id;
}
