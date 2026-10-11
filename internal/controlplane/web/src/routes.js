const pages = new Set(["runs", "analytics", "workers", "triggers", "commands", "workflows"]);

export function routeFromHash(hash) {
  const value = hash.replace(/^#\//, "");
  if (value.startsWith("runs/") && value.slice(5)) {
    const [path, query = ""] = value.slice(5).split("?");
    try {
      const tab = new URLSearchParams(query).get("tab") || "";
      return { view: "task", jobID: decodeURIComponent(path), tab };
    } catch {
      return { view: "runs", jobID: "", tab: "" };
    }
  }
  return { view: pages.has(value) ? value : "runs", jobID: "", tab: "" };
}
