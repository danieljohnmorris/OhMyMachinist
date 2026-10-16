import assert from "node:assert/strict";
import test from "node:test";
import { act } from "react";
import { JSDOM } from "jsdom";
import { createServer } from "vite";

const SPEC = [
  "## Approach",
  "",
  "Use **bold moves** and `inline_code`.",
  "",
  "- first bullet",
  "- second bullet",
  "",
  "See https://example.com/plan. Full docs at [docs](https://example.com/docs).",
].join("\n");

const job = {
  id: "job_markdown",
  state: "succeeded",
  repository: "example/repo",
  command: "build",
  task: { title: "Markdown fixture", spec: SPEC },
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:01:00Z",
  runs: [
    {
      id: "run_markdown",
      command: "build",
      state: "succeeded",
      outcome: "complete",
      summary: "Built it, see https://example.com/summary.",
    },
  ],
};

test("run page puts the sidebar right, renders markdown instructions, and styles links", async (context) => {
  const dom = new JSDOM('<div id="root"></div>', { url: "http://localhost/#/runs/job_markdown" });
  const priorGlobals = new Map();
  for (const name of ["window", "document", "navigator", "localStorage", "Event", "MouseEvent"]) {
    priorGlobals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: dom.window[name] });
  }
  priorGlobals.set("fetch", Object.getOwnPropertyDescriptor(globalThis, "fetch"));
  globalThis.fetch = async (url) => {
    if (url.endsWith("/artifacts")) return { ok: true, json: async () => [] };
    return { ok: true, json: async () => ({ jobs: [job], workers: [], commands: [], repositories: [], triggers: [], csrf_token: "test" }) };
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
  await eventually(() => assert.match(document.body.textContent, /Markdown fixture/));

  const sections = document.querySelector('[aria-label="Task sections"]');
  assert.ok(sections, "task sections grid exists");
  // Sidebar on the right: instructions fill the 1fr left column, the side
  // column is the fixed 380px right column, and nothing depends on order
  // utilities.
  assert.match(sections.className, /lg:grid-cols-\[minmax\(0,1fr\)_minmax\(0,380px\)\]/);
  const instructions = sections.querySelector('[aria-label="Instructions"]');
  const sideColumn = sections.querySelector('[aria-label="Current result"]').closest("div");
  assert.ok(instructions && sideColumn, "instructions and side column both render");
  assert.equal(
    instructions.compareDocumentPosition(sideColumn) & dom.window.Node.DOCUMENT_POSITION_FOLLOWING,
    dom.window.Node.DOCUMENT_POSITION_FOLLOWING,
    "side column comes after instructions in DOM order (right at lg, below on mobile)",
  );
  for (const child of sections.children) {
    assert.doesNotMatch(child.className, /lg:order-/);
  }

  // The tab row must not be its own vertical scroll container: `overflow-x:
  // auto` alone also makes the vertical axis scrollable, and the tags that
  // stick out of the flex line (underline, focus ring) then raise a stray
  // scrollbar. A padded wrapper scrolls sideways and clips nothing vertically.
  const tablist = sections.querySelector('[role="tablist"]');
  assert.ok(tablist, "tab row renders");
  assert.doesNotMatch(tablist.className, /overflow-[xy]-/, "tab row is not a scroll container");
  const tabScroller = tablist.parentElement;
  assert.match(tabScroller.className, /overflow-x-auto/, "tab row scrolls sideways");
  assert.match(tabScroller.className, /overflow-y-hidden/, "tab row cannot scroll vertically");

  // Instructions render markdown, not a pre-wrapped plain-text body.
  const body = instructions.querySelector(".markdown-body");
  assert.ok(body, "markdown body renders");
  assert.ok(body.querySelector("h2"), "markdown heading renders");
  assert.equal(body.querySelector("h2").textContent, "Approach");
  assert.equal(body.querySelectorAll("ul li").length, 2, "bullet list renders");
  assert.equal(body.querySelectorAll("li")[0].textContent, "first bullet");
  assert.ok(body.querySelector("code"), "inline code renders");
  assert.equal(body.querySelector("code").textContent, "inline_code");
  assert.equal(body.querySelector("strong")?.textContent, "bold moves");
  assert.equal(instructions.querySelector("pre"), null, "no pre wrapper around instructions body");

  // Every URL anchor looks like a link.
  const anchors = [...instructions.querySelectorAll("a"), ...document.querySelectorAll('[aria-label="Current result"] a')].filter((a) => a.href.startsWith("http"));
  const hrefs = anchors.map((a) => a.getAttribute("href"));
  assert.deepEqual(hrefs.sort(), ["https://example.com/docs", "https://example.com/plan", "https://example.com/summary"]);
  for (const anchor of anchors) {
    assert.match(anchor.className, /text-primary/);
    assert.match(anchor.className, /underline/);
    assert.equal(anchor.target, "_blank");
    assert.equal(anchor.rel, "noopener noreferrer");
  }
  const planAnchor = anchors.find((a) => a.getAttribute("href") === "https://example.com/plan");
  assert.equal(planAnchor.textContent, "https://example.com/plan", "trailing punctuation excluded from bare URL");
});

const skillsJob = {
  id: "job_skills",
  state: "succeeded",
  repository: "example/repo",
  command: "build",
  task: { title: "Skills fixture", spec: "Load skills." },
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:01:00Z",
  runs: [
    {
      id: "run_skills",
      command: "build",
      state: "succeeded",
      outcome: "complete",
      summary: "Done.",
      skills: {
        skills: [
          { name: "feature-dev:code-reviewer", version: "0123456789abcdef0123", scope: "repo", method: "native" },
        ],
        warnings: ['skill "missing-skill" is not installed; skipping'],
      },
    },
  ],
};

test("run page lists the skills a run loaded and its warnings", async (context) => {
  const dom = new JSDOM('<div id="root"></div>', { url: "http://localhost/#/runs/job_skills" });
  const priorGlobals = new Map();
  for (const name of ["window", "document", "navigator", "localStorage", "Event", "MouseEvent"]) {
    priorGlobals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: dom.window[name] });
  }
  priorGlobals.set("fetch", Object.getOwnPropertyDescriptor(globalThis, "fetch"));
  globalThis.fetch = async (url) => {
    if (String(url).endsWith("/artifacts")) return { ok: true, json: async () => [] };
    return { ok: true, json: async () => ({ jobs: [skillsJob], workers: [], commands: [], repositories: [], triggers: [], csrf_token: "test" }) };
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
  await eventually(() => assert.ok(document.querySelector('[aria-label="Loaded skills"]'), "skills card renders"));

  const card = document.querySelector('[aria-label="Loaded skills"]');
  assert.match(card.textContent, /feature-dev:code-reviewer/);
  assert.match(card.textContent, /0123456789ab/);
  assert.match(card.textContent, /repo/);
  assert.match(card.textContent, /native/);
  assert.match(card.textContent, /skill "missing-skill" is not installed/);
  assert.ok(card.querySelector('[aria-label="Skill warnings"]'), "warnings are called out");
});

async function eventually(assertion) {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    try { assertion(); return; } catch (error) {
      if (attempt === 49) throw error;
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
}
