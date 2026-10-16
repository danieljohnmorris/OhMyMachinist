import assert from "node:assert/strict";
import test from "node:test";
import { factoryStateLabel, factoryStateSummary, jobCapMax, laneStateLabel, stepJobCap } from "./factory-controls.js";

test("labels each dispatch state", () => {
  assert.equal(factoryStateLabel("running"), "Running");
  assert.equal(factoryStateLabel("paused"), "Paused");
  assert.equal(factoryStateLabel("draining"), "Draining");
  assert.equal(factoryStateLabel(undefined), "Running");
});

test("summarises a stop with who, why and when", () => {
  const summary = factoryStateSummary({ state: "paused", actor: "dan", reason: "spark build", since: "2026-10-16T00:00:00Z" }, () => "12m ago");
  assert.equal(summary, "Stopped by dan “spark build” since 12m ago");
});

test("has no summary while running", () => {
  assert.equal(factoryStateSummary({ state: "running", actor: "dan" }), "");
});

test("labels each lane slot", () => {
  assert.equal(laneStateLabel({ state: "busy" }), "Busy");
  assert.equal(laneStateLabel({ state: "idle" }), "Idle");
  assert.equal(laneStateLabel({ state: "offline" }), "Offline");
  assert.equal(laneStateLabel({ state: "disabled" }), "Disabled");
});

test("job cap never exceeds the ready lanes", () => {
  assert.equal(jobCapMax({ ready_lanes: 3 }), 3);
  assert.equal(jobCapMax({ ready_lanes: 0 }), 1);
  assert.equal(jobCapMax(undefined), 1);
});

test("stepping the cap clamps into range", () => {
  const factory = { ready_lanes: 2 };
  assert.equal(stepJobCap(1, -1, factory), 1);
  assert.equal(stepJobCap(2, 1, factory), 2);
  assert.equal(stepJobCap(1, 1, factory), 2);
});