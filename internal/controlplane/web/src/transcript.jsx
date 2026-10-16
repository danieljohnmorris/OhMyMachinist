import React, { useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { TranscriptEntry } from "./transcript-entry.js";
import { transcriptAttempts, transcriptEntries, transcriptIsLive, transcriptPollMillis } from "./transcript.js";
import { formatTimestamp } from "./task-display.jsx";

// How close to the bottom the scroller must stay for auto-scroll to follow new
// output; scrolling further up pauses following until the user returns.
const followThresholdPixels = 24;

// A relaunched run records one attempt per process launch. The header names the
// attempt, its worker and model and how it ended, so the attempts read apart.
function attemptLabel(attempt, index, fallback) {
  if (attempt) {
    const parts = [`Attempt ${attempt.number ?? index + 1}`];
    if (attempt.started_at) parts.push(`started ${formatTimestamp(attempt.started_at)}`);
    if (attempt.worker_name) parts.push(`worker ${attempt.worker_name}`);
    if (attempt.model) parts.push(`model ${attempt.model}`);
    if (attempt.end_reason) parts.push(`ended: ${attempt.end_reason}`);
    return parts.join(", ");
  }
  return fallback || `Attempt ${index + 1}`;
}

// Shows the agent transcript for a run, polling while it is still active so the
// log grows live, then keeping the full log once the run finishes. Structured
// agent events (messages, thinking, tool calls and results) render readably;
// tool calls and results collapse so long transcripts stay scannable.
export function Transcript({ run, csrfToken }) {
  const [state, setState] = useState({ runID: null, events: "", error: "", attempt: 0 });
  const scroller = useRef(null);
  const following = useRef(true);
  const live = transcriptIsLive(run?.state);
  useEffect(() => {
    if (!run?.id) return;
    const controller = new AbortController();
    let timer;
    const load = async () => {
      try {
        const response = await fetch(`/api/v1/runs/${encodeURIComponent(run.id)}/events`, {
          headers: { "X-Machinist-CSRF": csrfToken },
          signal: controller.signal,
        });
        if (!response.ok) {
          const body = await response.json().catch(() => null);
          throw new Error(`HTTP ${response.status}${body?.error ? `: ${body.error}` : ""}`);
        }
        const body = await response.json();
        if (controller.signal.aborted) return;
        setState({ runID: run.id, events: body.events || "", error: "", attempt: 0 });
        if (transcriptIsLive(body.state)) timer = setTimeout(load, transcriptPollMillis);
      } catch (error) {
        if (controller.signal.aborted) return;
        setState((current) => ({ ...current, runID: run.id, error: `Could not load transcript (${error.message})` }));
        if (live) timer = setTimeout(load, transcriptPollMillis);
      }
    };
    load();
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [run?.id, run?.state, csrfToken, state.attempt]);
  const entries = state.runID === run?.id ? transcriptEntries(state.events) : [];
  const sections = transcriptAttempts(entries);
  const attemptsByNumber = new Map((run?.attempts || []).map((attempt) => [attempt.number, attempt]));
  // The latest attempt is expanded; the others start collapsed.
  const [expanded, setExpanded] = useState(() => new Set());
  useEffect(() => {
    if (sections.length > 1) setExpanded(new Set([sections.at(-1).number]));
  }, [sections.length, sections.at(-1)?.number]);
  function toggleAttempt(number) {
    setExpanded((current) => {
      if (current.has(number)) return new Set();
      return new Set([number]);
    });
  }
  // The transcript grows with the page (no inner scroll box). While live, the
  // window follows new output as long as the reader is near the bottom.
  useEffect(() => {
    if (!live) return undefined;
    const trackFollow = () => {
      const page = document.scrollingElement || document.documentElement;
      following.current = page.scrollHeight - window.scrollY - window.innerHeight <= followThresholdPixels;
    };
    window.addEventListener("scroll", trackFollow, { passive: true });
    return () => window.removeEventListener("scroll", trackFollow);
  }, [live]);
  useEffect(() => {
    const element = scroller.current;
    if (live && element && following.current) {
      const page = document.scrollingElement || document.documentElement;
      window.scrollTo({ top: page.scrollHeight });
    }
  }, [entries.length, live]);
  function retry() {
    setState((current) => ({ ...current, error: "", attempt: current.attempt + 1 }));
  }
  if (!run?.id) return null;
  return (
    <section aria-label="Agent transcript" className="space-y-2" ref={scroller}>
      <div className="flex items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Transcript</h2>
        {live && !state.error && <span className="text-xs text-muted-foreground">Live</span>}
      </div>
      {state.error ? (
        <div role="alert" className="flex flex-wrap items-center gap-3 rounded-md border border-border bg-muted/40 p-3">
          <p className="text-sm text-danger">{state.error}</p>
          <Button variant="outline" size="sm" onClick={retry}>
            Retry
          </Button>
        </div>
      ) : entries.length ? (
        sections.length > 1 ? (
          <div className="space-y-2">
            {sections.map((section, index) => {
              const attempt = attemptsByNumber.get(section.number);
              const open = expanded.has(section.number);
              return (
                <div key={section.number} className="rounded-md border border-border bg-muted/40 text-xs leading-5">
                  <button
                    type="button"
                    className="flex w-full items-center justify-between gap-2 px-3 py-2 text-left font-medium"
                    aria-expanded={open}
                    aria-label={`Attempt ${section.number} transcript`}
                    onClick={() => toggleAttempt(section.number)}
                  >
                    <span className="min-w-0 break-words">{attemptLabel(attempt, index, section.header)}</span>
                    <span className="shrink-0 text-muted-foreground">{open ? "Hide" : "Show"}</span>
                  </button>
                  {open && (
                    <div className="space-y-2 border-t border-border p-3">
                      {section.entries.map((entry, entryIndex) => (
                        <TranscriptEntry key={entryIndex} entry={entry} />
                      ))}
                    </div>
                  )}
                </div>
              );
            })}
          </div>
        ) : (
          <div className="space-y-2 rounded-md border border-border bg-muted/40 p-3 text-xs leading-5">
            {entries.map((entry, index) => (
              <TranscriptEntry key={index} entry={entry} />
            ))}
          </div>
        )
      ) : (
        <p className="rounded-md border border-border bg-muted/40 p-3 text-xs text-muted-foreground">
          {live ? "Waiting for output…" : "No output recorded for this run."}
        </p>
      )}
    </section>
  );
}
