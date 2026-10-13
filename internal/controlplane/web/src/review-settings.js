// Review profiles (MACH-32): resolved per project from the repo base ref,
// the project setting, the global default, or the shipped default profile.
// Stored in the control plane, edited in Settings, consumed by the review
// step.

export function resolvedFromPayload(payload) {
  if (!payload || typeof payload !== "object") {
    return { project: "", source: "", profileName: "", profile: "", extraInstructions: "" };
  }
  const text = (value) => (typeof value === "string" ? value : "");
  return {
    project: text(payload.project),
    source: text(payload.source),
    profileName: text(payload.profile_name),
    profile: text(payload.profile),
    extraInstructions: text(payload.extra_instructions),
  };
}

export function profilesFromPayload(payload) {
  if (!payload || typeof payload !== "object") return { defaultName: "", profiles: [] };
  return {
    defaultName: payload.default || "",
    profiles: Array.isArray(payload.profiles)
      ? payload.profiles.map((profile) => ({
          name: profile.name,
          builtIn: Boolean(profile.built_in),
          body: typeof profile.body === "string" ? profile.body : "",
        }))
      : [],
  };
}

export async function loadReviewProfile(project, signal) {
  const query = project ? `?project=${encodeURIComponent(project)}` : "";
  const response = await fetch(`/api/v1/review-profile${query}`, { signal });
  if (!response.ok) throw new Error(`Review profile request failed (${response.status})`);
  return resolvedFromPayload(await response.json());
}

export async function loadReviewProfiles(signal) {
  const response = await fetch("/api/v1/review-profiles", { signal });
  if (!response.ok) throw new Error(`Review profiles request failed (${response.status})`);
  return profilesFromPayload(await response.json());
}

export async function saveReviewProfileSetting(project, setting, csrfToken) {
  const response = await fetch("/api/v1/review-profile", {
    method: "PUT",
    headers: { "Content-Type": "application/json", "X-Machinist-CSRF": csrfToken },
    body: JSON.stringify({ project, ...setting }),
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    const errors = Array.isArray(payload.errors) ? payload.errors : null;
    throw new Error(
      errors
        ? errors.map((issue) => `line ${issue.line}: ${issue.message}`).join("\n")
        : `Review profile save failed (${response.status})`,
    );
  }
  return resolvedFromPayload(payload);
}

export async function saveReviewDefaultProfile(name, csrfToken) {
  const response = await fetch("/api/v1/review-default-profile", {
    method: "PUT",
    headers: { "Content-Type": "application/json", "X-Machinist-CSRF": csrfToken },
    body: JSON.stringify({ name }),
  });
  if (!response.ok) throw new Error(`Review default profile save failed (${response.status})`);
  return true;
}

export async function previewReviewProfile(request, csrfToken, signal) {
  const response = await fetch("/api/v1/review-profile/preview", {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-Machinist-CSRF": csrfToken },
    body: JSON.stringify(request),
    signal,
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    const errors = Array.isArray(payload.errors) ? payload.errors : null;
    throw new Error(
      errors
        ? errors.map((issue) => `line ${issue.line}: ${issue.message}`).join("\n")
        : `Review profile preview failed (${response.status})`,
    );
  }
  return payload;
}
