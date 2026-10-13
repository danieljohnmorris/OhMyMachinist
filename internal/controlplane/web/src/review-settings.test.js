import assert from "node:assert/strict";
import test from "node:test";
import { instructionsFromPayload } from "./review-settings.js";
import { routeFromHash } from "./routes.js";

test("payload parsing keeps stored instructions and custom flag", () => {
  assert.deepEqual(instructionsFromPayload({ instructions: "# rules", custom: true }), { instructions: "# rules", custom: true });
});

test("payload parsing falls back to the default shape", () => {
  assert.deepEqual(instructionsFromPayload({ instructions: "seeded", custom: false }), { instructions: "seeded", custom: false });
  assert.deepEqual(instructionsFromPayload(undefined), { instructions: "", custom: false });
  assert.deepEqual(instructionsFromPayload({ instructions: 42 }), { instructions: "", custom: false });
});

test("settings is a routable page", () => {
  assert.equal(routeFromHash("#/settings").view, "settings");
});
