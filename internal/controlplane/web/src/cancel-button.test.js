import assert from "node:assert/strict";
import test from "node:test";
import { act } from "react";
import { JSDOM } from "jsdom";
import { createServer } from "vite";

const initialJobs = [
  {
    id: "job_plain_queued",
    state: "queued",
    prompt: "Plain queued fixture",
    repository: "example/repo",
    command: "codex",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    runs: [{ id: "run_plain", state: "queued", command: "codex" }],
  },
  {
    id: "job_workflow_queued",
    state: "queued",
    prompt: "Workflow queued fixture",
    repository: "example/repo",
    workflow: { name: "deliver", steps: ["build"], current_step: 0 },
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    runs: [{ id: "run_workflow", state: "queued", command: "build" }],
  },
  {
    id: "job_detail_cancel",
    state: "queued",
    prompt: "Detail cancel fixture",
    repository: "example/repo",
    command: "codex",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    runs: [{ id: "run_detail", state: "queued", command: "codex" }],
  },
];
let currentJobs = [...initialJobs];

test("cancel is offered for plain queued jobs and POSTs to the cancel endpoint", async (context) => {
  const dom = new JSDOM('<div id="root"></div>', { url: "http://localhost/#/runs" });
  const priorGlobals = new Map();
  for (const name of ["window", "document", "navigator", "localStorage", "Event", "MouseEvent"]) {
    priorGlobals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: dom.window[name] });
  }
  priorGlobals.set("fetch", Object.getOwnPropertyDescriptor(globalThis, "fetch"));
  const cancelRequests = [];
  globalThis.fetch = async (url, options) => {
    if (url.endsWith("/cancel") && options?.method === "POST") {
      cancelRequests.push({ url, csrf: options.headers?.["X-Machinist-CSRF"] });
      const jobID = decodeURIComponent(url.split("/api/v1/jobs/")[1].split("/")[0]);
      currentJobs = currentJobs.filter((job) => job.id !== jobID);
      return { ok: true, json: async () => ({ status: "accepted" }) };
    }
    return { ok: true, json: async () => ({ jobs: currentJobs, workers: [], commands: [], repositories: [], triggers: [], csrf_token: "test-csrf" }) };
  };

  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  let mountedRoot;
  context.after(async () => {
    const previousActEnvironment = globalThis.IS_REACT_ACT_ENVIRONMENT;
    globalThis.IS_REACT_ACT_ENVIRONMENT = true;
    await act(async () => mountedRoot?.unmount());
    globalThis.IS_REACT_ACT_ENVIRONMENT = previousActEnvironment;
    await server.close();
    dom.window.close();
    for (const [name, descriptor] of priorGlobals) {
      if (descriptor === undefined) delete globalThis[name];
      else Object.defineProperty(globalThis, name, descriptor);
    }
  });

  mountedRoot = (await server.ssrLoadModule("/src/main.jsx")).appRoot;
  await eventually(() => assert.match(document.body.textContent, /Plain queued fixture/));

  const plainCancel = [...document.querySelectorAll("button")].find((el) => el.getAttribute("aria-label") === "Cancel task Plain queued fixture");
  assert.ok(plainCancel, "plain queued job card shows a cancel button");
  assert.equal([...document.querySelectorAll("button")].find((el) => el.getAttribute("aria-label") === "Cancel task Workflow queued fixture"), undefined, "workflow job card shows no cancel button");

  plainCancel.click();
  await eventually(() => assert.equal(cancelRequests.length, 1));
  assert.equal(cancelRequests[0].url, "/api/v1/jobs/job_plain_queued/cancel");
  assert.equal(cancelRequests[0].csrf, "test-csrf");
  await eventually(() => assert.doesNotMatch(document.body.textContent, /Plain queued fixture/), "board refreshes and the cancelled job leaves the board");

  window.location.hash = "#/runs/job_workflow_queued";
  await eventually(() => assert.match(document.body.textContent, /Workflow queued fixture/));
  assert.equal([...document.querySelectorAll("button")].find((el) => el.getAttribute("aria-label") === "Cancel task Workflow queued fixture"), undefined, "workflow job detail shows no cancel button");

  window.location.hash = "#/runs/job_detail_cancel";
  await eventually(() => assert.match(document.body.textContent, /Detail cancel fixture/));
  const detailCancel = [...document.querySelectorAll("button")].find((el) => el.getAttribute("aria-label") === "Cancel task Detail cancel fixture");
  assert.ok(detailCancel, "plain queued job detail shows a cancel button");
  detailCancel.click();
  await eventually(() => assert.equal(cancelRequests.length, 2));
  assert.equal(cancelRequests[1].url, "/api/v1/jobs/job_detail_cancel/cancel");
});

async function eventually(assertion, message) {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    try { assertion(); return; } catch (error) {
      if (attempt === 49) throw (message ? new Error(`${message}: ${error.message}`) : error);
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
}
