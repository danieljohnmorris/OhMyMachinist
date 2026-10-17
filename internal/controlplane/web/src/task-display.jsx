import React from "react";
import { Badge } from "@/components/ui/badge";
const zeroTime = "0001-01-01T00:00:00Z";

export function State({ value }) {
  const tones = { running: "active", queued: "neutral", succeeded: "success", resolved: "success", failed: "danger", timed_out: "danger", cancelled: "neutral" };
  return <Badge tone={tones[value] || "neutral"} dot>{stateLabel(value)}</Badge>;
}

export function friendlyName(name) {
  return String(name || "")
    .replaceAll("_", " ")
    .replaceAll("-", " ")
    .replace(/^./, (c) => c.toUpperCase());
}
export { relativeTime } from "./relative-time.js";
export function formatTimestamp(value) {
  return !value || value === zeroTime || !Number.isFinite(Date.parse(value))
    ? "Unavailable"
    : new Date(value).toLocaleString();
}
export function stateLabel(value) {
  return String(value || "unknown").replaceAll("_", " ");
}
