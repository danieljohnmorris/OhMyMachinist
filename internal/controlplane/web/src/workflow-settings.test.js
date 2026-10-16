import assert from "node:assert/strict";
import test from "node:test";
import { jobTypeFromPayload, stageFromPayload, workflowFromPayload, workflowRequestBody } from "./workflow-settings.js";

test("workflow payload parsing normalizes the API shape", () => {
  const parsed = workflowFromPayload({
    project: "sparkdash",
    default: true,
    config: {
      default_type: "web_pr",
      label_mappings: { "type:config": "config_ops" },
      types: [
        {
          name: "config_ops",
          stages: [
            { id: "queued", name: "Queued", kind: "queued" },
            { id: "apply", name: "Apply", kind: "apply", session: "fresh", human_approval: true, model: "glm" },
          ],
        },
      ],
    },
  });
  assert.equal(parsed.project, "sparkdash");
  assert.equal(parsed.isDefault, true);
  assert.equal(parsed.defaultType, "web_pr");
  assert.deepEqual(parsed.labelMappings, [{ label: "type:config", type: "config_ops" }]);
  assert.equal(parsed.jobTypes[0].name, "config_ops");
  assert.equal(parsed.jobTypes[0].stages[1].session, "fresh");
  assert.equal(parsed.jobTypes[0].stages[1].humanApproval, true);
});

test("workflow payload parsing falls back to the empty shape", () => {
  assert.deepEqual(workflowFromPayload(undefined), { project: "", isDefault: false, defaultType: "", labelMappings: [], jobTypes: [] });
  assert.deepEqual(workflowFromPayload({ config: 42 }), { project: "", isDefault: false, defaultType: "", labelMappings: [], jobTypes: [] });
  assert.equal(stageFromPayload(null).kind, "coding");
  assert.deepEqual(jobTypeFromPayload({ name: "x" }), { name: "x", stages: [] });
});

test("workflow request body round-trips camelCase to the API shape", () => {
  const body = workflowRequestBody({
    defaultType: "docs",
    labelMappings: [{ label: "type:docs", type: "docs" }, { label: "", type: "docs" }],
    jobTypes: [
      {
        name: "docs",
        stages: [
          { id: "queued", name: "Queued", kind: "queued", agent: "", model: "", session: "continue", humanApproval: false, reviewProfile: "" },
          { id: "write", name: "Write", kind: "coding", agent: "codex", model: "gpt", session: "fresh", humanApproval: true, reviewProfile: "docs" },
        ],
      },
    ],
  });
  assert.equal(body.default_type, "docs");
  // Blank mappings are dropped.
  assert.deepEqual(body.label_mappings, { "type:docs": "docs" });
  assert.equal(body.types[0].stages[1].human_approval, true);
  assert.equal(body.types[0].stages[1].review_profile, "docs");
  assert.equal(body.types[0].stages[0].agent, "");
});