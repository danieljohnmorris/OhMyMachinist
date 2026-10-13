const pages = new Set(["runs", "analytics", "workers", "triggers", "commands", "workflows", "settings"]);

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
  return { view: pages.has(value) ? value : "runs", jobID: "", tab: "", attempt: "" };
}
