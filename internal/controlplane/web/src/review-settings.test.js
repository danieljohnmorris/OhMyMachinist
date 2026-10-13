import assert from "node:assert/strict";
import test from "node:test";
import { resolvedFromPayload, profilesFromPayload } from "./review-settings.js";
import { routeFromHash } from "./routes.js";

test("payload parsing keeps the resolved profile fields", () => {
  assert.deepEqual(
    resolvedFromPayload({
      project: "omacom/mobile",
      source: "project",
      profile_name: "mobile-profile",
      profile: "name: mobile-profile\nversion: 1\n",
      extra_instructions: "extra rules",
    }),
    {
      project: "omacom/mobile",
      source: "project",
      profileName: "mobile-profile",
      profile: "name: mobile-profile\nversion: 1\n",
      extraInstructions: "extra rules",
    },
  );
});

test("payload parsing falls back to the empty shape", () => {
  assert.deepEqual(resolvedFromPayload(undefined), { project: "", source: "", profileName: "", profile: "", extraInstructions: "" });
  assert.deepEqual(resolvedFromPayload({ profile: 42 }), { project: "", source: "", profileName: "", profile: "", extraInstructions: "" });
});

test("library payload parsing keeps built-in flags", () => {
  assert.deepEqual(
    profilesFromPayload({ default: "default", profiles: [{ name: "default", built_in: true, body: "x" }, { name: "custom" }] }),
    {
      defaultName: "default",
      profiles: [
        { name: "default", builtIn: true, body: "x" },
        { name: "custom", builtIn: false, body: "" },
      ],
    },
  );
  assert.deepEqual(profilesFromPayload(undefined), { defaultName: "", profiles: [] });
});

test("settings is a routable page", () => {
  assert.equal(routeFromHash("#/settings").view, "settings");
});
