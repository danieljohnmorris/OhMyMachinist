// Per-project PR review instructions (MACH-26): stored in the control plane,
// edited per project in Settings, consumed by the built-in review step.

export function instructionsFromPayload(payload) {
  if (!payload || typeof payload !== "object") return { instructions: "", custom: false };
  return {
    instructions: typeof payload.instructions === "string" ? payload.instructions : "",
    custom: Boolean(payload.custom),
  };
}

export async function loadReviewInstructions(project, signal) {
  const response = await fetch(`/api/v1/review-instructions?project=${encodeURIComponent(project)}`, { signal });
  if (!response.ok) throw new Error(`Review instructions request failed (${response.status})`);
  return instructionsFromPayload(await response.json());
}

export async function saveReviewInstructions(project, instructions, csrfToken) {
  const response = await fetch("/api/v1/review-instructions", {
    method: "PUT",
    headers: { "Content-Type": "application/json", "X-Machinist-CSRF": csrfToken },
    body: JSON.stringify({ project, instructions }),
  });
  if (!response.ok) throw new Error(`Review instructions save failed (${response.status})`);
  return instructionsFromPayload(await response.json());
}
