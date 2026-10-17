import assert from "node:assert/strict";
import test from "node:test";
import { act } from "react";
import { JSDOM } from "jsdom";
import { createServer } from "vite";

const baseJob = {
  repository: "example/repo",
  command: "codex",
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};

const initialJobs = [
  {
    ...baseJob,
    id: "job_running",
    state: "running",
    prompt: "MACH-60: running fixture",
    runs: [{ id: "run_running", state: "running", command: "codex" }],
  },
  {
    ...baseJob,
    id: "job_workflow_running",
    state: "running",
    prompt: "MACH-61: workflow fixture",
    workflow: { name: "deliver", steps: ["build"], current_step: 0 },
    runs: [{ id: "run_workflow", state: "running", command: "build" }],
  },
  {
    ...baseJob,
    id: "job_queued",
    state: "queued",
    prompt: "MACH-62: queued fixture",
    runs: [{ id: "run_queued", state: "queued", command: "codex" }],
  },
];

const moveButton = (title) =>
  [...document.querySelectorAll("button")].find((el) => el.getAttribute("aria-label") === `Move task ${title} back to the queue`);

test("move to queue is offered only for running plain jobs and asks before acting", async (context) => {
  await bootApp(context, [...initialJobs]);
  const confirmCalls = [];
  window.confirm = (message) => {
    confirmCalls.push(message);
    return false;
  };

  await eventually(() => assert.ok(moveButton("MACH-60: running fixture"), "running job card shows the move button"));
  assert.equal(moveButton("MACH-61: workflow fixture"), undefined, "workflow job card shows no move button");
  assert.equal(moveButton("MACH-62: queued fixture"), undefined, "queued job card shows no move button");

  // Declining the confirmation stops the request before it starts.
  moveButton("MACH-60: running fixture").click();
  await new Promise((resolve) => setTimeout(resolve, 50));
  assert.deepEqual(moveRequests, [], "declining the confirmation sends no request");
  assert.match(confirmCalls[0], /stops the current attempt/);
  assert.match(confirmCalls[0], /next attempt/);

  // The run page header and the transcript tab offer it too, for the running job only.
  window.location.hash = "#/runs/job_running";
  await eventually(() => assert.match(document.body.textContent, /This attempt is still in flight/), "transcript tab explains the move");
  assert.ok(moveButton("MACH-60: running fixture"), "run page header shows the move button");

  window.location.hash = "#/runs/job_workflow_running";
  await eventually(() => assert.match(document.body.textContent, /MACH-61: workflow fixture/));
  assert.equal(moveButton("MACH-61: workflow fixture"), undefined, "workflow run page shows no move button");
});

test("confirming moves the job: the card refreshes to attempt 2 with no duplicate, and the cancelled attempt records the saved work", async (context) => {
  let jobs = initialJobs.map((job) => ({ ...job }));
  await bootApp(context, () => jobs, (url, options) => {
    const jobID = decodeURIComponent(String(url).split("/api/v1/jobs/")[1].split("/move-to-queue")[0]);
    moveRequests.push({ url: String(url), csrf: options.headers?.["X-Machinist-CSRF"] });
    jobs = jobs.map((job) => job.id === jobID
      ? {
          ...job,
          state: "cancelled",
          superseded_by: "job_attempt2",
          updated_at: "2026-10-16T13:00:00Z",
          runs: [{ ...job.runs[0], state: "cancelled", exit_code: 130, error: "moved to queue", wip: "/srv/lanes/general-2/checkpoint" }],
        }
      : job);
    jobs = [...jobs, {
      ...baseJob,
      id: "job_attempt2",
      state: "queued",
      prompt: "MACH-60: running fixture",
      attempt: 2,
      previous_job_id: jobID,
      created_at: "2026-10-16T13:00:00Z",
      runs: [{ id: "run_attempt2", state: "queued", command: "codex" }],
    }];
    return { ok: true, json: async () => ({ job_id: "job_attempt2" }) };
  });
  window.confirm = () => true;

  moveButton("MACH-60: running fixture").click();
  await eventually(() => assert.equal(moveRequests.length, 1));
  assert.equal(moveRequests[0].url, "/api/v1/jobs/job_running/move-to-queue");
  assert.equal(moveRequests[0].csrf, "test-csrf");

  // The same card now shows the queued attempt N+1; the cancelled attempt is a prior, not a second card.
  await eventually(() => assert.match(document.body.textContent, /2 attempts/), "card shows attempt N+1");
  assert.equal(document.querySelectorAll('a[aria-label="Open run MACH-60: running fixture"]').length, 1, "no duplicate card appears");

  // The cancelled attempt shows the move reason and where the work was saved.
  window.location.hash = "#/runs/job_running";
  await eventually(() => assert.match(document.body.textContent, /moved to queue/), "cancelled attempt shows the move reason");
  await eventually(() => assert.match(document.body.textContent, /\/srv\/lanes\/general-2\/checkpoint/), "cancelled attempt shows the saved work location");
  assert.equal(moveButton("MACH-60: running fixture"), undefined, "terminal attempt shows no move button");
});

