const pages = new Set(["runs", "tickets", "projects", "analytics", "workers", "factory", "triggers", "commands", "workflows", "skills", "settings"]);

export function routeFromHash(hash) {
  const value = hash.replace(/^#\//, "");
  if (value.startsWith("runs/") && value.slice(5)) {
    const [path, query = ""] = value.slice(5).split("?");
    try {
      const params = new URLSearchParams(query);
      return {
        view: "task",
        jobID: decodeURIComponent(path),
        tab: params.get("tab") || "",
        attempt: params.get("attempt") || "",
      };
    } catch {
      return { view: "runs", jobID: "", tab: "", attempt: "" };
    }
  }
  if (value.startsWith("tickets/") && value.slice(8)) {
    const [path] = value.slice(8).split("?");
    try {
      return { view: "tickets", jobID: "", tab: "", attempt: "", ticketKey: decodeURIComponent(path) };
    } catch {
      return { view: "tickets", jobID: "", tab: "", attempt: "", ticketKey: "" };
    }
  }
  return { view: pages.has(value) ? value : "runs", jobID: "", tab: "", attempt: "" };
}
