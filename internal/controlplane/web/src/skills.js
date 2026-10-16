// Skills library (MACH-51): the installed versioned skills exposed by
// /api/v1/skills, shown on the Skills page and consumed by the per-project
// skills editor. Parsing is defensive: the server always sends a list, but a
// malformed or partial payload must not crash the page.

function text(value) {
  return typeof value === "string" ? value : "";
}

function versionsFromPayload(value) {
  if (!Array.isArray(value)) return [];
  return value.map((version) => ({
    version: text(version?.version),
    resolved: text(version?.resolved),
    installedAt: text(version?.installed_at),
    scanned: Boolean(version?.scanned),
  }));
}

export function skillsFromPayload(payload) {
  if (!payload || typeof payload !== "object" || !Array.isArray(payload.skills)) return [];
  return payload.skills.map((skill) => ({
    name: text(skill?.name),
    source: text(skill?.source),
    active: text(skill?.active),
    versions: versionsFromPayload(skill?.versions),
    usedBy: Array.isArray(skill?.used_by) ? skill.used_by.map(text).filter(Boolean) : [],
  }));
}

// activeVersion returns the installed version a skill currently resolves.
export function activeVersion(skill) {
  return skill?.versions?.find((version) => version.version === skill.active) || null;
}

// skillInstalledAt is when the active version was installed.
export function skillInstalledAt(skill) {
  return activeVersion(skill)?.installedAt || "";
}

// skillUpdatedAt is when the most recent version was installed. Versions are
// ordered oldest first, so that is the last entry.
export function skillUpdatedAt(skill) {
  return skill?.versions?.length ? skill.versions[skill.versions.length - 1].installedAt : "";
}

// shortVersion trims a full version identity for a table cell.
export function shortVersion(version) {
  const value = text(version);
  return value.length > 12 ? value.slice(0, 12) : value;
}

// skillNames lists the installed skill names, for validation and datalists.
export function skillNames(skills) {
  return (skills || []).map((skill) => skill.name).filter(Boolean);
}

async function readError(response, fallback) {
  const body = await response.json().catch(() => ({}));
  return new Error(body.error || `${fallback} (${response.status})`);
}

export async function loadSkills(signal) {
  const response = await fetch("/api/v1/skills", { headers: { Accept: "application/json" }, signal });
  if (!response.ok) throw await readError(response, "Skills request failed");
  return skillsFromPayload(await response.json());
}

export async function installSkill(source, csrfToken) {
  const response = await fetch("/api/v1/skills", {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Machinist-CSRF": csrfToken },
    body: JSON.stringify({ source }),
  });
  if (!response.ok) throw await readError(response, "Install failed");
  return response.json();
}

export async function uploadSkill(file, csrfToken) {
  const form = new FormData();
  form.append("file", file);
  const response = await fetch("/api/v1/skills/upload", {
    method: "POST",
    headers: { "X-Machinist-CSRF": csrfToken },
    body: form,
  });
  if (!response.ok) throw await readError(response, "Upload failed");
  return response.json();
}

export async function updateSkill(name, csrfToken) {
  const response = await fetch(`/api/v1/skills/${encodeURIComponent(name)}/update`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Machinist-CSRF": csrfToken },
    body: JSON.stringify({ activate: true }),
  });
  if (!response.ok) throw await readError(response, "Update failed");
  return response.json();
}

export async function pinSkill(name, version, csrfToken) {
  const response = await fetch(`/api/v1/skills/${encodeURIComponent(name)}/pin`, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Machinist-CSRF": csrfToken },
    body: JSON.stringify({ version }),
  });
  if (!response.ok) throw await readError(response, "Pin failed");
  return response.json();
}

export async function uninstallSkill(name, force, csrfToken) {
  const query = force ? "?force=true" : "";
  const response = await fetch(`/api/v1/skills/${encodeURIComponent(name)}${query}`, {
    method: "DELETE",
    headers: { "X-Machinist-CSRF": csrfToken },
  });
  if (!response.ok) throw await readError(response, "Uninstall failed");
  return true;
}