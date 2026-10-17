// Client for the ticket tracker API. Every call throws an Error with an
// actionable message; a missing endpoint (404) is reported as such, never as
// an empty ticket list.

export const ticketStates = [
  { id: "backlog", label: "Backlog" },
  { id: "todo", label: "Todo" },
  { id: "approved", label: "Approved" },
  { id: "in_progress", label: "In progress" },
  { id: "in_review", label: "In review" },
  { id: "done", label: "Done" },
  { id: "cancelled", label: "Cancelled" },
];

const stateLabels = new Map(ticketStates.map((state) => [state.id, state.label]));

export function ticketStateLabel(state) {
  return stateLabels.get(state) || String(state || "unknown").replaceAll("_", " ");
}

// ticketStateTone maps a ticket state onto a Badge tone (DESIGN.md Tone Map).
export function ticketStateTone(state) {
  if (state === "in_progress") return "active";
  if (state === "done") return "success";
  if (state === "approved" || state === "in_review") return "warning";
  return "neutral";
}

// canApproveTicket: only tickets not yet handed to the factory or finished.
export function canApproveTicket(ticket) {
  return Boolean(ticket) && ["backlog", "todo"].includes(ticket.state) && Boolean(ticket.repository);
}

export class TicketAPIError extends Error {
  constructor(message, status) {
    super(message);
    this.status = status;
  }
}

async function request(path, { method = "GET", body, csrfToken, fallback }) {
  const headers = { Accept: "application/json" };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  if (method !== "GET") headers["X-Machinist-CSRF"] = csrfToken || "";
  let response;
  try {
    response = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  } catch (error) {
    throw new TicketAPIError(`${fallback}: the control plane is unreachable (${error.message}).`, 0);
  }
  const payload = await response.json().catch(() => ({}));
  if (response.status === 404 && path === "/api/v1/tickets") {
    throw new TicketAPIError("This control plane has no ticket API. Update and restart machinist, then retry.", 404);
  }
  if (!response.ok) {
    throw new TicketAPIError(payload.error || `${fallback} (${response.status})`, response.status);
  }
  return payload;
}

export async function listTickets() {
  const payload = await request("/api/v1/tickets", { fallback: "Unable to load tickets" });
  return Array.isArray(payload.tickets) ? payload.tickets : [];
}

export async function loadTrackers() {
  const payload = await request("/api/v1/trackers", { fallback: "Unable to load trackers" });
  return {
    default: payload.default || "builtin",
    trackers: Array.isArray(payload.trackers) ? payload.trackers : [],
    projects: payload.projects && typeof payload.projects === "object" ? payload.projects : {},
  };
}

function ticketPath(key, suffix = "") {
  return `/api/v1/tickets/${encodeURIComponent(key)}${suffix}`;
}

export function getTicket(key) {
  return request(ticketPath(key), { fallback: `Unable to load ${key}` });
}

export function createTicket(csrfToken, fields) {
  return request("/api/v1/tickets", { method: "POST", body: fields, csrfToken, fallback: "Unable to create ticket" });
}

export function updateTicket(csrfToken, key, fields) {
  return request(ticketPath(key), { method: "PATCH", body: fields, csrfToken, fallback: `Unable to update ${key}` });
}

// approveTicket hands the ticket to the factory; returns {ticket, job_id, created}.
export function approveTicket(csrfToken, key) {
  return request(ticketPath(key, "/approve"), { method: "POST", body: {}, csrfToken, fallback: `Unable to approve ${key}` });
}

export function commentOnTicket(csrfToken, key, body) {
  return request(ticketPath(key, "/comments"), { method: "POST", body: { body }, csrfToken, fallback: `Unable to comment on ${key}` });
}

export function deleteTicket(csrfToken, key) {
  return request(ticketPath(key), { method: "DELETE", csrfToken, fallback: `Unable to delete ${key}` });
}
