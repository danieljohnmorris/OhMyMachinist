import { useMemo, useState } from "react";
import { Spinner } from "@/components/ui/status-icon";
import { analyticsState } from "@/analytics-state";
import { formatDurationMillis, formatReportingCoverage, formatSuccessRate, formatTokenUsage, tokenUsageSummary } from "@/run-metrics";

// Usage is the Settings summary of task outcomes, timing and reported tokens.
export function Usage({ jobs, loaded, error }) {
  const [days, setDays] = useState("30");
  const view = useMemo(() => analyticsState({ jobs, days, loaded, error }), [days, error, jobs, loaded]);
  const runs = view.runs || [];
  const usage = useMemo(() => tokenUsageSummary(runs), [runs]);
  const header = <h2 className="group-header">Usage<span className="flex-1" /><select className="compact-select" aria-label="Time window" value={days} onChange={(event) => setDays(event.target.value)}><option value="7">Last 7 days</option><option value="30">Last 30 days</option></select></h2>;
  if (view.kind === "error") return <section>{header}<p role="alert" className="row text-danger">{view.message}</p></section>;
  if (view.kind === "loading") return <section>{header}<p role="status" className="row text-faint"><Spinner />Loading usage</p></section>;
  const metrics = [
    ["Total tasks", view.metrics.totalTasks],
    ["Success rate", formatSuccessRate(view.metrics.successRate)],
    ["Average task time", formatDurationMillis(view.metrics.averageTaskDurationMillis)],
    ["Failed tasks", view.metrics.failedTasks],
    ["Active tasks", view.metrics.activeTasks],
    ["Total reported tokens", formatTokenUsage(usage.total)],
    ["Reporting coverage", formatReportingCoverage(usage)],
  ];
  return <section aria-label="Usage">
    {header}
    <dl className="grid grid-cols-2 border-b border-border sm:grid-cols-4">{metrics.map(([label, value]) => <div key={label} className="border-r border-b border-border px-4 py-3 last:border-r-0"><dt className="text-xs text-faint">{label}</dt><dd className="mt-0.5 text-base font-medium tabular-nums">{value}</dd></div>)}</dl>
    <details className="border-b border-border">
      <summary className="row cursor-pointer text-muted-foreground">Completed runs <span className="text-faint">{runs.length}</span></summary>
      {runs.length ? <table className="w-full text-left">
        <thead className="text-xs text-faint"><tr><th className="px-4 py-1.5 font-normal">Run</th><th className="px-4 py-1.5 font-normal">Command</th><th className="px-4 py-1.5 font-normal">Duration</th><th className="px-4 py-1.5 font-normal">Reported token usage</th></tr></thead>
        <tbody>{runs.map((run) => <tr key={run.id} className="border-t border-border"><td className="px-4 py-2 font-mono text-xs text-faint">{run.id.split("_").at(-1).slice(0, 8)}</td><td className="px-4 py-2 capitalize">{run.command}</td><td className="px-4 py-2 tabular-nums">{formatDurationMillis(run.duration_millis)}</td><td className="px-4 py-2 tabular-nums">{formatTokenUsage(run.token_usage)}</td></tr>)}</tbody>
      </table> : <p className="px-4 pb-3 text-faint">No completed runs in this window.</p>}
    </details>
  </section>;
}
