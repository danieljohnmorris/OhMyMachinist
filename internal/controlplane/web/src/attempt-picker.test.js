import assert from "node:assert/strict";
import test from "node:test";
import { act } from "react";
import { JSDOM } from "jsdom";
import { createServer } from "vite";

const older = {
  id: "job_older",
  state: "failed",
  attempt: 1,
  title: "SPARK-1: Attempt picker",
  repository: "example/repo",
  command: "codex",
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-01T01:00:00Z",
  runs: [{ id: "run_older", command: "codex", state: "failed", outcome: "failed", started_at: "2026-10-01T00:10:00Z", completed_at: "2026-10-01T00:30:00Z" }],
};
const newer = {
  id: "job_newer",
  state: "running",
  attempt: 2,
  previous_job_id: "job_older",
  title: "SPARK-1: Attempt picker",
  repository: "example/repo",
  command: "codex",
  created_at: "2026-10-02T00:00:00Z",
  updated_at: "2026-10-02T01:00:00Z",
  runs: [{ id: "run_newer", command: "codex", state: "running", outcome: "", started_at: "2026-10-02T00:10:00Z" }],
};

test("attempt picker smoke", async (context) => {
  const dom = new JSDOM('<div id="root"></div>', { url: "http://localhost/#/runs/job_older?tab=activity" });
  const priorGlobals = new Map();
  for (const name of ["window", "document", "navigator", "localStorage", "Event", "MouseEvent"]) {
    priorGlobals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: dom.window[name] });
  }
  priorGlobals.set("fetch", Object.getOwnPropertyDescriptor(globalThis, "fetch"));
  globalThis.fetch = async () => ({ ok: true, json: async () => ({ jobs: [older, newer], workers: [], commands: [], repositories: [], triggers: [], csrf_token: "test" }) });

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
  await eventually(() => assert.match(document.body.textContent, /Attempt picker/));

  const picker = document.querySelector('#attempt-picker');
  assert.ok(picker, "attempt picker renders");
  // The task URL identifies its latest attempt: opening an older attempt
  // lands on the latest, and the picker defaults to it. The tab param stays.
  await eventually(() => assert.equal(document.querySelector('#attempt-picker').value, "job_newer"));
  assert.match(dom.window.location.hash, /#\/runs\/job_newer/);
  assert.match(dom.window.location.hash, /tab=activity/);
  const labels = [...picker.options].map((option) => option.textContent);
  assert.equal(labels.length, 2);
  assert.match(labels[0], /Attempt 1/);
  assert.match(labels[0], /failed/);
  assert.match(labels[0], /20m/, "older attempt shows its duration");
  assert.match(labels[1], /Attempt 2/);
  assert.match(labels[1], /running/);
  assert.match(labels[1], /elapsed|\.| /);

  // Header shows the latest attempt; switch to the older attempt explicitly.
  const header = document.querySelector("header");
  assert.match(header.textContent, /running/);
  picker.value = "job_older";
  picker.dispatchEvent(new dom.window.Event("change", { bubbles: true }));
  await eventually(() => assert.match(dom.window.location.hash, /attempt=job_older/));
  assert.match(dom.window.location.hash, /tab=activity/, "tab param kept");
  await eventually(() => assert.match(document.querySelector("header").textContent, /failed/), "header status follows the attempt");
  assert.equal(document.querySelector('#attempt-picker').value, "job_older");

  // Switching back to the latest attempt drops the attempt param.
  const pickerAgain = document.querySelector('#attempt-picker');
  pickerAgain.value = "job_newer";
  pickerAgain.dispatchEvent(new dom.window.Event("change", { bubbles: true }));
  await eventually(() => assert.doesNotMatch(dom.window.location.hash, /attempt=/));
  await eventually(() => assert.match(document.querySelector("header").textContent, /running/));
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
