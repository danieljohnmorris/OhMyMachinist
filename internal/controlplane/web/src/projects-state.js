// Projects view state helpers: the resolved tracker projects joined with the
// raw settings, and the [tracker.projects.NAME] table serializer.

// mergeProjects joins the resolved projects with the raw settings so the table
// can show which entries are settings overrides and keep every field the
// server resolved (defaults applied, names uppercased).
export function mergeProjects(resolved, settings) {
  const overrides = Object.keys(settings)
    .filter((key) => key.startsWith("tracker.projects."))
    .map((key) => key.slice("tracker.projects.".length).toUpperCase());
  const projects = Object.entries(resolved).map(([name, project]) => ({
    name,
    repository: project.Repository || "",
    command: project.Command || "",
    model: project.Model || "",
    source: overrides.includes(name) ? "override" : "config file",
  }));
  // A setting the server has not resolved yet (invalid command or name) still
  // shows up, so the operator can fix it here instead of via the API.
  for (const name of overrides) {
    if (!projects.some((project) => project.name === name)) {
      projects.push({ name, repository: "", command: "", model: "", source: "override" });
    }
  }
  return projects.sort((a, b) => a.name.localeCompare(b.name));
}

// serializeProjectTOML renders the [tracker.projects.NAME] table body. Empty
// fields are omitted so the server falls back to the tracker/command defaults.
export function serializeProjectTOML(project) {
  return ["repository", "command", "model"]
    .map((field) => [field, (project[field] || "").trim()])
    .filter(([, value]) => value !== "")
    .map(([field, value]) => `${field} = ${JSON.stringify(value)}`)
    .join("\n");
}
