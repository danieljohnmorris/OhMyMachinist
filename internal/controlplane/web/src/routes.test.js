import assert from "node:assert/strict";
import test from "node:test";
import { routeFromHash } from "./routes.js";

test("routeFromHash recognizes task detail routes", () => {
  assert.deepEqual(routeFromHash("#/runs/job_123"), { view: "task", jobID: "job_123", tab: "", attempt: "" });
  assert.deepEqual(routeFromHash("#/runs/job%2F123"), { view: "task", jobID: "job/123", tab: "", attempt: "" });
  assert.deepEqual(routeFromHash("#/runs/job_123?tab=transcript"), { view: "task", jobID: "job_123", tab: "transcript", attempt: "" });
  assert.deepEqual(routeFromHash("#/runs/job_123?tab=activity&attempt=job_456"), { view: "task", jobID: "job_123", tab: "activity", attempt: "job_456" });
  assert.deepEqual(routeFromHash("#/runs/job_123?attempt=job%2F456"), { view: "task", jobID: "job_123", tab: "", attempt: "job/456" });
});

test("routeFromHash falls back to runs for incomplete or malformed routes", () => {
  assert.deepEqual(routeFromHash("#/runs/"), { view: "runs", jobID: "", tab: "", attempt: "" });
  assert.deepEqual(routeFromHash("#/runs/%E0%A4%A"), { view: "runs", jobID: "", tab: "", attempt: "" });
  assert.deepEqual(routeFromHash("#/unknown"), { view: "runs", jobID: "", tab: "", attempt: "" });
});
