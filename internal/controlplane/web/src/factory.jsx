import { useState } from "react";
import { OctagonX, Pause, Power, Server } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { QuietState } from "@/components/ui/page-heading";
import { busyLaneCount, factoryActions, factoryStateLabel, factoryStateSummary, jobCapMax, laneStateLabel, laneTone, stepJobCap } from "@/factory-controls";
import { capacityLabel } from "@/concurrency";
import { relativeTime } from "@/task-display.jsx";

// factoryRequest performs one control-plane factory change with the browser
// CSRF defense the other UI writes use.
async function factoryRequest(csrfToken, path, method, body) {
  const response = await fetch(path, {
    method,
    headers: { "Content-Type": "application/json", "X-Machinist-CSRF": csrfToken },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!response.ok) {
    const parsed = await response.json().catch(() => ({}));
    throw new Error(parsed.error || `Request failed (${response.status})`);
  }
  return response.json();
}

function stateTone(state) {
  if (state === "paused") return "danger";
  if (state === "draining") return "warning";
  return "active";
}

// FactoryControls is the one panel for pausing/resuming the factory, setting
// how many jobs run at once, and managing lanes. compact renders the sidebar
// form with just the state and Start/Stop.
export function FactoryControls({ factory, csrfToken, onChanged, compact = false, workers }) {
  const [pending, setPending] = useState("");
  const [confirming, setConfirming] = useState(false);
  const [reason, setReason] = useState("");
  const [error, setError] = useState("");

  if (!factory) return null;

  async function run(label, action) {
    setPending(label);
    setError("");
    try {
      await action();
      setConfirming(false);
      setReason("");
      await onChanged();
    } catch (requestError) {
      setError(requestError.message);
    } finally {
      setPending("");
    }
  }

  const start = () => run("start", () => factoryRequest(csrfToken, "/api/v1/factory/start", "POST"));
  const stop = (mode) => run(mode, () => factoryRequest(csrfToken, "/api/v1/factory/stop", "POST", { mode, reason: reason.trim() }));

  const summary = factoryStateSummary(factory, relativeTime);
  const stateBadge = <Badge tone={stateTone(factory.state)} dot>{factoryStateLabel(factory.state)}</Badge>;
  const busy = busyLaneCount(factory);
  const actions = factoryActions(factory);
  const buttons = <FactoryActionButtons actions={actions} pending={pending} compact={compact} onStart={start} onDrain={() => stop("drain")} onStopAll={() => (busy ? setConfirming(true) : stop("cancel"))} />;
  const confirmation = confirming && <StopAllConfirm busy={busy} reason={reason} setReason={setReason} pending={pending} onCancel={() => setConfirming(false)} onStop={() => stop("cancel")} />;

  if (compact) {
    // Single sidebar row: live capacity plus workers, with the state badge and
    // stop/start controls. The tooltip explains the terms.
    const online = workers?.online ?? 0;
    const total = workers?.total ?? 0;
    const capacityText = `${capacityLabel(factory.max_concurrent_jobs, busy, factory.ready_lanes || 0)} · ${online ? `${online} worker${online === 1 ? "" : "s"} online` : "No workers online"}`;
    return <div className="space-y-1">
      {error && <Alert inline>{error}</Alert>}
      <div className="flex items-center justify-between gap-2">
        <a href="#/factory" className="min-w-0 truncate text-xs text-muted-foreground underline" title={`Workers are connected lanes that can take a job. The limit is how many jobs may run at once. ${total} registered.`}>{capacityText}</a>
        {stateBadge}
      </div>
      {actions.length > 0 && <div className="flex gap-1">{buttons}</div>}
      {confirmation}
    </div>;
  }

  return <Card className="overflow-hidden">
    <header className="flex flex-wrap items-center justify-between gap-3 border-b border-border px-5 py-3">
      <div className="flex items-center gap-2"><h2 className="text-sm font-semibold">Factory controls</h2>{stateBadge}</div>
      {buttons}
    </header>
    <div className="space-y-5 p-5">
      {summary && <p className="text-xs text-muted-foreground">{summary}</p>}
      {error && <Alert>{error}</Alert>}
      {confirmation}
      <section aria-label="Capacity" className="space-y-5">
        <h3 className="text-base font-semibold">Capacity</h3>
        <p className="text-sm text-muted-foreground">Workers provide lanes. The concurrency limit controls how many can run together.</p>
        <JobCap factory={factory} csrfToken={csrfToken} onChanged={onChanged} />
        <LaneSlots factory={factory} csrfToken={csrfToken} onChanged={onChanged} />
      </section>
    </div>
  </Card>;
}

function FactoryActionButtons({ actions, pending, compact, onStart, onDrain, onStopAll }) {
  const disabled = Boolean(pending);
  return <div className={compact ? "grid gap-2" : "flex flex-wrap gap-2"}>
    {actions.includes("start") && <Button size="sm" disabled={disabled} onClick={onStart}><Power className="size-3.5 shrink-0" />{pending === "start" ? "Resuming…" : "Resume"}</Button>}
    {actions.includes("drain") && <Button size="sm" variant="outline" className="h-auto min-h-8 whitespace-normal py-2 text-left" disabled={disabled} onClick={onDrain} title="Running jobs finish; queued jobs wait."><Pause className="size-3.5 shrink-0" />{pending === "drain" ? "Stopping pickups…" : "Stop picking up new jobs"}</Button>}
    {actions.includes("cancel") && <Button size="sm" variant="outline" className="border-danger/40 text-danger hover:bg-danger/10" disabled={disabled} onClick={onStopAll}><OctagonX className="size-3.5 shrink-0" />{pending === "cancel" ? "Stopping…" : "Stop all now"}</Button>}
  </div>;
}

function StopAllConfirm({ busy, reason, setReason, pending, onCancel, onStop }) {
  return <div className="space-y-3 rounded-lg border border-danger/30 bg-danger/5 p-3">
    <p className="text-xs text-muted-foreground">{`Cancel ${busy} running job${busy === 1 ? "" : "s"} now? Queued jobs stay queued until you resume.`}</p>
    <input className="field-control" aria-label="Reason for stopping the factory" placeholder="Reason (optional)" value={reason} onChange={(event) => setReason(event.target.value)} />
    <div className="flex flex-wrap gap-2">
      <Button size="sm" variant="outline" className="border-danger/40 text-danger" disabled={Boolean(pending)} onClick={onStop}>{pending === "cancel" ? "Cancelling…" : "Confirm stop"}</Button>
      <Button size="sm" variant="ghost" disabled={Boolean(pending)} onClick={onCancel}>Keep running</Button>
    </div>
  </div>;
}

// JobCap is the stepper for jobs at once, clamped to the ready lanes.
function JobCap({ factory, csrfToken, onChanged }) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const max = jobCapMax(factory);
  const ready = factory.ready_lanes || 0;
  const unlimited = !(factory.max_concurrent_jobs > 0);
  const current = Math.min(max, Math.max(1, factory.max_concurrent_jobs || max));
  const apply = async (value) => {
    if (!unlimited && value === current) return;
    setPending(true);
    setError("");
    try {
      await factoryRequest(csrfToken, "/api/v1/factory/max-concurrent-jobs", "PUT", { value });
      await onChanged();
    } catch (requestError) {
      setError(requestError.message);
    } finally {
      setPending(false);
    }
  };
  return <section>
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div>
        <h4 className="text-sm font-medium">Concurrent jobs</h4>
        <p className="mt-0.5 text-xs text-muted-foreground">{!ready ? "No workers ready. Connect and enable a lane to run jobs." : unlimited ? `All ${ready} ready lane${ready === 1 ? "" : "s"} can run. Set a limit to use fewer.` : `${current} of ${ready} ready lane${ready === 1 ? "" : "s"} can run at once. Changes apply immediately.`}</p>
      </div>
      <div className="inline-flex items-center gap-1" role="group" aria-label="Jobs at once">
        <Button size="icon" variant="outline" aria-label="Fewer jobs at once" disabled={pending || !ready || current <= 1} onClick={() => apply(stepJobCap(current, -1, factory))}>−</Button>
        <span className="min-w-16 text-center text-sm font-semibold tabular-nums" aria-live="polite">{!ready ? "0 ready" : unlimited ? "All lanes" : current}</span>
        <Button size="icon" variant="outline" aria-label="More jobs at once" disabled={pending || !ready || (!unlimited && current >= max)} onClick={() => apply(unlimited ? max : stepJobCap(current, 1, factory))}>+</Button>
      </div>
    </div>
    {error && <Alert inline className="mt-2">{error}</Alert>}
  </section>;
}

