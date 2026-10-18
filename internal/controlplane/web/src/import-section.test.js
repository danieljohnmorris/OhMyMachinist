import assert from "node:assert/strict";
import test from "node:test";
import React, { act } from "react";
import { createRoot } from "react-dom/client";
import { JSDOM } from "jsdom";
import { createServer } from "vite";

function jsonResponse(body) {
  return { ok: true, json: async () => body };
}

function setupDOM(hash) {
  const dom = new JSDOM('<div id="root"></div>', { url: `http://localhost/${hash}` });
  const priorGlobals = new Map();
  for (const name of ["window", "document", "navigator", "localStorage", "Event", "MouseEvent"]) {
    priorGlobals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: dom.window[name] });
  }
  priorGlobals.set("fetch", Object.getOwnPropertyDescriptor(globalThis, "fetch"));
  return { dom, priorGlobals };
}

async function eventually(assertion) {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    try { assertion(); return; } catch (error) {
      if (attempt === 49) throw error;
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
}

function setInput(dom, input, value) {
  const setter = Object.getOwnPropertyDescriptor(dom.window.HTMLSelectElement.prototype, "value").set;
  setter.call(input, value);
  const propsKey = Object.keys(input).find((key) => key.startsWith("__reactProps$"));
  assert.ok(propsKey && typeof input[propsKey].onChange === "function", "select is React-controlled");
  input[propsKey].onChange({ target: input, currentTarget: input });
}

const ROSTER = { source: "plane", projects: [
  { id: "proj", name: "Platform", identifier: "MACH", repository: "platform" },
  { id: "other", name: "Other", repository: "" },
] };
const PLAN = { source: "plane", project: "Platform", tickets: [
  { key: "MACH-5", title: "MACH-5: Add archive page", state: "in_progress", action: "create" },
  { key: "MACH-6", title: "MACH-6: Shipped work", state: "", action: "skip", reason: "completed" },
] };
const TRACKERS = {
  default: "builtin",
  trackers: [{ id: "builtin", name: "Built-in", configured: true }, { id: "plane", name: "Plane", configured: true }],
  projects: {},
};
const TICKETS = { tickets: [] };

test("tickets page previews and imports a Plane project", async (context) => {
  const { dom, priorGlobals } = setupDOM("#/tickets");
  const posts = [];
  globalThis.fetch = async (url, options = {}) => {
    const target = String(url);
    if (options.method === "POST" && target === "/api/v1/import") {
      posts.push(JSON.parse(options.body));
      return jsonResponse({ source: "plane", project: "Platform", created: 1, skipped: 1, tickets: PLAN.tickets });
    }
    if (target === "/api/v1/tickets") return jsonResponse(TICKETS);
    if (target === "/api/v1/trackers") return jsonResponse(TRACKERS);
    if (target.startsWith("/api/v1/import/preview") && !target.includes("project=")) return jsonResponse(ROSTER);
    if (target.startsWith("/api/v1/import/preview") && target.includes("project=Platform")) return jsonResponse(PLAN);
    return jsonResponse({});
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

  const { TicketsPage } = await server.ssrLoadModule("/src/tickets.jsx");

  root = createRoot(document.getElementById("root"));
  await act(async () => root.render(React.createElement(TicketsPage, { csrfToken: "csrf", repositories: [], jobs: [], onChanged: async () => {} })));
  await eventually(() => assert.match(document.body.textContent, /Import from Plane/));

  // The import card is scoped separately: the page header also carries selects
  // whose options share values with the import selectors.
  const importCard = () => [...document.querySelectorAll("h2")].find((heading) => heading.textContent.startsWith("Import from")).closest(".crafted-card");
  const showButton = [...importCard().querySelectorAll("button")].find((item) => item.textContent.includes("Import issues"));
  await act(async () => showButton.click());
  await eventually(() => assert.match(importCard().textContent, /Choose a source/));

  const sourceSelect = [...importCard().querySelectorAll("select")].find((item) => [...item.options].some((option) => option.value === "plane"));
  await act(async () => setInput(dom, sourceSelect, "plane"));
  await eventually(() => assert.match(importCard().textContent, /Choose a project/));

  const projectSelect = [...importCard().querySelectorAll("select")].find((item) => [...item.options].some((option) => option.value === "Platform"));
  await act(async () => setInput(dom, projectSelect, "Platform"));
  await eventually(() => assert.match(importCard().textContent, /MACH-5/));
  const table = importCard().querySelector("table");
  assert.match(table.textContent, /In progress/);
  assert.match(table.textContent, /Completed/);

  const importButton = [...importCard().querySelectorAll("button")].find((item) => item.textContent.trim() === "Import");
  await act(async () => importButton.click());
  await eventually(() => assert.match(importCard().textContent, /Created 1, skipped 1\./));
  assert.deepEqual(posts, [{ source: "plane", project: "Platform" }]);
});
