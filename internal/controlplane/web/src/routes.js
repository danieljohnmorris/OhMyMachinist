const pages = new Set(["runs", "analytics", "workers", "triggers", "commands", "workflows", "settings", "backlog"]);

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
      return { view: "runs", jobID: "", tab: "", attempt: "", issueID: "" };
    }
  }
  // Plain pages may carry a query (e.g. #/backlog?issue=<id>); task routes
  // are handled above and keep their own parsing.
  const [path, query = ""] = value.split("?");
  let issueID = "";
  try {
    issueID = new URLSearchParams(query).get("issue") || "";
  } catch {
    issueID = "";
  }
  return { view: pages.has(path) ? path : "runs", jobID: "", tab: "", attempt: "", issueID };
}
