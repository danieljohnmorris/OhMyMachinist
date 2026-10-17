import assert from "node:assert/strict";
import test from "node:test";
import React, { act } from "react";
import { createRoot } from "react-dom/client";
import { JSDOM } from "jsdom";
import { createServer } from "vite";
import { serializeProjectTOML } from "./projects-state.js";

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
  const setter = Object.getOwnPropertyDescriptor(dom.window.HTMLInputElement.prototype, "value").set;
  setter.call(input, value);
  // Dispatching a synthetic "input" event does not reach React here: react-dom
  // computes isInputEventSupported at import time, before the jsdom window
  // exists, so onChange falls back to an IE polyfill that never fires. Invoke
  // the React props handler directly instead.
  const propsKey = Object.keys(input).find((key) => key.startsWith("__reactProps$"));
  assert.ok(propsKey && typeof input[propsKey].onChange === "function", "input is React-controlled");
  input[propsKey].onChange({ target: input, currentTarget: input });
}

const PROJECT = "SPARKS";
const TRACKERS = {
  default: "builtin",
  projects: { [PROJECT]: { Name: PROJECT, Repository: "sparks-repo", Command: "build-pr", Model: "m1" } },
};
const SETTINGS = {
  [`tracker.projects.${PROJECT}`]: 'repository = "sparks-repo"\ncommand = "build-pr"\nmodel = "m1"\n',
};

test("projects page lists resolved projects and saves edits as settings", async (context) => {
  const { dom, priorGlobals } = setupDOM("#/projects");
  const writes = [];
  globalThis.fetch = async (url, options = {}) => {
    const target = String(url);
    if (options.method === "PUT" || options.method === "POST" || options.method === "DELETE") {
      writes.push({ url: target, method: options.method, body: options.body });
      return jsonResponse({});
    }
    if (target === "/api/v1/trackers") return jsonResponse(TRACKERS);
    if (target === "/api/v1/settings") return jsonResponse({ settings: SETTINGS });
    if (target === "/api/v1/skills") return jsonResponse({ skills: [] });
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

  const { ProjectsPage } = await server.ssrLoadModule("/src/projects.jsx");

  root = createRoot(document.getElementById("root"));
  await act(async () => root.render(React.createElement(ProjectsPage, { repositories: ["sparks-repo"], commands: ["build-pr", "task-to-pr"], csrfToken: "csrf" })));
  await eventually(() => assert.match(document.body.textContent, /SPARKS/));

  const table = document.querySelector("table");
  assert.match(table.textContent, /sparks-repo/);
  assert.match(table.textContent, /build-pr/);
  assert.match(table.textContent, /m1/);
  assert.match(table.textContent, /setting/);

  const editButton = [...table.querySelectorAll("button")].find((item) => item.getAttribute("aria-label") === `Edit project ${PROJECT}`);
  await act(async () => editButton.click());
  const form = [...document.querySelectorAll("h2")].find((heading) => heading.textContent === `Edit ${PROJECT}`).closest(".crafted-card");

  const modelInput = form.querySelector('input[aria-label="Model override"]');
  await act(async () => setInput(dom, modelInput, "m2"));

  const saveButton = [...form.querySelectorAll("button")].find((item) => item.textContent.trim() === "Save");
  await act(async () => saveButton.click());

  await eventually(() => assert.ok(writes.some((write) => write.url === `/api/v1/settings/tracker.projects.${PROJECT}`)));
  const save = writes.find((write) => write.url.startsWith("/api/v1/settings/tracker.projects."));
  assert.deepEqual(JSON.parse(save.body), {
    value: 'repository = "sparks-repo"\ncommand = "build-pr"\nmodel = "m2"',
  });
  await eventually(() => assert.match(document.body.textContent, /Saved\. Tickets filed under SPARKS/));
});

test("projects page adds a new project from an empty config", async (context) => {
  const { dom, priorGlobals } = setupDOM("#/projects");
  const writes = [];
  globalThis.fetch = async (url, options = {}) => {
    const target = String(url);
    if (options.method === "PUT" || options.method === "POST" || options.method === "DELETE") {
      writes.push({ url: target, method: options.method, body: options.body });
      return jsonResponse({});
    }
    if (target === "/api/v1/trackers") return jsonResponse({ default: "builtin", projects: {} });
    if (target === "/api/v1/settings") return jsonResponse({ settings: {} });
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

  const { ProjectsPage } = await server.ssrLoadModule("/src/projects.jsx");

  root = createRoot(document.getElementById("root"));
  await act(async () => root.render(React.createElement(ProjectsPage, { repositories: ["sparks-repo"], commands: ["build-pr"], csrfToken: "csrf" })));
  await eventually(() => assert.match(document.body.textContent, /No projects yet/));

  const newButton = [...document.querySelectorAll("button")].find((item) => item.textContent.trim() === "New project");
  await act(async () => newButton.click());
  await eventually(() => assert.match(document.body.textContent, /New project/));

  const form = [...document.querySelectorAll("h2")].find((heading) => heading.textContent === "New project").closest(".crafted-card");
  await act(async () => setInput(dom, form.querySelector('input[aria-label="Project name"]'), "spark"));
  await act(async () => setInput(dom, form.querySelector('input[aria-label="Repository"]'), "sparks-repo"));

  const saveButton = [...form.querySelectorAll("button")].find((item) => item.textContent.trim() === "Save");
  await act(async () => saveButton.click());

  await eventually(() => assert.ok(writes.some((write) => write.url === "/api/v1/settings/tracker.projects.SPARK")));
  const save = writes.find((write) => write.url.startsWith("/api/v1/settings/tracker.projects."));
  assert.deepEqual(JSON.parse(save.body), { value: 'repository = "sparks-repo"' });
});

test("serializeProjectTOML omits empty fields and quotes values", () => {
  assert.equal(serializeProjectTOML({ repository: "sparks repo", command: "", model: " " }), 'repository = "sparks repo"');
  assert.equal(serializeProjectTOML({ repository: "", command: "build-pr", model: "m1" }), 'command = "build-pr"\nmodel = "m1"');
  assert.equal(serializeProjectTOML({}), "");
});
