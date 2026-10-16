import assert from "node:assert/strict";
import test from "node:test";
import React, { act } from "react";
import { createRoot } from "react-dom/client";
import { JSDOM } from "jsdom";
import { createServer } from "vite";

const PROJECT = "acme/widgets";

function jsonResponse(body) {
  return { ok: true, json: async () => body };
}

test("project skills editor loads the saved list, previews versions and saves", async (context) => {
  const dom = new JSDOM('<div id="root"></div>', { url: "http://localhost/#/settings" });
  const priorGlobals = new Map();
  for (const name of ["window", "document", "navigator", "localStorage", "Event", "MouseEvent"]) {
    priorGlobals.set(name, Object.getOwnPropertyDescriptor(globalThis, name));
    Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: dom.window[name] });
  }
  priorGlobals.set("fetch", Object.getOwnPropertyDescriptor(globalThis, "fetch"));
  const writes = [];
  globalThis.fetch = async (url, options = {}) => {
    const target = String(url);
    if (options.method === "PUT" || options.method === "POST" || options.method === "DELETE") {
      writes.push({ url: target, method: options.method, body: options.body });
      return jsonResponse({});
    }
    if (target.startsWith("/api/v1/review-profiles")) return jsonResponse({ default: "default", profiles: [] });
    if (target.startsWith("/api/v1/review-profile")) return jsonResponse({ project: PROJECT, source: "default" });
    if (target === "/api/v1/tokens") return jsonResponse({ tokens: [] });
    if (target === "/api/v1/settings") {
      return jsonResponse({ settings: { [`skills.project.${PROJECT}`]: JSON.stringify({ skills: [{ name: "rust-review", applies_to: ["review"], required: true }] }) } });
    }
    if (target === "/api/v1/skills") {
      return jsonResponse({ skills: [{ name: "rust-review", source: "https://example.com/rust-review", active: "def456", versions: [{ version: "def456", installed_at: "2026-02-01T00:00:00Z" }], used_by: [] }] });
    }
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

  const { SettingsPage } = await server.ssrLoadModule("/src/settings.jsx");

  root = createRoot(document.getElementById("root"));
  await act(async () => root.render(React.createElement(SettingsPage, { repositories: [PROJECT], csrfToken: "csrf" })));
  await eventually(() => assert.match(document.body.textContent, /Project skills/));

  const card = [...document.querySelectorAll("h2")].find((heading) => heading.textContent === "Project skills").closest(".crafted-card");
  await eventually(() => assert.equal(card.querySelector('input[aria-label="Skill 1 name"]').value, "rust-review"));

  const clickButton = async (label) => {
    const button = [...card.querySelectorAll("button")].find((item) => item.textContent.trim() === label);
    assert.ok(button, `button ${label} exists`);
    await act(async () => button.click());
  };

  await clickButton("Resolve now");
  await eventually(() => assert.match(card.textContent, /def456/));

  await clickButton("Save");
  await eventually(() => assert.ok(writes.some((write) => write.url === `/api/v1/settings/skills.project.${encodeURIComponent(PROJECT)}`)));
  const save = writes.find((write) => write.url.startsWith("/api/v1/settings/"));
  assert.deepEqual(JSON.parse(save.body), {
    value: JSON.stringify({ skills: [{ name: "rust-review", applies_to: ["review"], required: true }] }),
  });
  await eventually(() => assert.match(card.textContent, /Saved/));
});

async function eventually(assertion) {
  for (let attempt = 0; attempt < 50; attempt += 1) {
    try { assertion(); return; } catch (error) {
      if (attempt === 49) throw error;
      await new Promise((resolve) => setTimeout(resolve, 10));
    }
  }
}