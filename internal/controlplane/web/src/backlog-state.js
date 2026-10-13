// Pure helpers for the native backlog page: state maps, client-side
// filtering, and payload builders for the backlog HTTP API.

export const backlogStates = ["backlog", "todo", "started", "completed", "cancelled"];

export const backlogStateLabels = {
  backlog: "Backlog",
  todo: "Todo",
  started: "Started",
  completed: "Completed",
  cancelled: "Cancelled",
};

export const backlogStateTones = {
  backlog: "border-border bg-muted text-muted-foreground",
  todo: "border-primary/25 bg-primary/10 text-primary",
  started: "border-warning/25 bg-warning/10 text-warning",
  completed: "border-success/25 bg-success/10 text-success",
  cancelled: "border-danger/25 bg-danger/10 text-danger",
};

export function backlogStateLabel(state) {
  return backlogStateLabels[state] || String(state || "unknown");
}

export function isValidBacklogState(state) {
  return backlogStates.includes(state);
}

// Client-side filters mirror the API's project= and q= parameters so typing
// stays instant between polls.
export function filterBacklogIssues(issues, { project = "", query = "" } = {}) {
  const needle = query.trim().toLowerCase();
  return (issues || []).filter((issue) => {
    if (project && issue.project !== project) return false;
    if (!needle) return true;
    const haystack = [issue.key, issue.title, issue.spec]
      .map((value) => (typeof value === "string" ? value : ""))
      .join("\n")
      .toLowerCase();
    return haystack.includes(needle);
  });
}

export function groupIssuesByState(issues) {
  const groups = Object.fromEntries(backlogStates.map((state) => [state, []]));
  for (const issue of issues || []) {
    (groups[issue.state] || groups.backlog).push(issue);
  }
  return groups;
}

// Project options: projects already present in the backlog plus the
// repositories the UI knows, deduplicated and sorted. An empty result means
// the composer falls back to free text.
export function backlogProjectOptions(issues, repositories = []) {
  const known = new Set();
  for (const issue of issues || []) {
    if (typeof issue?.project === "string" && issue.project.trim()) known.add(issue.project.trim());
  }
  for (const repository of repositories || []) {
    if (typeof repository === "string" && repository.trim()) known.add(repository.trim());
  }
  return [...known].sort();
}

export function createIssuePayload({ project, title, spec = "", url = "" } = {}) {
  const payload = {
    project: String(project || "").trim(),
    title: String(title || "").trim(),
  };
  const specValue = String(spec || "").trim();
  if (specValue) payload.spec = specValue;
  const urlValue = String(url || "").trim();
  if (urlValue) payload.url = urlValue;
  return payload;
}

// PATCH bodies carry only the fields the user can edit; the server bumps the
// revision and never enqueues anything.
export function issueUpdatePayload({ title, spec, state } = {}) {
  const payload = {};
  if (typeof title === "string" && title.trim()) payload.title = title.trim();
  if (typeof spec === "string") payload.spec = spec;
  if (isValidBacklogState(state)) payload.state = state;
  return payload;
}

// Start bodies always carry a repository; a workflow excludes a command, and
// empty optionals are omitted.
export function startRunPayload({ repository, workflow = "", command = "", model = "" } = {}) {
  const payload = { repository: String(repository || "").trim() };
  const workflowValue = String(workflow || "").trim();
  const commandValue = String(command || "").trim();
  if (workflowValue) payload.workflow = workflowValue;
  else if (commandValue) payload.command = commandValue;
  const modelValue = String(model || "").trim();
  if (modelValue) payload.model = modelValue;
  return payload;
}
