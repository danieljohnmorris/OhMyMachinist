import React, { useEffect, useRef, useState } from "react";
import { transcriptIsLive, transcriptLines, transcriptPollMillis } from "./transcript.js";

// Shows the agent transcript for a run, polling while it is still active so the
// log grows live, then keeping the full log once the run finishes.
export function Transcript({ run, csrfToken }) {
  const [state, setState] = useState({ runID: null, events: "", error: "" });
  const scroller = useRef(null);
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
        if (!response.ok) throw new Error("Could not load transcript");
        const body = await response.json();
        if (controller.signal.aborted) return;
        setState({ runID: run.id, events: body.events || "", error: "" });
        if (transcriptIsLive(body.state)) timer = setTimeout(load, transcriptPollMillis);
      } catch (error) {
        if (controller.signal.aborted) return;
        setState((current) => ({ ...current, runID: run.id, error: error.message }));
        if (live) timer = setTimeout(load, transcriptPollMillis);
      }
    };
    load();
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [run?.id, run?.state, csrfToken]);
  const lines = state.runID === run?.id ? transcriptLines(state.events) : [];
  useEffect(() => {
    const element = scroller.current;
    if (live && element) element.scrollTop = element.scrollHeight;
  }, [lines.length, live]);
  if (!run?.id || (!lines.length && !live && !state.error)) return null;
  return (
    <section aria-label="Agent transcript" className="space-y-2">
      <div className="flex items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">Transcript</h2>
        {live && <span className="text-xs text-muted-foreground">Live</span>}
      </div>
      {state.error && <p role="alert" className="text-sm text-danger">{state.error}</p>}
      <pre
        ref={scroller}
        className="max-h-[480px] overflow-auto whitespace-pre-wrap break-words rounded-md border border-border bg-muted/40 p-3 font-mono text-xs leading-5"
      >
        {lines.length
          ? lines.map((entry, index) => (
              <div key={index} className={entry.stream === "stderr" ? "text-danger" : entry.stream === "event" ? "text-muted-foreground" : undefined}>
                {entry.text}
              </div>
            ))
          : "Waiting for output…"}
      </pre>
    </section>
  );
}
