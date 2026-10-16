// Workflow settings client (MACH-43). Reads and writes a project's job types,
// ordered stages, default type and Plane label mapping through the HTTP API.
// The UI, API and MCP are all views of the same database config.

export const stageKindOptions = [
  { value: "queued", label: "Queued" },
  { value: "planning", label: "Planning" },
  { value: "coding", label: "Coding" },
  { value: "review", label: "PR review" },
  { value: "uat", label: "UAT" },
  { value: "apply", label: "Apply" },
  { value: "verify", label: "Verify" },
  { value: "approve", label: "Approve" },
  { value: "finished", label: "Finished" },
];

export const sessionOptions = [
  { value: "continue", label: "Continue session" },
  { value: "fresh", label: "Fresh session" },
];

const text = (value) => (typeof value === "string" ? value : "");

export function stageFromPayload(raw) {
  const stage = raw && typeof raw === "object" ? raw : {};
  return {
    id: text(stage.id),
    name: text(stage.name),
    kind: text(stage.kind) || "coding",
    agent: text(stage.agent),
    model: text(stage.model),
    session: text(stage.session),
    humanApproval: Boolean(stage.human_approval),
    reviewProfile: text(stage.review_profile),
  };
}

export function jobTypeFromPayload(raw) {
  const jobType = raw && typeof raw === "object" ? raw : {};
  return {
    name: text(jobType.name),
    stages: Array.isArray(jobType.stages) ? jobType.stages.map(stageFromPayload) : [],
  };
}

export function workflowFromPayload(payload) {
  const wrapper = payload && typeof payload === "object" ? payload : {};
  const config = wrapper.config && typeof wrapper.config === "object" ? wrapper.config : {};
  const mappings = config.label_mappings && typeof config.label_mappings === "object" ? config.label_mappings : {};
  return {
    project: text(wrapper.project),
    isDefault: Boolean(wrapper.default),
    defaultType: text(config.default_type),
    labelMappings: Object.entries(mappings).map(([label, type]) => ({ label, type: text(type) })),
    jobTypes: Array.isArray(config.types) ? config.types.map(jobTypeFromPayload) : [],
  };
}

// workflowRequestBody converts the editor's camelCase state to the API's
// snake_case config body.
export function workflowRequestBody(config) {
  const labelMappings = {};
  for (const mapping of config.labelMappings || []) {
    const label = text(mapping.label).trim();
    const type = text(mapping.type).trim();
    if (label && type) labelMappings[label] = type;
  }
  return {
    default_type: text(config.defaultType),
    label_mappings: labelMappings,
    types: (config.jobTypes || []).map((jobType) => ({
      name: text(jobType.name),
      stages: (jobType.stages || []).map((stage) => ({
        id: text(stage.id),
        name: text(stage.name),
        kind: text(stage.kind),
        agent: text(stage.agent),
        model: text(stage.model),
        session: text(stage.session),
        human_approval: Boolean(stage.humanApproval),
        review_profile: text(stage.reviewProfile),
      })),
    })),
  };
}

export async function loadWorkflow(project, signal) {
  const query = project ? `?project=${encodeURIComponent(project)}` : "";
  const response = await fetch(`/api/v1/workflows${query}`, { signal });
  if (!response.ok) throw new Error(`Workflow request failed (${response.status})`);
  return workflowFromPayload(await response.json());
}

export async function saveWorkflow(project, config, csrfToken) {
  const response = await fetch(`/api/v1/workflows?project=${encodeURIComponent(project)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json", "X-Machinist-CSRF": csrfToken },
    body: JSON.stringify(workflowRequestBody(config)),
  });
  if (!response.ok) throw new Error(`Workflow save failed (${response.status})`);
  return workflowFromPayload(await response.json());
}

export async function putJobType(project, jobType, csrfToken) {
  const response = await fetch(`/api/v1/job-types?project=${encodeURIComponent(project)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json", "X-Machinist-CSRF": csrfToken },
    body: JSON.stringify({
      name: text(jobType.name),
      stages: (jobType.stages || []).map((stage) => ({
        id: text(stage.id),
        name: text(stage.name),
        kind: text(stage.kind),
        agent: text(stage.agent),
        model: text(stage.model),
        session: text(stage.session),
        human_approval: Boolean(stage.humanApproval),
        review_profile: text(stage.reviewProfile),
      })),
    }),
  });
  if (!response.ok) throw new Error(`Job type save failed (${response.status})`);
  return response.json();
}

export async function deleteJobType(project, name, csrfToken) {
  const response = await fetch(`/api/v1/job-types?project=${encodeURIComponent(project)}&name=${encodeURIComponent(name)}`, {
    method: "DELETE",
    headers: { "X-Machinist-CSRF": csrfToken },
  });
  if (!response.ok) throw new Error(`Job type delete failed (${response.status})`);
  return true;
}