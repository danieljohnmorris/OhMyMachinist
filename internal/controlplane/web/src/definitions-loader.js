export const DEFINITIONS_NETWORK_ERROR = "Couldn't load commands (connection lost), retrying";

// Backoff between attempts, capped at the last entry, so a deploy restart or
// dropped connection recovers without hammering the control plane.
export const DEFINITIONS_RETRY_DELAYS_MS = [1000, 2000, 4000, 8000, 15000];

// Repeatedly requests the definitions until one succeeds, then stops. A failed
// attempt keeps the last known value and reports why it is retrying; a later
// success clears the error. Network failures (no response) use a friendly
// message rather than the browser's bare "Failed to fetch".
export function createDefinitionsLoader({ request, apply, schedule = setTimeout, unschedule = clearTimeout }) {
  let cancelled = false;
  let timer = null;
  let failures = 0;

  async function attempt() {
    try {
      const value = await request();
      if (cancelled) return;
      failures = 0;
      apply({ kind: "success", value });
    } catch (error) {
      if (cancelled || error?.name === "AbortError") return;
      apply({ kind: "error", message: error?.network ? DEFINITIONS_NETWORK_ERROR : error.message });
      timer = schedule(attempt, DEFINITIONS_RETRY_DELAYS_MS[Math.min(failures, DEFINITIONS_RETRY_DELAYS_MS.length - 1)]);
      failures += 1;
    }
  }

  attempt();
  return {
    cancel() {
      cancelled = true;
      if (timer !== null) unschedule(timer);
    },
  };
}

// Success clears the retry error and swaps in fresh definitions; a failure
// keeps the last known value and shows the (friendlier) retry message.
export function applyDefinitions(current, outcome) {
  return outcome.kind === "success"
    ? { loading: false, error: "", value: outcome.value }
    : { ...current, loading: false, error: outcome.message };
}