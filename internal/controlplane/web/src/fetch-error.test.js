import assert from "node:assert/strict";
import test from "node:test";
import { fetchErrorMessage } from "./fetch-error.js";

test("browser transport errors become a plain message", () => {
  assert.equal(
    fetchErrorMessage(new TypeError("Failed to fetch"), "Cannot reach the control plane. Retrying…"),
    "Cannot reach the control plane. Retrying…",
  );
  assert.equal(fetchErrorMessage(new TypeError("Load failed"), "unavailable"), "unavailable");
  assert.equal(fetchErrorMessage(new TypeError("NetworkError when attempting to fetch resource."), "unavailable"), "unavailable");
  assert.equal(fetchErrorMessage(new TypeError("Network request failed"), "unavailable"), "unavailable");
});

test("server messages pass through unchanged", () => {
  assert.equal(fetchErrorMessage(new Error("HTTP 404: run not found"), "unavailable"), "HTTP 404: run not found");
  assert.equal(fetchErrorMessage(new Error("Could not load outputs"), "unavailable"), "Could not load outputs");
  assert.equal(fetchErrorMessage(new Error("Status request failed (503)"), "unavailable"), "Status request failed (503)");
});

test("aborted requests report nothing and missing errors use the fallback", () => {
  const abort = new Error("The operation was aborted.");
  abort.name = "AbortError";
  assert.equal(fetchErrorMessage(abort, "unavailable"), "");
  assert.equal(fetchErrorMessage(undefined, "unavailable"), "unavailable");
  assert.equal(fetchErrorMessage("", "unavailable"), "unavailable");
});