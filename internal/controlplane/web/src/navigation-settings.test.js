import assert from "node:assert/strict";
import test from "node:test";
import { act } from "react";
import { JSDOM } from "jsdom";
import { createServer } from "vite";

const FACTORY = {
  state: "running",
  max_concurrent_jobs: 2,
  ready_lanes: 1,
  lanes: [{ name: "general-1", instance_id: "inst-1", enabled: true, state: "idle", busy: false, repositories: ["acme/widgets"] }],
};

const DEFINITIONS = {
  commands: [{ name: "triage", executor: "claude-code", timeout: "1h" }],
  workflows: { deliver: [{ name: "triage" }] },
};

const SKILLS = { skills: [] };

// The primary nav keeps the operational surfaces top-level and folds the
// configuration surfaces (Factory, Workflows, Skills) into Settings.
const PRIMARY_NAV = ["Runs", "Tickets", "Projects", "Analytics", "Workers", "Triggers", "Settings"];

test("sidebar keeps configuration surfaces inside Settings and old hashes land on their section", async (context) => {
  const dom = new JSDOM('<div id="root"></div>', { url: "http://localhost/#/settings" });
  const priorGlobals = new Map();
  for (const name of ["window", "document", "navigator", "localStorage", "Event", "MouseEvent"]) {
    priorGlobals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: dom.window[name] });
  }
  priorGlobals.set("fetch", Object.getOwnPropertyDescriptor(globalThis, "fetch"));
  globalThis.fetch = async (url) => {
    const target = String(url).split("?")[0];
    if (target === "/api/v1/status") return { ok: true, json: async () => ({ jobs: [], workers: [], commands: ["triage"], repositories: ["acme/widgets"], triggers: [], factory: FACTORY, csrf_token: "test-csrf" }) };
    if (target === "/api/v1/definitions") return { ok: true, json: async () => DEFINITIONS };
    if (target === "/api/v1/skills") return { ok: true, json: async () => SKILLS };
    return { ok: true, json: async () => ({}) };
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
    for (const [name, descriptor] of priorGlobals) {
      if (descriptor === undefined) delete globalThis[name];
      else Object.defineProperty(globalThis, name, descriptor);
    }
  });

  await eventually(() => assert.match(document.body.textContent, /Settings/));

  // The primary nav lists exactly the operational surfaces plus Settings.
  const nav = document.querySelector('nav[aria-label="Primary"]');
  assert.ok(nav, "primary nav exists");
  const labels = [...nav.querySelectorAll("a")].map((link) => link.querySelector("span")?.textContent);
  assert.deepEqual(labels, PRIMARY_NAV);
  for (const route of ["#/factory", "#/workflows", "#/skills"]) {
    assert.ok(!nav.querySelector(`a[href="${route}"]`), `${route} is not a top-level entry`);
  }

  // The Settings page hosts the configuration sections as tabs.
  const tabs = [...document.querySelectorAll('[role="tablist"][aria-label="Settings sections"] [role="tab"]')].map((tab) => tab.textContent);
  assert.deepEqual(tabs, ["General", "Factory", "Workflows", "Skills"]);

  // Every old deep link still lands on the right view, inside Settings.
  const activeTab = () => document.querySelector('[role="tab"][aria-selected="true"]')?.textContent;
  const visit = async (hash, assertSection) => {
    window.location.hash = hash;
    await eventually(() => assert.ok(document.querySelector('a[href="#/settings"]')?.getAttribute("aria-current") === "page"), `${hash} marks Settings current`);
    await assertSection();
  };

  await visit("#/factory", async () => {
    // The Factory section keeps the concurrency and lane controls working.
    await eventually(() => assert.ok(document.querySelector('[aria-label="Jobs at once"]'), "concurrency stepper is present"));
    await eventually(() => assert.match(document.body.textContent, /Worker lanes/));
    await eventually(() => assert.match(document.body.textContent, /general-1/));
  });

  await visit("#/workflows", async () => {
    await eventually(() => assert.equal(activeTab(), "Workflows"), "Workflows tab is active");
    await eventually(() => assert.match(document.body.textContent, /1 step/), "workflow definition is shown");
  });

  // The legacy #/commands alias rides the same Workflows section.
  await visit("#/commands", async () => {
    await eventually(() => assert.equal(activeTab(), "Workflows"), "commands alias lands on the Workflows section");
  });

  await visit("#/skills", async () => {
    await eventually(() => assert.equal(activeTab(), "Skills"), "Skills tab is active");
    await eventually(() => assert.match(document.body.textContent, /Install from a URL or path/));
  });

  await visit("#/settings", async () => {
    await eventually(() => assert.equal(activeTab(), "General"), "General tab is active");
    await eventually(() => assert.match(document.body.textContent, /Preview resolved profile/), "General section shows the review settings");
  });
});

async function eventually(assertion, message) {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    try { assertion(); return; } catch (error) {
      if (attempt === 49) throw (message ? new Error(`${message}: ${error.message}`) : error);
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
}
