import assert from "node:assert/strict";
import test from "node:test";
import { act } from "react";
import { JSDOM } from "jsdom";
import { createServer } from "vite";

const b64 = (text) => Buffer.from(text, "utf8").toString("base64");
const line = (event) => JSON.stringify(event);

const events = [
  line({ type: "run.started", message: "command=codex repository=lanes/general-3 attempt=1" }),
  line({ type: "process.output", stream: "stdout", encoding: "base64", data: b64("first attempt output\n") }),
  line({ type: "run.started", message: "command=codex repository=lanes/general-3 attempt=2" }),
  line({ type: "process.output", stream: "stdout", encoding: "base64", data: b64("second attempt output\n") }),
].join("\n");

const relaunchJob = {
  id: "job_relaunch",
  state: "running",
  prompt: "Relaunch fixture",
  repository: "example/repo",
  command: "codex",
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-01T00:06:00Z",
  state_history: [
    { state: "queued", changed_at: "2026-10-01T00:00:00Z" },
    { state: "running", changed_at: "2026-10-01T00:01:00Z" },
  ],
  runs: [{
    id: "run_relaunch",
    state: "running",
    command: "codex",
    started_at: "2026-10-01T00:01:00Z",
    attempts: [
      { number: 1, worker_name: "lane-a", model: "gpt-5", started_at: "2026-10-01T00:01:00Z", ended_at: "2026-10-01T00:05:00Z", end_reason: "lease expired" },
      { number: 2, worker_name: "lane-a", model: "gpt-5", started_at: "2026-10-01T00:06:00Z" },
    ],
  }],
};

test("run page separates relaunch attempts and shows the previous end reason", async (context) => {
  const dom = new JSDOM('<div id="root"></div>', { url: "http://localhost/#/runs/job_relaunch" });
  const priorGlobals = new Map();
  for (const name of ["window", "document", "navigator", "localStorage", "Event", "MouseEvent"]) {
    priorGlobals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: dom.window[name] });
  }
  priorGlobals.set("fetch", Object.getOwnPropertyDescriptor(globalThis, "fetch"));
  globalThis.fetch = async (url) => {
    if (url.endsWith("/events")) return { ok: true, json: async () => ({ state: "running", events }) };
    return { ok: true, json: async () => ({ jobs: [relaunchJob], workers: [], commands: [], repositories: [], triggers: [], csrf_token: "test" }) };
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
  await eventually(() => assert.ok(document.querySelector('[aria-label="Attempt 2 transcript"]'), "attempt 2 header renders"));

  const attempt1 = document.querySelector('[aria-label="Attempt 1 transcript"]');
  const attempt2 = document.querySelector('[aria-label="Attempt 2 transcript"]');
  assert.match(attempt1.textContent, /Attempt 1/);
  assert.match(attempt1.textContent, /lease expired/);
  assert.match(attempt2.textContent, /Attempt 2/);

  // Latest attempt is expanded, the earlier one collapsed.
  assert.equal(attempt2.getAttribute("aria-expanded"), "true");
  assert.equal(attempt1.getAttribute("aria-expanded"), "false");
  assert.match(document.body.textContent, /second attempt output/);
  assert.doesNotMatch(document.body.textContent, /first attempt output/);

  // Details panel shows the attempt count and the previous end reason.
  assert.match(document.body.textContent, /Attempt 2 of 2/);
  assert.match(document.body.textContent, /previous: lease expired/);
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