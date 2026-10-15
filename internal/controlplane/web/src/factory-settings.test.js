import assert from "node:assert/strict";
import test from "node:test";
import {
  catalogFromPayload,
  factoryFromPayload,
  saveFactoryDefault,
  saveFactorySetting,
  settingToPayload,
} from "./factory-settings.js";

const EMPTY_SETTING = {
  tracker: "",
  codingAgent: { kind: "", executor: "", command: "" },
  reviewAgent: { kind: "", executor: "", command: "" },
  models: {
    coding: { model: "", effort: "", endpoint: "", url: "" },
    review: { model: "", effort: "", endpoint: "", url: "" },
    non_coding: { model: "", effort: "", endpoint: "", url: "" },
  },
};

test("factory payload parsing keeps the project, source and nested setting", () => {
  assert.deepEqual(
    factoryFromPayload({
      project: "omacom/mobile",
      source: "project",
      setting: {
        tracker: "linear",
        coding_agent: { kind: "omp", executor: "worker-a" },
        review_agent: { kind: "command", command: "codex exec --model {{model}}" },
        models: {
          coding: { model: "glm-5", effort: "high", endpoint: "cloud" },
          review: { model: "gpt-5", endpoint: "local", url: "http://127.0.0.1:8080" },
        },
      },
    }),
    {
      project: "omacom/mobile",
      source: "project",
      setting: {
        tracker: "linear",
        codingAgent: { kind: "omp", executor: "worker-a", command: "" },
        reviewAgent: { kind: "command", executor: "", command: "codex exec --model {{model}}" },
        models: {
          coding: { model: "glm-5", effort: "high", endpoint: "cloud", url: "" },
          review: { model: "gpt-5", effort: "", endpoint: "local", url: "http://127.0.0.1:8080" },
          non_coding: { model: "", effort: "", endpoint: "", url: "" },
        },
      },
    },
  );
});

test("factory payload parsing coerces missing and malformed fields", () => {
  assert.deepEqual(factoryFromPayload(undefined), { project: "", source: "", setting: EMPTY_SETTING });
  assert.deepEqual(factoryFromPayload({ project: 7, source: null, setting: [] }), { project: "", source: "", setting: EMPTY_SETTING });
  assert.deepEqual(
    factoryFromPayload({ setting: { tracker: 3, coding_agent: "omp", models: { coding: "glm-5" } } }),
    { project: "", source: "", setting: EMPTY_SETTING },
  );
});

test("catalog payload parsing keeps the supported adapters and roles", () => {
  assert.deepEqual(
    catalogFromPayload({ trackers: ["plane", "linear"], agents: ["omp", "codex"], roles: ["coding"], endpoints: ["local", "cloud"] }),
    { trackers: ["plane", "linear"], agents: ["omp", "codex"], roles: ["coding"], endpoints: ["local", "cloud"] },
  );
  assert.deepEqual(catalogFromPayload(undefined), { trackers: [], agents: [], roles: [], endpoints: [] });
  assert.deepEqual(catalogFromPayload({ trackers: "plane", agents: [7] }), { trackers: [], agents: [], roles: [], endpoints: [] });
});

test("settingToPayload emits snake_case and drops unset fields", () => {
  assert.deepEqual(
    settingToPayload({
      tracker: " plane ",
      codingAgent: { kind: "codex", executor: "", command: "" },
      reviewAgent: { kind: "", executor: "", command: "codex exec --model {{model}}" },
      models: {
        coding: { model: " glm-5 ", effort: "", endpoint: "", url: "" },
        review: { model: "", effort: "", endpoint: "", url: "" },
        non_coding: { model: "", effort: "low", endpoint: "cloud", url: "" },
      },
    }),
    {
      tracker: "plane",
      coding_agent: { kind: "codex" },
      review_agent: { command: "codex exec --model {{model}}" },
      models: { coding: { model: "glm-5" }, non_coding: { effort: "low", endpoint: "cloud" } },
    },
  );
});

test("settingToPayload trims an unset setup to an empty body", () => {
  assert.deepEqual(settingToPayload(EMPTY_SETTING), {});
  assert.deepEqual(settingToPayload(undefined), {});
});

test("saveFactorySetting PUTs the setting with the CSRF header and normalizes the response", async () => {
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options });
    return {
      ok: true,
      status: 200,
      json: async () => ({
        project: "omacom/mobile",
        source: "project",
        setting: { tracker: "linear", coding_agent: { kind: "omp", executor: "worker-a" } },
      }),
    };
  };
  try {
    const result = await saveFactorySetting(
      "omacom/mobile",
      { tracker: " linear ", codingAgent: { kind: "omp", executor: "worker-a" }, models: { coding: { model: "glm-5" } } },
      "csrf-token",
    );
    assert.equal(calls.length, 1);
    assert.equal(calls[0].url, "/api/v1/factory-setting");
    assert.equal(calls[0].options.method, "PUT");
    assert.equal(calls[0].options.headers["Content-Type"], "application/json");
    assert.equal(calls[0].options.headers["X-Machinist-CSRF"], "csrf-token");
    assert.deepEqual(JSON.parse(calls[0].options.body), {
      project: "omacom/mobile",
      setting: { tracker: "linear", coding_agent: { kind: "omp", executor: "worker-a" }, models: { coding: { model: "glm-5" } } },
    });
    assert.deepEqual(result, {
      project: "omacom/mobile",
      source: "project",
      setting: {
        ...EMPTY_SETTING,
        tracker: "linear",
        codingAgent: { kind: "omp", executor: "worker-a", command: "" },
      },
    });
  } finally {
    delete globalThis.fetch;
  }
});

test("factory helpers surface the server error message", async () => {
  globalThis.fetch = async () => ({
    ok: false,
    status: 400,
    json: async () => ({ error: "models.coding: endpoint local requires url" }),
  });
  try {
    await assert.rejects(saveFactoryDefault({ models: { coding: { model: "glm-5" } } }, "csrf-token"), {
      message: "models.coding: endpoint local requires url",
    });
  } finally {
    delete globalThis.fetch;
  }
});