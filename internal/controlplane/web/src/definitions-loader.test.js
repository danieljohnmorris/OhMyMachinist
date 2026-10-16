import assert from "node:assert/strict";
import test from "node:test";
import { DEFINITIONS_NETWORK_ERROR, DEFINITIONS_RETRY_DELAYS_MS, applyDefinitions, createDefinitionsLoader } from "./definitions-loader.js";

function networkFailure() {
  const error = new Error("definitions request failed before a response");
  error.network = true;
  return error;
}

function harness(request) {
  const applied = [];
  const scheduled = [];
  const loader = createDefinitionsLoader({
    request,
    apply: (outcome) => applied.push(outcome),
    schedule: (fn, ms) => { scheduled.push({ fn, ms }); return scheduled.length; },
    unschedule: () => {},
  });
  return { applied, scheduled, loader };
}

test("a lost connection is retried with a clearer message", async () => {
  const { applied, scheduled } = harness(() => Promise.reject(networkFailure()));
  await delay();

  assert.deepEqual(applied, [{ kind: "error", message: DEFINITIONS_NETWORK_ERROR }]);
  assert.deepEqual(scheduled.map((entry) => entry.ms), [DEFINITIONS_RETRY_DELAYS_MS[0]]);
});

test("retries back off and cap at the slowest delay", async () => {
  const { scheduled, loader } = harness(() => Promise.reject(new Error("Definitions request failed (503)")));
  await delay();

  for (let index = 1; index < DEFINITIONS_RETRY_DELAYS_MS.length + 2; index += 1) {
    const next = scheduled[index - 1];
    next.fn();
    await delay();
  }

  assert.deepEqual(scheduled.map((entry) => entry.ms), [
    ...DEFINITIONS_RETRY_DELAYS_MS,
    DEFINITIONS_RETRY_DELAYS_MS.at(-1),
    DEFINITIONS_RETRY_DELAYS_MS.at(-1),
  ]);
  assert.equal(scheduled.length, DEFINITIONS_RETRY_DELAYS_MS.length + 2);
  loader.cancel();
});

test("a recovered request reports success and stops retrying", async () => {
  let attempts = 0;
  const { applied, scheduled } = harness(() => ++attempts === 1 ? Promise.reject(networkFailure()) : Promise.resolve({ commands: [] }));
  await delay();

  assert.equal(scheduled.length, 1);
  scheduled[0].fn();
  await delay();

  assert.deepEqual(applied, [
    { kind: "error", message: DEFINITIONS_NETWORK_ERROR },
    { kind: "success", value: { commands: [] } },
  ]);
  assert.equal(scheduled.length, 1);
});

test("server errors keep their own message while still retrying", async () => {
  const { applied } = harness(() => Promise.reject(new Error("Definitions request failed (503)")));
  await delay();

  assert.deepEqual(applied, [{ kind: "error", message: "Definitions request failed (503)" }]);
});

test("cancel stops further attempts", async () => {
  const { applied, scheduled, loader } = harness(() => Promise.reject(networkFailure()));
  await delay();
  loader.cancel();
  scheduled[0].fn();
  await delay();

  assert.equal(applied.length, 1);
});

test("success clears the error and swaps in the new definitions", () => {
  const stale = { loading: false, error: DEFINITIONS_NETWORK_ERROR, value: { commands: [{ name: "old" }] } };
  assert.deepEqual(applyDefinitions(stale, { kind: "success", value: { commands: [{ name: "new" }] } }), {
    loading: false,
    error: "",
    value: { commands: [{ name: "new" }] },
  });
});

test("a retry failure keeps the last known definitions and shows the retry message", () => {
  const current = { loading: false, error: "", value: { commands: [{ name: "old" }] } };
  assert.deepEqual(applyDefinitions(current, { kind: "error", message: DEFINITIONS_NETWORK_ERROR }), {
    loading: false,
    error: DEFINITIONS_NETWORK_ERROR,
    value: { commands: [{ name: "old" }] },
  });
});

function delay() {
  return new Promise((resolve) => setTimeout(resolve, 0));
}