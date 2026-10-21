const pages = new Set(["runs", "tickets", "projects", "analytics", "settings"]);

const settingsSections = new Set(["review", "workers", "triggers", "factory", "workflows", "skills"]);

// Workers, triggers, factory, workflows and skills used to be top-level pages.
// Their old hashes still work and open the matching Settings tab.
const settingsAliases = {
  workers: "workers",
  triggers: "triggers",
  factory: "factory",
  workflows: "workflows",
  commands: "workflows",
  skills: "skills",
};

export function routeFromHash(hash) {
  const value = hash.replace(/^#\//, "");
  if (value.startsWith("runs/") && value.slice(5)) {
    const [path, query = ""] = value.slice(5).split("?");
    try {
      const params = new URLSearchParams(query);
      return {
        view: "task",
        section: "",
        jobID: decodeURIComponent(path),
        tab: params.get("tab") || "",
        attempt: params.get("attempt") || "",
      };
    } catch {
      return { view: "runs", section: "", jobID: "", tab: "", attempt: "" };
    }
  }
  if (value.startsWith("tickets/") && value.slice(8)) {
    const [path] = value.slice(8).split("?");
    try {
      return { view: "tickets", section: "", jobID: "", tab: "", attempt: "", ticketKey: decodeURIComponent(path) };
    } catch {
      return { view: "tickets", section: "", jobID: "", tab: "", attempt: "", ticketKey: "" };
    }
  }
  const [path] = value.split("?");
  if (path === "settings") {
    return { view: "settings", section: "review", jobID: "", tab: "", attempt: "" };
  }
  if (path.startsWith("settings/")) {
    const section = path.slice("settings/".length);
    return { view: "settings", section: settingsSections.has(section) ? section : "review", jobID: "", tab: "", attempt: "" };
  }
  if (Object.hasOwn(settingsAliases, path)) {
    return { view: "settings", section: settingsAliases[path], jobID: "", tab: "", attempt: "" };
  }
  return { view: pages.has(path) ? path : "runs", section: "", jobID: "", tab: "", attempt: "" };
}

export function settingsHash(section) {
  return `#/settings/${settingsSections.has(section) ? section : "review"}`;
}
