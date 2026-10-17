import assert from "node:assert/strict";
import test from "node:test";
import { act } from "react";
import { JSDOM } from "jsdom";
import { createServer } from "vite";

const jobs = [
  { id: "job_failed", state: "failed", prompt: "Failed fixture", repository: "example/repo", command: "codex", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-05T00:00:00Z", runs: [{ id: "run_failed", state: "failed", command: "codex", completed_at: "2026-01-01T06:00:00Z" }] },
  { id: "job_retried", state: "running", prompt: "MACH-99 Retried fixture", repository: "example/repo", command: "codex", created_at: "2026-01-04T10:00:00Z", updated_at: "2026-01-05T00:00:00Z", runs: [{ id: "run_second", state: "running", command: "codex" }] },
  { id: "job_retried_old", state: "failed", prompt: "MACH-99 Retried fixture", repository: "example/repo", command: "codex", created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-04T09:00:00Z", runs: [{ id: "run_first", state: "failed", command: "codex", completed_at: "2026-01-04T09:00:00Z" }] },
  { id: "job_succeeded", state: "succeeded", prompt: "Succeeded fixture", repository: "example/repo", command: "codex", created_at: "2026-01-01T00:00:00Z", runs: [{ id: "run_succeeded", state: "succeeded", command: "codex" }] },
];

test("runs default to board view and share filters when switching views", async (context) => {
  const dom = new JSDOM('<div id="root"></div>', { url: "http://localhost/#/runs" });
  const priorGlobals = new Map();
  for (const name of ["window", "document", "navigator", "localStorage", "Event", "MouseEvent"]) {
    priorGlobals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: dom.window[name] });
  }
  priorGlobals.set("fetch", Object.getOwnPropertyDescriptor(globalThis, "fetch"));
  let artifactRequests = 0;
  const detailJob = {
    id: "job_detail", state: "succeeded", repository: "example/repo", command: "build",
    task: {
      title: "Task with files",
      spec: "Build the feature: https://github.com/omacom/mobile/pull/179. <img src=x onerror=\"alert(1)\">",
    },
    created_at: "2026-01-01T00:00:00Z", updated_at: "2026-01-01T00:01:00Z",
    runs: [
      { id: "plan", command: "plan", state: "succeeded", outcome: "complete", summary: "Planned" },
      { id: "build", command: "build", state: "succeeded", outcome: "complete", summary: "Built", executor: "test-executor", model: "test-model", worker_name: "test-worker", duration_millis: 1000, exit_code: 0 },
    ],
  };
  const interruptedJob = { ...detailJob, id: "job_interrupted", state: "interrupted", workflow: { name: "build", steps: ["build"], current_step: 0 }, runs: [{ id: "interrupted", command: "build", state: "interrupted" }] };
  let cancelled = false;
  globalThis.fetch = async (url, options) => {
    if (url.endsWith("/cancel") && options?.method === "POST") {
      cancelled = true;
      interruptedJob.state = "cancelled";
      return { ok: true, json: async () => ({}) };
    }
    if (url.endsWith("/artifacts")) {
      artifactRequests += 1;
      return { ok: true, json: async () => [
        { id: "file_plan", run_id: "plan", path: "plan.md", size: 10, content_type: "text/plain" },
        { id: "file_build", run_id: "build", path: "result.md", size: 10, content_type: "text/plain" },
      ] };
    }
    if (url.endsWith("/content")) return { ok: true, text: async () => "<script>literal file text</script>" };
    return { ok: true, json: async () => ({ jobs: [...jobs, detailJob, interruptedJob], workers: [], commands: [], repositories: [], triggers: [], csrf_token: "test" }) };
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
  await eventually(() => assert.match(document.body.textContent, /Failed fixture/));

  assert.equal(button("Board").getAttribute("aria-pressed"), "true");
  assert.ok(document.querySelector('a[href="#/runs/job_failed"]'), "board links to task");
  const failedCardTime = document.querySelector('a[href="#/runs/job_failed"] time');
  assert.equal(failedCardTime?.getAttribute("datetime"), "2026-01-01T06:00:00Z", "board card prefers finish time over last update");
  assert.match(failedCardTime?.textContent ?? "", /ago|just now/);

  const retriedCard = [...document.querySelectorAll(".crafted-card")].find((card) => card.textContent.includes("Retried fixture"));
  assert.ok(retriedCard, "retried job renders a board card");
  assert.equal(retriedCard.querySelector('a[href="#/runs/job_retried_old"]'), null, "prior attempts are not listed inline");
  const pill = retriedCard.querySelector('[aria-label^="2 attempts"]');
  assert.ok(pill, "attempts pill renders in the card");
  assert.match(pill.getAttribute("title"), /Failed|Cancelled|Succeeded|Timed out/i, "prior attempt outcomes are in the tooltip");
  const singleCard = [...document.querySelectorAll(".crafted-card")].find((card) => card.textContent.includes("Failed fixture"));
  assert.ok(singleCard && !singleCard.querySelector('[aria-label*=" attempts"]'), "single-attempt cards show no attempts row");

  assert.equal(button("List").getAttribute("aria-pressed"), "false");

  button("Board").click();
  await eventually(() => assert.equal(button("Board").getAttribute("aria-pressed"), "true"));
  assert.match(document.body.textContent, /Waiting to start/);

  // Default membership is active + failed only; the Show succeeded tickbox is off.
  const showSucceeded = document.querySelector('input[type="checkbox"]');
  assert.ok(showSucceeded, "Show succeeded tickbox renders");
  assert.equal(showSucceeded.checked, false);
  assert.equal(window.location.hash, "#/runs");
  assert.match(document.body.textContent, /Failed fixture/);
  assert.doesNotMatch(document.body.textContent, /Succeeded fixture/);

  // Ticking it shows succeeded and resolved tickets and updates the URL.
  showSucceeded.click();
  await eventually(() => assert.match(document.body.textContent, /Succeeded fixture/));
  assert.equal(window.location.hash, "#/runs?succeeded=1");

  button("List").click();
  await eventually(() => assert.equal(button("List").getAttribute("aria-pressed"), "true"));
  assert.equal(window.location.hash, "#/runs?succeeded=1&view=list");
  assert.ok(document.querySelector('a[href="#/runs/job_failed"]'), "list links to task");
  assert.match(document.body.textContent, /Failed fixture/);
  assert.match(document.body.textContent, /Succeeded fixture/);

  // Browser back/forward restores filters from the URL hash.
  window.history.back();
  await eventually(() => assert.equal(button("List").getAttribute("aria-pressed"), "false"));
  assert.equal(window.location.hash, "#/runs?succeeded=1");
  assert.equal(document.querySelector('input[type="checkbox"]').checked, true);
  assert.match(document.body.textContent, /Succeeded fixture/);

  window.history.back();
  await eventually(() => assert.equal(window.location.hash, "#/runs"));
  await eventually(() => assert.doesNotMatch(document.body.textContent, /Succeeded fixture/));
  assert.equal(document.querySelector('input[type="checkbox"]').checked, false);

  showSucceeded.click();
  await eventually(() => assert.equal(window.location.hash, "#/runs?succeeded=1"));
  assert.match(document.body.textContent, /Failed fixture/);

  window.location.hash = "#/runs/job_detail";
  await eventually(() => assert.match(document.body.textContent, /Task with files/));
  await eventually(() => assert.ok(document.querySelector('[aria-label="View result.md"]')));
  assert.equal(artifactRequests, 1, "metadata fetched once across all panels and attempts");
  for (const label of ["Result", "Instructions", "Details"]) {
    assert.equal([...document.querySelectorAll('[role="tab"]')].find(el => el.textContent === label), undefined, `${label} should not be a tab`);
  }
  const instructions = document.querySelector('[aria-label="Run sections"] [aria-label="Instructions"]');
  const details = document.querySelector('[aria-label="Run details"]');
  const result = document.querySelector('[aria-label="Current result"]');
  assert.ok(instructions && result && details, "instructions, result, and details are visible together");
  for (const text of ["test-executor", "test-model", "test-worker", "Not reported", "Delete run", "Exit code"]) {
    assert.ok(details.textContent.includes(text), `details include ${text}`);
  }
  const instructionLink = instructions.querySelector('a[href="https://github.com/omacom/mobile/pull/179"]');
  assert.equal(instructionLink?.target, "_blank");
  assert.equal(instructionLink?.rel, "noopener noreferrer");
  assert.doesNotMatch(instructions.innerHTML, /<img /);
  assert.ok(instructions.textContent.includes('<img src=x onerror="alert(1)">'));
  const tab = name => [...document.querySelectorAll('[role="tab"]')].find(el => el.textContent === name);
  tab("History").click();
  await eventually(() => assert.equal(tab("History").getAttribute("aria-selected"), "true"));
  assert.match(document.querySelector('[role="tabpanel"]:not([hidden])').textContent, /plan.md/);
  tab("Files").click();
  await eventually(() => assert.equal(tab("Files").getAttribute("aria-selected"), "true"));
  document.querySelector('[role="tabpanel"]:not([hidden]) [aria-label="View result.md"]').click();
  await eventually(() => assert.match(document.querySelector('[role="tabpanel"]:not([hidden])').textContent, /<script>literal file text<\/script>/));
  assert.equal(document.querySelectorAll("script").length, 0, "preview renders text, not markup");
  assert.equal(artifactRequests, 1, "changing tabs does not refetch metadata");
  window.location.hash = "#/runs/job_interrupted";
  await eventually(() => assert.ok([...document.querySelectorAll("button")].find(el => el.textContent === "Cancel run")));
  button("Cancel run").click();
  await eventually(() => assert.equal(cancelled, true));
  await eventually(() => assert.equal(button("Delete run").disabled, false));
  await eventually(() => assert.equal([...document.querySelectorAll("button")].find(el => el.textContent === "Cancel run"), undefined));

});

function button(label) {
  const match = [...document.querySelectorAll("button")].find((element) => element.textContent.includes(label));
  assert.ok(match, `button ${label} should exist`);
  return match;
}

async function eventually(assertion) {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    try { assertion(); return; } catch (error) {
      if (attempt === 49) throw error;
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
}
