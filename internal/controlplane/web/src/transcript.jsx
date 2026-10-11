import React, { useEffect, useRef, useState } from "react";
import { Button } from "@/components/ui/button";
import { transcriptIsLive, transcriptLines, transcriptPollMillis } from "./transcript.js";

// How close to the bottom the scroller must stay for auto-scroll to follow new
// output; scrolling further up pauses following until the user returns.
const followThresholdPixels = 24;

// Shows the agent transcript for a run, polling while it is still active so the
// log grows live, then keeping the full log once the run finishes.
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
  const lines = state.runID === run?.id ? transcriptLines(state.events) : [];
  useEffect(() => {
    const element = scroller.current;
    if (live && element && following.current) element.scrollTop = element.scrollHeight;
  }, [lines.length, live]);
  function trackFollow() {
    const element = scroller.current;
    if (!element) return;
    following.current = element.scrollHeight - element.scrollTop - element.clientHeight <= followThresholdPixels;
  }
  function retry() {
    setState((current) => ({ ...current, error: "", attempt: current.attempt + 1 }));
  }
  if (!run?.id) return null;
  return (
    <section aria-label="Agent transcript" className="space-y-2">
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
      ) : (
        <pre
          ref={scroller}
          onScroll={trackFollow}
          className="h-[480px] overflow-auto whitespace-pre-wrap break-words rounded-md border border-border bg-muted/40 p-3 font-mono text-xs leading-5"
        >
          {lines.length
            ? lines.map((entry, index) => (
                <div key={index} className={entry.stream === "stderr" ? "text-danger" : entry.stream === "event" ? "text-muted-foreground" : undefined}>
                  {entry.text}
                </div>
              ))
            : live
              ? "Waiting for output…"
              : "No output recorded for this run."}
        </pre>
      )}
    </section>
  );
}
