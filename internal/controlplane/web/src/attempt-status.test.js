import assert from "node:assert/strict";
import test from "node:test";
import { act } from "react";
import { JSDOM } from "jsdom";
import { createServer } from "vite";

// A cancelled attempt that never ran: no start time, no worker, an actor and a
// reason. The status card and details must say "cancelled before start" rather
// than showing Unavailable/Unassigned.
const cancelled = {
  id: "job_cancelled",
  state: "cancelled",
  attempt: 1,
  title: "SPARK-2: Cancelled attempt",
  repository: "example/repo",
  command: "codex",
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-01T00:49:00Z",
  runs: [
    {
      id: "run_cancelled",
      command: "codex",
      state: "cancelled",
      exit_code: 130,
      cancel_actor: "dan",
      cancel_reason: "superseded by retry",
      completed_at: "2026-10-01T00:49:00Z",
    },
  ],
};

test("cancelled attempt shows actor, reason and cancelled before start", async (context) => {
  const dom = new JSDOM('<div id="root"></div>', { url: "http://localhost/#/runs/job_cancelled" });
  const priorGlobals = new Map();
  for (const name of ["window", "document", "navigator", "localStorage", "Event", "MouseEvent"]) {
    priorGlobals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: dom.window[name] });
  }
  priorGlobals.set("fetch", Object.getOwnPropertyDescriptor(globalThis, "fetch"));
  globalThis.fetch = async (url) => {
    if (url.endsWith("/artifacts")) return { ok: true, json: async () => [] };
    return { ok: true, json: async () => ({ jobs: [cancelled], workers: [], commands: [], repositories: [], triggers: [], csrf_token: "test" }) };
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
  await eventually(() => assert.match(document.body.textContent, /SPARK-2: Cancelled attempt/));

  const cancelDetail = document.querySelector('[data-testid="cancel-detail"]');
  assert.ok(cancelDetail, "cancelled attempt shows the cancel detail");
  assert.match(cancelDetail.textContent, /Cancelled by dan/);
  assert.match(cancelDetail.textContent, /superseded by retry/);

  const startedMetric = [...document.querySelectorAll("dt,dd")].find((node) => node.textContent === "Started");
  assert.ok(startedMetric, "details show a Started metric");
  assert.match(startedMetric.parentElement.textContent, /cancelled before start/);
  assert.doesNotMatch(startedMetric.parentElement.textContent, /Unavailable/);

  const workerMetric = [...document.querySelectorAll("dt,dd")].find((node) => node.textContent === "Worker");
  assert.ok(workerMetric, "details show a Worker metric");
  assert.match(workerMetric.parentElement.textContent, /cancelled before start/);
});

function eventually(assertion) {
  return (async () => {
    for (let attempt = 0; attempt < 50; attempt += 1) {
      try { assertion(); return; } catch (error) {
        if (attempt === 49) throw error;
        await new Promise((resolve) => setTimeout(resolve, 10));
      }
    }
  })();
}