import { fetchErrorMessage } from "./fetch-error.js";

// The status poll runs continuously; a failed request is reported as a plain
// sentence instead of the browser's raw transport error.
const statusUnavailableMessage = "Cannot reach the control plane. Retrying…";

export function createStatusLoader({ request, apply }) {
  let latestRequest = 0;

  async function refresh() {
    const requestNumber = ++latestRequest;
    try {
      const status = await request();
      if (requestNumber !== latestRequest) return;
      apply({ kind: "success", status });
    } catch (error) {
      if (requestNumber !== latestRequest) return;
      apply({ kind: "error", message: fetchErrorMessage(error, statusUnavailableMessage) });
    }
  }

  return {
    refresh,
    cancel() { latestRequest += 1; },
  };
}
