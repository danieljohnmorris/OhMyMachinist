import React from "react";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { resolutionSummary } from "./run-links.js";
const zeroTime = "0001-01-01T00:00:00Z";

export function State({ value }) {
  const tones = {
    running: "border-warning/25 bg-warning/10 text-warning",
    queued: "border-warning/25 bg-warning/10 text-warning",
    succeeded: "border-success/25 bg-success/10 text-success",
    resolved: "border-success/25 bg-success/10 text-success",
    failed: "border-danger/25 bg-danger/10 text-danger",
    timed_out: "border-danger/25 bg-danger/10 text-danger",
    cancelled: "border-danger/25 bg-danger/10 text-danger",
  };
  return (
    <Badge className={cn("gap-1.5", tones[value] || tones.queued)}>
      <span className="size-1.5 rounded-full bg-current" />
      {stateLabel(value)}
    </Badge>
  );
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

// ResolutionLabel renders how a resolved ticket closed. The merged pull request
// and the login that merged it are links when they can be identified; every
// other resolution stays plain text.
export function ResolutionLabel({ job, result, className = "text-sm text-success" }) {
  const summary = resolutionSummary(job, result);
  if (!summary) return null;
  return (
    <p className={className} data-testid="resolution-label">
      {summary.parts.map((part, index) =>
        part.href ? (
          <a
            key={index}
            className="text-primary underline"
            href={part.href}
            target="_blank"
            rel="noopener noreferrer"
          >
            {part.text}
          </a>
        ) : (
          <React.Fragment key={index}>{part.text}</React.Fragment>
        ),
      )}
    </p>
  );
}
