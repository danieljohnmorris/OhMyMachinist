import { useState } from "react";
import { Power, Server } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { PageHeading, QuietState } from "@/components/ui/page-heading";
import { cn } from "@/lib/utils";
import { factoryStateLabel, factoryStateSummary, jobCapMax, laneStateLabel, laneTone, stepJobCap } from "@/factory-controls";
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
  if (state === "paused") return "tone-paused";
  if (state === "draining") return "tone-draining";
  return "tone-running";
}

// FactoryControls is the one panel for pausing/resuming the factory, setting
// how many jobs run at once, and managing lanes. compact renders the sidebar
// form with just the state and Start/Stop.
export function FactoryControls({ factory, csrfToken, onChanged, compact = false }) {
  const [pending, setPending] = useState("");
  const [confirming, setConfirming] = useState(false);
  const [reason, setReason] = useState("");
  const [error, setError] = useState("");

  if (!factory) return null;
  const stopped = factory.state === "paused" || factory.state === "draining";

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
  const stateBadge = <Badge className={cn(stateTone(factory.state))}><span className="size-1.5 rounded-full bg-current" />{factoryStateLabel(factory.state)}</Badge>;

  if (compact) {
    return <div className="space-y-2">
      <div className="flex items-center justify-between gap-2"><span className="text-xs text-muted-foreground">Factory</span>{stateBadge}</div>
      {error && <p role="alert" className="text-xs text-danger">{error}</p>}
      {stopped
        ? <Button size="sm" className="w-full" disabled={Boolean(pending)} onClick={start}><Power className="size-3.5" />{pending === "start" ? "Starting…" : "Start"}</Button>
        : <Button size="sm" variant="outline" className="w-full" disabled={Boolean(pending)} onClick={() => setConfirming(true)}><Power className="size-3.5" />Stop</Button>}
      {confirming && <StopConfirm reason={reason} setReason={setReason} pending={pending} onCancel={() => setConfirming(false)} onStop={stop} />}
    </div>;
  }

  return <Card className="overflow-hidden">
    <header className="flex flex-wrap items-center justify-between gap-3 border-b border-border px-5 py-3">
      <div className="flex items-center gap-2"><h2 className="text-sm font-semibold">Factory controls</h2>{stateBadge}</div>
      {stopped
        ? <Button size="sm" disabled={Boolean(pending)} onClick={start}><Power className="size-3.5" />{pending === "start" ? "Starting…" : "Start"}</Button>
        : <Button size="sm" variant="outline" disabled={Boolean(pending)} onClick={() => setConfirming(true)}><Power className="size-3.5" />Stop</Button>}
    </header>
    <div className="space-y-5 p-5">
      {summary && <p className="text-xs text-muted-foreground">{summary}</p>}
      {error && <p role="alert" className="rounded-md border border-danger/35 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</p>}
      {confirming && <StopConfirm reason={reason} setReason={setReason} pending={pending} onCancel={() => setConfirming(false)} onStop={stop} />}
      <JobCap factory={factory} csrfToken={csrfToken} onChanged={onChanged} />
      <LaneSlots factory={factory} csrfToken={csrfToken} onChanged={onChanged} />
    </div>
  </Card>;
}

// StopConfirm asks how the stop should treat the running job before it acts.
function StopConfirm({ reason, setReason, pending, onCancel, onStop }) {
  return <div className="space-y-3 rounded-lg border border-warning/30 bg-warning/5 p-3">
    <p className="text-xs text-muted-foreground">Stopping dispatch keeps workers connected. Choose what happens to the running job.</p>
    <input className="field-control" aria-label="Reason for stopping the factory" placeholder="Reason (optional)" value={reason} onChange={(event) => setReason(event.target.value)} />
    <div className="flex flex-wrap gap-2">
      <Button size="sm" variant="outline" disabled={Boolean(pending)} onClick={() => onStop("drain")}>{pending === "drain" ? "Draining…" : "Let running job finish"}</Button>
      <Button size="sm" variant="outline" className="border-danger/40 text-danger" disabled={Boolean(pending)} onClick={() => onStop("cancel")}>{pending === "cancel" ? "Cancelling…" : "Cancel running job"}</Button>
      <Button size="sm" variant="ghost" disabled={Boolean(pending)} onClick={onCancel}>Cancel</Button>
    </div>
  </div>;
}

// JobCap is the stepper for jobs at once, clamped to the ready lanes.
function JobCap({ factory, csrfToken, onChanged }) {
  const [pending, setPending] = useState(false);
  const [error, setError] = useState("");
  const max = jobCapMax(factory);
  const unlimited = !(factory.max_concurrent_jobs > 0);
  const current = Math.min(max, Math.max(1, factory.max_concurrent_jobs || 1));
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
        <h3 className="text-sm font-medium">Jobs at once</h3>
        <p className="mt-0.5 text-xs text-muted-foreground">{unlimited ? `No limit set. Setting one caps it at the ${max} ready lane${max === 1 ? "" : "s"}.` : `Applies immediately. Capped at the ${max} ready lane${max === 1 ? "" : "s"}.`}</p>
      </div>
      <div className="inline-flex items-center gap-1" role="group" aria-label="Jobs at once">
        <Button size="icon" variant="outline" aria-label="Fewer jobs at once" disabled={pending || current <= 1} onClick={() => apply(stepJobCap(current, -1, factory))}>−</Button>
        <span className="w-10 text-center text-sm font-semibold" aria-live="polite">{unlimited ? "No limit" : current}</span>
        <Button size="icon" variant="outline" aria-label="More jobs at once" disabled={pending || current >= max} onClick={() => apply(stepJobCap(current, 1, factory))}>+</Button>
      </div>
    </div>
    {error && <p role="alert" className="mt-2 text-xs text-danger">{error}</p>}
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
      <h3 className="text-sm font-medium">Lanes</h3>
      <p className="text-xs text-muted-foreground">{factory.ready_lanes} ready · {lanes.length} registered</p>
    </div>
    {error && <p role="alert" className="mb-2 text-xs text-danger">{error}</p>}
    {lanes.length ? <ul className="overflow-hidden rounded-lg border border-border">{lanes.map((lane) => <li key={lane.instance_id} className="flex flex-wrap items-center gap-3 border-b border-border p-3 last:border-b-0">
      <Server className="size-4 shrink-0 text-muted-foreground" />
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2"><span className="truncate text-sm font-medium">{lane.name}</span><Badge className={cn(laneTone(lane))}><span className="size-1.5 rounded-full bg-current" />{laneStateLabel(lane)}</Badge></div>
        <p className="mt-0.5 truncate font-mono text-xs text-muted-foreground">{lane.job_id ? `job ${lane.job_id}` : lane.repositories?.length ? lane.repositories.join(", ") : "No repositories"}</p>
      </div>
      <Button size="sm" variant="outline" disabled={pending === lane.name} onClick={() => toggle(lane)}>{lane.enabled ? "Disable" : "Enable"}</Button>
    </li>)}</ul> : <p className="rounded-lg border border-border p-3 text-xs text-muted-foreground">No lanes registered. Start a worker to register this machine.</p>}
    <p className="mt-2 text-xs text-muted-foreground">Lanes are ready checkouts. A disabled lane stays connected but gets no new jobs.</p>
  </section>;
}

// FactoryPage wraps the panel with the page chrome.
export function FactoryPage({ factory, csrfToken, onChanged, loaded, error }) {
  return <div className="mx-auto max-w-[1500px] space-y-6 p-4 sm:p-6 lg:p-8">
    <PageHeading title="Factory" description="Pause or resume the factory, set how many jobs run at once, and manage lanes." />
    {error && <div role="alert" className="rounded-md border border-danger/35 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</div>}
    {!loaded && !error ? <Card><QuietState title="Reading the factory" description="Fetching the current dispatch state." role="status" /></Card>
      : <FactoryControls factory={factory} csrfToken={csrfToken} onChanged={onChanged} />}
  </div>;
}