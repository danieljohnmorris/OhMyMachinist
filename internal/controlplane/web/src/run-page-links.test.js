import assert from "node:assert/strict";
import test from "node:test";
import { act } from "react";
import { JSDOM } from "jsdom";
import { createServer } from "vite";

const PR_URL = "https://github.com/danieljohnmorris/OhMyMachinist/pull/33";
const resolvedJob = {
  id: "job_resolved",
  state: "resolved",
  title: "MACH-31: Make the built-in review runnable",
  issue_key: "MACH-31",
  repository: "danieljohnmorris/OhMyMachinist",
  command: "task-to-pr",
  task: { title: "MACH-31: Make the built-in review runnable", source_url: "https://plane.danieljohnmorris.com/dan/browse/MACH-31/", spec: "Make the built-in review runnable." },
  resolution_kind: "pr",
  resolution_detail: "PR #33 (merged by danieljohnmorris)",
  metadata: { pr_url: PR_URL },
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-02T00:00:00Z",
  runs: [{ id: "run_resolved", command: "task-to-pr", state: "succeeded", outcome: "complete", summary: "Done, see " + PR_URL }],
};

function statusPayload() {
  return { jobs: [resolvedJob], workers: [], commands: [], repositories: [], triggers: [], csrf_token: "test" };
}

async function mount(context, { hash, fetch: fetchImpl }) {
  const dom = new JSDOM('<div id="root"></div>', { url: `http://localhost/${hash}` });
  const priorGlobals = new Map();
  for (const name of ["window", "document", "navigator", "localStorage", "Event", "MouseEvent"]) {
    priorGlobals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: dom.window[name] });
  }
  priorGlobals.set("fetch", Object.getOwnPropertyDescriptor(globalThis, "fetch"));
  globalThis.fetch = fetchImpl;

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
  return { dom, mountedRoot };
}

test("the run page links the resolved pull request and its merger", async (context) => {
  await mount(context, {
    hash: "#/runs/job_resolved",
    fetch: async () => ({ ok: true, json: async () => statusPayload() }),
  });
  await eventually(() => assert.match(document.body.textContent, /MACH-31/));

  const label = document.querySelector('[data-testid="resolution-label"]');
  assert.ok(label, "resolution label renders");
  assert.equal(label.textContent, "Resolved via PR #33 (merged by danieljohnmorris)");
  const links = [...label.querySelectorAll("a")];
  assert.deepEqual(links.map((anchor) => [anchor.getAttribute("href"), anchor.textContent]), [
    [PR_URL, "PR #33"],
    ["https://github.com/danieljohnmorris", "danieljohnmorris"],
  ]);
  for (const anchor of links) {
    assert.equal(anchor.target, "_blank");
    assert.equal(anchor.rel, "noopener noreferrer");
  }
});

test("board and list cards link the resolved pull request outside the task link", async (context) => {
  const { dom } = await mount(context, {
    hash: "#/runs",
    fetch: async () => ({ ok: true, json: async () => statusPayload() }),
  });
  await eventually(() => assert.match(document.body.textContent, /Tasks/));
  // Resolved cards are only listed once succeeded tasks are shown.
  const showSucceeded = [...document.querySelectorAll('input[type="checkbox"]')].find((input) => input.closest("label")?.textContent.includes("Show succeeded"));
  assert.ok(showSucceeded, "Show succeeded tickbox renders");
  showSucceeded.click();
  await eventually(() => assert.match(document.body.textContent, /MACH-31/));

  const assertCard = (card) => {
    assert.ok(card, "resolved card renders");
    const taskLink = card.querySelector('a[href="#/runs/job_resolved"]');
    const pullRequest = card.querySelector(`a[href="${PR_URL}"]`);
    const profile = card.querySelector('a[href="https://github.com/danieljohnmorris"]');
    assert.ok(taskLink && pullRequest && profile, "card links the task, the pull request and the merger");
    assert.equal(pullRequest.closest("a"), pullRequest, "pull request link is not nested in the task link");
    assert.equal(profile.closest("a"), profile, "profile link is not nested in the task link");
  };

  const boardCard = [...document.querySelectorAll(".crafted-card")].find((card) => card.textContent.includes("MACH-31"));
  assertCard(boardCard);

  const listButton = [...document.querySelectorAll("button")].find((button) => button.textContent.includes("List"));
  listButton.click();
  await eventually(() => assert.ok(document.querySelector('article a[href="#/runs/job_resolved"]')));
  assertCard(document.querySelector('article a[href="#/runs/job_resolved"]').closest("article"));
  assert.ok(dom.window.document.body.textContent.length > 0);
});

test("a failed status request shows a clear message instead of the raw fetch error", async (context) => {
  await mount(context, {
    hash: "#/runs/job_resolved",
    fetch: async () => { throw new TypeError("Failed to fetch"); },
  });

  await eventually(() => assert.match(document.body.textContent, /Cannot reach the control plane\. Retrying/));
  assert.doesNotMatch(document.body.textContent, /Failed to fetch/);
  const alert = document.querySelector('[role="alert"]');
  assert.ok(alert, "the failure is announced");
  assert.match(alert.textContent, /Cannot reach the control plane\. Retrying/);
});

test("a failed artifacts request shows a clear message instead of the raw fetch error", async (context) => {
  await mount(context, {
    hash: "#/runs/job_resolved",
    fetch: async (url) => {
      if (url.endsWith("/artifacts")) throw new TypeError("Failed to fetch");
      return { ok: true, json: async () => statusPayload() };
    },
  });
  await eventually(() => assert.match(document.body.textContent, /MACH-31/));
  await eventually(() => assert.match(document.body.textContent, /Could not load outputs/));
  assert.doesNotMatch(document.body.textContent, /Failed to fetch/);
});

async function eventually(assertion) {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    try { assertion(); return; } catch (error) {
      if (attempt === 49) throw error;
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
}