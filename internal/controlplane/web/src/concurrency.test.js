import assert from "node:assert/strict";
import test from "node:test";
import { concurrencyLabel } from "./concurrency.js";

test("sidebar shows singular limit for one job at a time", () => {
  assert.equal(concurrencyLabel(1, 0), "1 job at a time");
});

test("sidebar shows plural limit for several jobs at a time", () => {
  assert.equal(concurrencyLabel(4, 2), "4 jobs at a time");
});

test("sidebar shows no job limit when unlimited", () => {
  assert.equal(concurrencyLabel(null, 0), "No job limit");
  assert.equal(concurrencyLabel(undefined, 0), "No job limit");
  assert.equal(concurrencyLabel(0, 0), "No job limit");
});

test("sidebar flags running jobs above the limit as draining", () => {
  assert.equal(concurrencyLabel(1, 3), "3 running (limit 1, draining)");
});