test("a failed move shows the server's readable message", async (context) => {
  await bootApp(context, [...initialJobs], () => ({
    ok: false,
    status: 409,
    json: async () => ({ error: "move to queue is only available for a running plain job" }),
  }));
  window.confirm = () => true;

  moveButton("MACH-60: running fixture").click();
  await eventually(() => assert.match(document.querySelector('[role="alert"]')?.textContent || "", /move to queue is only available for a running plain job/), "readable error is shown on the page");
  assert.ok(moveButton("MACH-60: running fixture"), "the card stays for another try");
});

const moveRequests = [];

async function bootApp(context, jobsOrLoader, onMove) {
  const dom = new JSDOM('<div id="root"></div>', { url: "http://localhost/#/runs" });
  const priorGlobals = new Map();
  for (const name of ["window", "document", "navigator", "localStorage", "Event", "MouseEvent"]) {
    priorGlobals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: dom.window[name] });
  }
  priorGlobals.set("fetch", Object.getOwnPropertyDescriptor(globalThis, "fetch"));
  globalThis.fetch = async (url, options) => {
    if (String(url).endsWith("/move-to-queue") && options?.method === "POST") {
      if (onMove) return onMove(String(url), options);
      return { ok: true, json: async () => ({ job_id: "job_moved" }) };
    }
    if (String(url).endsWith("/events")) return { ok: true, json: async () => ({ events: "", state: "succeeded" }) };
    const jobs = typeof jobsOrLoader === "function" ? jobsOrLoader() : jobsOrLoader;
    return { ok: true, json: async () => ({ jobs, workers: [], commands: [], repositories: [], triggers: [], csrf_token: "test-csrf" }) };
  };

  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  const mountedRoot = (await server.ssrLoadModule("/src/main.jsx")).appRoot;
  context.after(async () => {
    const previousActEnvironment = globalThis.IS_REACT_ACT_ENVIRONMENT;
    globalThis.IS_REACT_ACT_ENVIRONMENT = true;
    await act(async () => mountedRoot?.unmount());
    globalThis.IS_REACT_ACT_ENVIRONMENT = previousActEnvironment;
    await server.close();
    dom.window.close();
    moveRequests.length = 0;
    for (const [name, descriptor] of priorGlobals) {
      if (descriptor === undefined) delete globalThis[name];
      else Object.defineProperty(globalThis, name, descriptor);
    }
  });
  await eventually(() => assert.match(document.body.textContent, /running fixture/));
}

async function eventually(assertion, message) {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    try { assertion(); return; } catch (error) {
      if (attempt === 49) throw (message ? new Error(`${message}: ${error.message}`) : error);
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
}
