// Factory setup (MACH-37): the pluggable per-project tracker, coding agent,
// review agent and per-role models. Resolved per project from the project
// setting, the global default, the control-plane config, or the built-in empty
// setup, and edited in Settings. An empty field means "unset": the run keeps
// whatever the trigger or command configured.

const MODEL_ROLES = ["coding", "review", "non_coding"];

const text = (value) => (typeof value === "string" ? value : "");

const listOfText = (value) => (Array.isArray(value) ? value.filter((item) => typeof item === "string") : []);

function agentFromPayload(payload) {
  const source = payload && typeof payload === "object" ? payload : {};
  return { kind: text(source.kind), executor: text(source.executor), command: text(source.command) };
}

function modelFromPayload(payload) {
  const source = payload && typeof payload === "object" ? payload : {};
  return {
    model: text(source.model),
    effort: text(source.effort),
    endpoint: text(source.endpoint),
    url: text(source.url),
  };
}

function settingFromPayload(payload) {
  const source = payload && typeof payload === "object" ? payload : {};
  const models = source.models && typeof source.models === "object" ? source.models : {};
  const roles = {};
  for (const role of MODEL_ROLES) roles[role] = modelFromPayload(models[role]);
  return {
    tracker: text(source.tracker),
    codingAgent: agentFromPayload(source.coding_agent),
    reviewAgent: agentFromPayload(source.review_agent),
    models: roles,
  };
}

function agentToPayload(agent) {
  const source = agent && typeof agent === "object" ? agent : {};
  const payload = {};
  for (const field of ["kind", "executor", "command"]) {
    const value = text(source[field]).trim();
    if (value) payload[field] = value;
  }
  return payload;
}

function modelToPayload(model) {
  const source = model && typeof model === "object" ? model : {};
  const payload = {};
  for (const field of ["model", "effort", "endpoint", "url"]) {
    const value = text(source[field]).trim();
    if (value) payload[field] = value;
  }
  return payload;
}

function modelsToPayload(models) {
  const source = models && typeof models === "object" ? models : {};
  const payload = {};
  for (const role of MODEL_ROLES) {
    const model = modelToPayload(source[role]);
    if (Object.keys(model).length) payload[role] = model;
  }
  return payload;
}

// factoryFromPayload normalizes a GET/PUT/DELETE factory-setting response.
export function factoryFromPayload(payload) {
  const source = payload && typeof payload === "object" ? payload : {};
  return { project: text(source.project), source: text(source.source), setting: settingFromPayload(source.setting) };
}

// catalogFromPayload normalizes the adapters and roles the editor may offer.
export function catalogFromPayload(payload) {
  const source = payload && typeof payload === "object" ? payload : {};
  return {
    trackers: listOfText(source.trackers),
    agents: listOfText(source.agents),
    roles: listOfText(source.roles),
    endpoints: listOfText(source.endpoints),
  };
}

// settingToPayload renders a setting as the snake_case body the API expects,
// dropping unset fields so the stored setup stays canonical.
export function settingToPayload(setting) {
  const source = setting && typeof setting === "object" ? setting : {};
  const payload = {};
  const tracker = text(source.tracker).trim();
  if (tracker) payload.tracker = tracker;
  const codingAgent = agentToPayload(source.codingAgent);
  if (Object.keys(codingAgent).length) payload.coding_agent = codingAgent;
  const reviewAgent = agentToPayload(source.reviewAgent);
  if (Object.keys(reviewAgent).length) payload.review_agent = reviewAgent;
  const models = modelsToPayload(source.models);
  if (Object.keys(models).length) payload.models = models;
  return payload;
}

// readFactoryPayload parses a response, surfacing the server's error message.
async function readFactoryPayload(response, fallback) {
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    const message = typeof payload.error === "string" ? payload.error.trim() : "";
    throw new Error(message || `${fallback} (${response.status})`);
  }
  return payload;
}

export async function loadFactoryCatalog(signal) {
  const response = await fetch("/api/v1/factory-catalog", { signal });
  return catalogFromPayload(await readFactoryPayload(response, "Factory catalog request failed"));
}

export async function loadFactorySetting(project, signal) {
  const query = project ? `?project=${encodeURIComponent(project)}` : "";
  const response = await fetch(`/api/v1/factory-setting${query}`, { signal });
  return factoryFromPayload(await readFactoryPayload(response, "Factory setting request failed"));
}

export async function saveFactorySetting(project, setting, csrfToken) {
  const response = await fetch("/api/v1/factory-setting", {
    method: "PUT",
    headers: { "Content-Type": "application/json", "X-Machinist-CSRF": csrfToken },
    body: JSON.stringify({ project, setting: settingToPayload(setting) }),
  });
  return factoryFromPayload(await readFactoryPayload(response, "Factory setting save failed"));
}

export async function resetFactorySetting(project, csrfToken) {
  const response = await fetch(`/api/v1/factory-setting?project=${encodeURIComponent(project)}`, {
    method: "DELETE",
    headers: { "X-Machinist-CSRF": csrfToken },
  });
  return factoryFromPayload(await readFactoryPayload(response, "Factory setting revert failed"));
}

export async function loadFactoryDefault(signal) {
  const response = await fetch("/api/v1/factory-default", { signal });
  return factoryFromPayload(await readFactoryPayload(response, "Factory default request failed"));
}

export async function saveFactoryDefault(setting, csrfToken) {
  const response = await fetch("/api/v1/factory-default", {
    method: "PUT",
    headers: { "Content-Type": "application/json", "X-Machinist-CSRF": csrfToken },
    body: JSON.stringify({ setting: settingToPayload(setting) }),
  });
  return factoryFromPayload(await readFactoryPayload(response, "Factory default save failed"));
}