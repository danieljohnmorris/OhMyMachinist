import assert from "node:assert/strict";
import test from "node:test";
import React, { act } from "react";
import { createRoot } from "react-dom/client";
import { JSDOM } from "jsdom";
import { createServer } from "vite";

const SKILLS = {
  skills: [
    {
      name: "feature-dev:code-reviewer",
      source: "https://example.com/skills/reviewer",
      active: "abc123",
      versions: [
        { version: "abc123", installed_at: "2026-02-01T00:00:00Z" },
        { version: "def456", installed_at: "2026-03-01T00:00:00Z" },
      ],
      used_by: ["acme-widgets", "shipped review profile"],
    },
  ],
};

test("skills page lists skills with references and runs actions against the API", async (context) => {
  const dom = new JSDOM('<div id="root"></div>', { url: "http://localhost/#/skills" });
  const priorGlobals = new Map();
  for (const name of ["window", "document", "navigator", "localStorage", "Event", "MouseEvent"]) {
    priorGlobals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: dom.window[name] });
  }
  priorGlobals.set("fetch", Object.getOwnPropertyDescriptor(globalThis, "fetch"));
  const writes = [];
  globalThis.fetch = async (url, options = {}) => {
    if (options.method && options.method !== "GET") writes.push({ url: String(url), method: options.method, body: options.body });
    if (String(url) === "/api/v1/skills") return { ok: true, json: async () => SKILLS };
    return { ok: true, json: async () => ({}) };
  };

  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  let root;
  const previousActEnvironment = globalThis.IS_REACT_ACT_ENVIRONMENT;
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  context.after(async () => {
    await act(async () => root?.unmount());
    globalThis.IS_REACT_ACT_ENVIRONMENT = previousActEnvironment;
    await server.close();
    dom.window.close();
    for (const [name, descriptor] of priorGlobals) {
      if (descriptor === undefined) delete globalThis[name];
      else Object.defineProperty(globalThis, name, descriptor);
    }
  });

  const { SkillsPage } = await server.ssrLoadModule("/src/skills.jsx");
  root = createRoot(document.getElementById("root"));
  await act(async () => root.render(React.createElement(SkillsPage, { csrfToken: "csrf" })));

  await eventually(() => assert.match(document.body.textContent, /feature-dev:code-reviewer/));
  assert.match(document.body.textContent, /abc123/, "active version is shown");
  assert.match(document.body.textContent, /acme-widgets/, "used by references are shown");

  const updateButton = [...document.querySelectorAll("button")].find((button) => button.textContent.trim() === "Update");
  assert.ok(updateButton, "Update action exists");
  await act(async () => updateButton.click());
  await eventually(() => assert.ok(writes.some((write) => write.url === "/api/v1/skills/feature-dev%3Acode-reviewer/update"), "update endpoint called"));

  const select = document.querySelector('select[aria-label="Version for feature-dev:code-reviewer"]');
  assert.ok(select, "version picker exists");
  assert.equal(select.options.length, 2, "every installed version is offered");
});

async function eventually(assertion) {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    try { assertion(); return; } catch (error) {
      if (attempt === 49) throw error;
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
}