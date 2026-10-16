// Browser fetch failures reject with a TypeError whose message is the transport
// detail, not something a reader can act on: "Failed to fetch" in Chromium,
// "Load failed" in Safari, "NetworkError when attempting to fetch resource." in
// Firefox. Translate those to a plain sentence, keep real server messages, and
// report nothing for a request the caller aborted.
const NETWORK_FAILURE_PATTERN = /failed to fetch|load failed|networkerror|network request failed|fetch failed|null is not an object/i;

export function fetchErrorMessage(error, fallback) {
  if (error?.name === "AbortError") return "";
  const message = error instanceof Error ? error.message : String(error ?? "");
  if (!message) return fallback;
  return NETWORK_FAILURE_PATTERN.test(message) ? fallback : message;
}