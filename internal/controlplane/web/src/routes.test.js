import assert from "node:assert/strict";
import test from "node:test";
import { routeFromHash, settingsHash } from "./routes.js";

test("routeFromHash recognizes task detail routes", () => {
  assert.deepEqual(routeFromHash("#/runs/job_123"), { view: "task", section: "", jobID: "job_123", tab: "", attempt: "" });
  assert.deepEqual(routeFromHash("#/runs/job%2F123"), { view: "task", section: "", jobID: "job/123", tab: "", attempt: "" });
  assert.deepEqual(routeFromHash("#/runs/job_123?tab=transcript"), { view: "task", section: "", jobID: "job_123", tab: "transcript", attempt: "" });
  assert.deepEqual(routeFromHash("#/runs/job_123?tab=activity&attempt=job_456"), { view: "task", section: "", jobID: "job_123", tab: "activity", attempt: "job_456" });
  assert.deepEqual(routeFromHash("#/runs/job_123?attempt=job%2F456"), { view: "task", section: "", jobID: "job_123", tab: "", attempt: "job/456" });
});

test("routeFromHash falls back to runs for incomplete or malformed routes", () => {
  assert.deepEqual(routeFromHash("#/runs/"), { view: "runs", section: "", jobID: "", tab: "", attempt: "" });
  assert.deepEqual(routeFromHash("#/runs/%E0%A4%A"), { view: "runs", section: "", jobID: "", tab: "", attempt: "" });
  assert.deepEqual(routeFromHash("#/unknown"), { view: "runs", section: "", jobID: "", tab: "", attempt: "" });
});

test("routeFromHash opens settings tabs and keeps old configuration pages working", () => {
  const settings = (section) => ({ view: "settings", section, jobID: "", tab: "", attempt: "" });
  assert.deepEqual(routeFromHash("#/settings"), settings("review"));
  assert.deepEqual(routeFromHash("#/settings/skills"), settings("skills"));
  assert.deepEqual(routeFromHash("#/settings/nonsense"), settings("review"));
  assert.deepEqual(routeFromHash("#/workers"), settings("workers"));
  assert.deepEqual(routeFromHash("#/triggers"), settings("triggers"));
  assert.deepEqual(routeFromHash("#/factory"), settings("factory"));
  assert.deepEqual(routeFromHash("#/workflows"), settings("workflows"));
  assert.deepEqual(routeFromHash("#/commands"), settings("workflows"));
  assert.deepEqual(routeFromHash("#/skills"), settings("skills"));
});

test("settingsHash builds a valid settings route", () => {
  assert.equal(settingsHash("factory"), "#/settings/factory");
  assert.equal(settingsHash("unknown"), "#/settings/review");
});