// LaneSlots shows one slot per lane: lanes are the ready checkouts, the cap is
// how many run at once.
function LaneSlots({ factory, csrfToken, onChanged }) {
  const [pending, setPending] = useState("");
  const [error, setError] = useState("");
  const lanes = factory.lanes || [];
  const toggle = async (lane) => {
    setPending(lane.name);
    setError("");
    try {
      await factoryRequest(csrfToken, `/api/v1/factory/lanes/${encodeURIComponent(lane.name)}`, "PUT", { enabled: !lane.enabled });
      await onChanged();
    } catch (requestError) {
      setError(requestError.message);
    } finally {
      setPending("");
    }
  };
  return <section>
    <div className="mb-2 flex items-center justify-between gap-3">
      <h4 className="text-sm font-medium">Worker lanes</h4>
      <p className="text-xs text-muted-foreground">{factory.ready_lanes} ready · {lanes.length} registered</p>
    </div>
    {error && <Alert inline className="mb-2">{error}</Alert>}
    {lanes.length ? <ul className="overflow-hidden rounded-lg border border-border">{lanes.map((lane) => <li key={lane.instance_id} className="flex flex-wrap items-center gap-3 border-b border-border p-3 last:border-b-0">
      <Server className="size-4 shrink-0 text-muted-foreground" />
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2"><span className="truncate text-sm font-medium">{lane.name}</span><Badge tone={laneTone(lane)} dot>{laneStateLabel(lane)}</Badge></div>
        <p className="mt-0.5 truncate font-mono text-xs text-muted-foreground">{lane.job_id ? `job ${lane.job_id}` : lane.repositories?.length ? lane.repositories.join(", ") : "No repositories"}</p>
      </div>
      <Button size="sm" variant="outline" disabled={pending === lane.name} onClick={() => toggle(lane)}>{lane.enabled ? "Disable" : "Enable"}</Button>
    </li>)}</ul> : <p className="rounded-lg border border-border p-3 text-xs text-muted-foreground">No lanes registered. Start a worker to register this machine.</p>}
    <p className="mt-2 text-xs text-muted-foreground">Lanes are ready checkouts. A disabled lane stays connected but gets no new jobs.</p>
  </section>;
}

// FactorySettings is the Factory section inside Settings: the pause/resume
// controls, the concurrency cap and the worker lanes.
export function FactorySettings({ factory, csrfToken, onChanged, loaded, error }) {
  return <>
    {error && <Alert>{error}</Alert>}
    {!loaded && !error ? <Card><QuietState title="Reading the factory" description="Fetching the current dispatch state." role="status" /></Card>
      : <FactoryControls factory={factory} csrfToken={csrfToken} onChanged={onChanged} />}
  </>;
}