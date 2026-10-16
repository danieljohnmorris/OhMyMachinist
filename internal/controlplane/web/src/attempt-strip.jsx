import React from "react";
import { attemptEndReason, attemptStripEntries, attemptTone, attemptTime } from "./runs-board.js";
import { relativeTime } from "./relative-time.js";

// Most board-card dots are the eight newest attempts; older ones collapse into
// a "+K" count so the strip never widens past the card.
export const maxAttemptDots = 8;

const dotToneClasses = {
  success: "bg-success",
  failed: "bg-danger",
  cancelled: "bg-muted-foreground",
  timedOut: "bg-danger/60",
  stalled: "bg-warning",
  running: "bg-warning",
  queued: "bg-border",
};

// AttemptStrip replaces the old list of identical "cancelled · 2d ago" links
// with one compact line: "Attempt N of M" and a coloured dot per attempt, one
// dot per end state, latest on the right. Each dot is a keyboard-focusable link
// to its attempt with a hover/focus tooltip carrying the end reason and time.
export function AttemptStrip({ attempts = [], now }) {
  if (attempts.length < 2) return null;
  const { total, overflow, visible } = attemptStripEntries(attempts, maxAttemptDots);
  return (
    <div
      className="flex min-w-0 flex-nowrap items-center gap-2 border-t border-border px-4 py-2.5"
      data-testid="attempt-strip"
    >
      <span className="shrink-0 whitespace-nowrap text-xs text-muted-foreground">
        Attempt {total} of {total}
      </span>
      <span className="flex shrink-0 items-center gap-1.5">
        {overflow > 0 && (
          <span className="shrink-0 text-xs text-muted-foreground" data-testid="attempt-overflow">
            +{overflow}
          </span>
        )}
        {visible.map((attempt, index) => {
          const number = overflow + index + 1;
          const reason = attemptEndReason(attempt);
          const ago = relativeTime(attemptTime(attempt), now);
          const label = `Attempt ${number} of ${total} · ${reason} · ${ago}`;
          return (
            <a
              key={attempt.id}
              href={`#/runs/${encodeURIComponent(attempt.id)}`}
              title={label}
              aria-label={label}
              data-testid={`attempt-dot-${number}`}
              className={`size-2.5 shrink-0 rounded-full outline-none transition focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-surface ${
                dotToneClasses[attemptTone(attempt.state)] || dotToneClasses.queued
              }`}
            />
          );
        })}
      </span>
    </div>
  );
}