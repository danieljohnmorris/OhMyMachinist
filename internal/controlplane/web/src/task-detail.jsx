import React, { useEffect, useState } from "react";
import { ArrowLeft, ArrowRight, Check } from "lucide-react";
import { Tabs } from "@/components/ui/tabs";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import { shortVersion } from "./skills.js";
import { Artifacts, useTaskArtifacts } from "./artifacts.jsx";
import { Transcript } from "./transcript.jsx";
import { taskPresentation } from "./task-presentation.js";
import { linkifyText } from "./linkify.js";
import { Markdown } from "./markdown.js";
import { jobDisplayTitle, canCancelJob, canMoveToQueue, resolutionLabel, supersededJob, ticketAttempts, attemptStart, attemptDurationMillis } from "./runs-board.js";
import { runSidebarLinks, safeExternalURL, trackerNames } from "./run-links.js";
import { formatDurationMillis, formatTokenUsage } from "./run-metrics.js";
import { activityRows } from "./run-activity.js";
import {
  State,
  friendlyName,
  stateLabel,
  formatTimestamp,
} from "./task-display.jsx";

export function TaskDetail({
  csrfToken,
  job,
  jobs = [],
  loaded,
  error,
  deleting,
  onDelete,
  onWorkflowAction,
  onCancelJob,
  onMoveToQueue,
  onAttemptChange,
  tab,
  onTabChange,
}) {
  const artifacts = useTaskArtifacts(job, csrfToken);
  if (!job)
    return (
      <div className="p-8">
        <a href="#/runs" className="text-sm underline">
          Back to runs
        </a>
        <p className="mt-4">{!loaded ? "Loading run…" : "Run not found."}</p>
        {error && <p role="alert">{error}</p>}
      </div>
    );
  const terminal = ["succeeded", "failed", "cancelled", "timed_out", "resolved"].includes(job.state);
  const latest = job.runs.at(-1);
  const lastCompleted = job.runs.findLast((run) => run.outcome === "complete");
  const { result, history, stages } = taskPresentation(job);
  const { ticket, pullRequest } = runSidebarLinks(job, result);
  const reviewing = job.state === "awaiting_approval";
  const retriedAs = supersededJob(jobs, job);
  const attempts = ticketAttempts(jobs, job);
  return (
    <div className="mx-auto max-w-[1500px] space-y-6 p-4 sm:p-6 lg:p-8">
      <header className="space-y-4">
        <Button asChild variant="ghost" size="sm" className="-ml-3">
          <a href="#/runs">
            <ArrowLeft className="size-4" />
            Back to runs
          </a>
        </Button>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <h1 className="min-w-0 break-words text-2xl font-semibold">
            {jobDisplayTitle(job)}
          </h1>
          <div className="flex flex-wrap items-center gap-3">
            <State value={job.state} />
            {canMoveToQueue(job) && <MoveToQueueButton job={job} onMoveToQueue={onMoveToQueue} />}
          </div>
        </div>
        {resolutionLabel(job) && (
          <p className="text-sm text-success" data-testid="resolution-label">{resolutionLabel(job)}</p>
        )}
        <p className="text-sm text-muted-foreground">
          {job.repository} · {friendlyName(job.workflow?.name || job.command)}
        </p>
        <div className="flex flex-wrap items-center gap-x-5 gap-y-2 text-sm">
          {ticket && <a className="text-primary underline" href={ticket.href} target={ticket.external ? "_blank" : undefined} rel={ticket.external ? "noopener noreferrer" : undefined}>{trackerNames[ticket.tracker] || "Ticket"} · {ticket.text}</a>}
          {pullRequest && <a className="text-primary underline" href={pullRequest.href} target="_blank" rel="noopener noreferrer">{pullRequest.text}</a>}
          {latest?.model && <span className="font-mono text-xs text-muted-foreground">{latest.model}</span>}
        </div>
        {retriedAs && (
          <Card
            className="flex flex-wrap items-center gap-2 p-4"
            aria-label="Retried as"
          >
            <ArrowRight className="size-4 text-muted-foreground" aria-hidden="true" />
            <span className="text-sm">Retried as</span>
            <a
              className="text-sm font-medium text-primary underline"
              href={`#/runs/${encodeURIComponent(retriedAs.id)}`}
            >
              {jobDisplayTitle(retriedAs)}
            </a>
            <State value={retriedAs.state} />
          </Card>
        )}
        {safeExternalURL(job.task?.source_url) && (
          <a
            className="block text-sm text-primary underline"
            href={safeExternalURL(job.task.source_url)}
            target="_blank"
            rel="noreferrer"
          >
            Source ↗
          </a>
        )}
        {error && (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        )}
        {job.metadata && (
          <details className="border-y border-border py-3" aria-label="Job metadata">
            <summary className="cursor-pointer text-sm font-medium">Delivery details</summary>
            <dl className="mt-4 grid gap-4 text-sm sm:grid-cols-2">
              {[
                ["Branch", job.metadata?.branch],
                ["Preview", job.metadata?.preview_url],
                ["Review verdict", job.metadata?.review_verdict],
                ["Review summary", job.metadata?.review_summary],
                ["Intake", job.metadata?.intake_source],
                ["Plane delivery", job.metadata?.intake_delivery_id],
                ["Spec hash", job.metadata?.intake_spec_hash],
                ["Labels", job.metadata?.labels?.length ? job.metadata.labels.join(", ") : undefined],
                [
                  "Diagnosis",
                  job.metadata?.healing &&
                    `(${job.metadata.healing.cause}) ${job.metadata.healing.fix || ""} ${
                      job.metadata.healing.exhausted ? "Retry budget exhausted." : ""
                    }`.trim(),
                ],
                [
                  "Auto-merge",
                  job.metadata?.automerge &&
                    [
                      `#${job.metadata.automerge.pr_number || ""} ${job.metadata.automerge.state}`.trim(),
                      job.metadata.automerge.merge_sha && `merged ${job.metadata.automerge.merge_sha}`,
                      job.metadata.automerge.deploy_result && `deploy: ${job.metadata.automerge.deploy_result}`,
                      job.metadata.automerge.health_result && `health: ${job.metadata.automerge.health_result}`,
                      job.metadata.automerge.reason,
                    ]
                      .filter(Boolean)
                      .join(" · "),
                ],
                ["Status", stateLabel(job.state)],
                ["Created", formatTimestamp(job.created_at)],
                ["Updated", formatTimestamp(job.updated_at)],
              ]
                .filter(([, value]) => value)
                .map(([label, value]) => (
                  <div key={label} className="min-w-0">
                    <dt className="text-xs uppercase tracking-wide text-muted-foreground">{label}</dt>
                    <dd className="mt-1 break-words">
                      {linkifyText(value)}
                    </dd>
                  </div>
                ))}
            </dl>
            {job.metadata?.screenshots?.length > 0 && (
              <div>
                <h3 className="text-sm font-medium">Screenshots</h3>
                <ul className="mt-3 flex flex-wrap gap-3">
                  {job.metadata.screenshots.map((screenshot, index) => {
                    const source = screenshot.artifact_id
                      ? `/api/v1/artifacts/${encodeURIComponent(screenshot.artifact_id)}/content`
                      : screenshot.url || screenshot.thumbnail_url;
                    if (!source) return null;
                    return (
                      <li key={index}>
                        <a className="block border border-border" href={source} target="_blank" rel="noreferrer">
                          {screenshot.thumbnail_url || screenshot.artifact_id ? (
                            <img className="h-24 w-auto" src={screenshot.thumbnail_url || source} alt={screenshot.label || "Job screenshot"} />
                          ) : (
                            <span className="block p-3 text-sm">{screenshot.label || source}</span>
                          )}
                        </a>
                      </li>
                    );
                  })}
                </ul>
              </div>
            )}
          </details>
        )}
      </header>
      {stages.length > 1 && (
        <ol
          className="flex flex-wrap items-center gap-3 text-sm"
          aria-label="Run progress"
        >
          {stages.map((stage, index) => (
            <li
              key={index}
              className="flex items-center gap-3"
              aria-current={stage.current ? "step" : undefined}
            >
              {index > 0 && (
                <ArrowRight
                  className="size-4 text-muted-foreground"
                  aria-hidden="true"
                />
              )}
              <span
                className={cn(
                  "flex items-center gap-2 py-1",
                  stage.current
                    ? "font-medium text-foreground"
                    : "text-muted-foreground",
                )}
              >
                {stage.complete ? (
                  <Check
                    className="size-4 text-success"
                    aria-label="Complete"
                  />
                ) : (
                  <span className="text-xs">{index + 1}</span>
                )}
                {friendlyName(stage.name)}
                {stage.current && (
                  <span className="text-xs text-muted-foreground">
                    {reviewing ? "Awaiting approval" : stateLabel(job.state)}
                  </span>
                )}
              </span>
            </li>
          ))}
        </ol>
      )}
      {attempts.length > 1 && (
        <div
          className="flex flex-wrap items-center gap-3"
          aria-label="Attempt picker"
        >
          <label className="text-sm font-medium" htmlFor="attempt-picker">
            Attempt
          </label>
          <select
            id="attempt-picker"
            className="field-control w-auto"
            value={job.id}
            onChange={(event) => onAttemptChange?.(event.target.value)}
          >
            {attempts.map((attempt, index) => {
              const start = attemptStart(attempt);
              const duration = attemptDurationMillis(attempt);
              return (
                <option key={attempt.id} value={attempt.id}>
                  {[
                    `Attempt ${index + 1}`,
                    stateLabel(attempt.state),
                    start ? `started ${formatTimestamp(start)}` : "",
                    duration == null ? "" : formatDurationMillis(duration),
                  ]
                    .filter(Boolean)
                    .join(" · ")}
                </option>
              );
            })}
          </select>
          <State value={job.state} />
        </div>
      )}
      <div
        className="grid min-w-0 items-start gap-6 lg:grid-cols-[minmax(0,1fr)_320px]"
        aria-label="Run sections"
      >
        <Tabs
          key={job.id}
          label="Run spec, transcript, activity, artifacts and history"
          selected={tab || "transcript"}
          onSelect={onTabChange}
          items={[
            {
              id: "spec",
              label: "Spec",
              content: (
                <section aria-label="Instructions">
                  <Markdown
                    className="break-words leading-6"
                    source={
                      job.task
                        ? job.task.spec || "Use the linked source for requirements."
                        : job.prompt
                    }
                  />
                </section>
              ),
            },
            {
              id: "transcript",
              label: "Transcript",
              content: latest ? (
                <div className="space-y-3">
                  {canMoveToQueue(job) && (
                    <div className="flex flex-wrap items-center justify-between gap-2 rounded-md border border-border bg-muted/40 px-3 py-2">
                      <p className="text-xs text-muted-foreground">
                        This attempt is still in flight. Moving the job back to
                        the queue stops it, saves its uncommitted work and
                        starts a new attempt.
                      </p>
                      <MoveToQueueButton job={job} onMoveToQueue={onMoveToQueue} />
                    </div>
                  )}
                  <Transcript run={latest} csrfToken={csrfToken} />
                </div>
              ) : (
                <p className="text-sm text-muted-foreground">
                  No transcript yet.
                </p>
              ),
            },
            {
              id: "activity",
              label: "Activity",
              content: <RunActivity job={job} />,
            },
            ...(job.task && lastCompleted
              ? [
                  {
                    id: "files",
                    label: "Files",
                    content: (
                      <Artifacts
                        artifacts={artifacts}
                        runID={lastCompleted.id}
                        csrfToken={csrfToken}
                      />
                    ),
                  },
                ]
              : []),
            ...(history.length
              ? [
                  {
                    id: "history",
                    label: "History",
                    content: (
                      <ol className="space-y-4">
                        {history.map((run) => (
                          <li
                            key={run.id}
                            className="space-y-3 border-l-2 border-border pl-4"
                          >
                            <div className="flex flex-wrap items-center justify-between gap-2">
                              <h3 className="font-medium">
                                {run.outcome === "changes_requested"
                                  ? "Changes requested"
                                  : friendlyName(run.command)}
                              </h3>
                              <State
                                value={
                                  run.outcome === "complete"
                                    ? "succeeded"
                                    : run.outcome || run.state
                                }
                              />
                            </div>
                            {run.summary && (
                              <p className="whitespace-pre-wrap break-words leading-6">
                                {linkifyText(run.summary)}
                              </p>
                            )}
                            {run.error && run.error !== run.summary && (
                              <p className="text-danger">{linkifyText(run.error)}</p>
                            )}
                            {job.task && (
                              <Artifacts
                                artifacts={artifacts}
                                runID={run.id}
                                csrfToken={csrfToken}
                              />
                            )}
                            <ExecutionDetails run={run} />
                          </li>
                        ))}
                      </ol>
                    ),
                  },
                ]
              : []),
          ]}
        />
        <div className="space-y-5">
          <Card
            className="space-y-5 p-5 sm:p-6"
            aria-label="Current result"
          >
            <h2 className="text-lg font-semibold">
              {resultTitle(job, result)}
            </h2>
            {result?.summary && (
              <p className="whitespace-pre-wrap break-words text-sm leading-6 text-muted-foreground">
                {linkifyText(result.summary)}
              </p>
            )}
            {result?.error && result.error !== result.summary && (
              <p
                role="alert"
                className="whitespace-pre-wrap break-words text-sm text-danger"
              >
                {linkifyText(result.error)}
              </p>
            )}
            {result && job.task && (
              <Artifacts
                key={result.id}
                artifacts={artifacts}
                runID={result.id}
                csrfToken={csrfToken}
              />
            )}
            {job.workflow && (
              <TaskActions
                key={`${job.id}:${latest?.id}:${job.state}`}
                job={job}
                result={result}
                onAction={onWorkflowAction}
              />
            )}
            {canCancelJob(job) && <CancelJobButton job={job} onCancel={onCancelJob} />}
          </Card>
          <RunSkills run={latest} />
          <div
            className="space-y-6 rounded-sm border border-border bg-surface p-5 text-sm sm:p-6"
            aria-label="Run details"
          >
            <h2 className="text-lg font-semibold">Details</h2>
            {result?.revision && (
              <section>
                <h2 className="mb-2 font-medium">Requested changes</h2>
                <p className="whitespace-pre-wrap break-words">
                  {linkifyText(result.revision.feedback)}
                </p>
              </section>
            )}
            {result?.summary && (
              <section>
                <h2 className="mb-2 font-medium">Full summary</h2>
                <p className="whitespace-pre-wrap break-words leading-6">
                  {linkifyText(result.summary)}
                </p>
              </section>
            )}
            {result && <ExecutionDetails run={result} />}
            <section className="border-t border-border pt-4">
              <dl className="my-4 grid gap-3 sm:grid-cols-2">
                <RunMetric label="Run ID" value={job.id} />
                <RunMetric label="Repository" value={job.repository} />
                <RunMetric
                  label="Created"
                  value={formatTimestamp(job.created_at)}
                />
                <RunMetric
                  label="Updated"
                  value={formatTimestamp(job.updated_at)}
                />
              </dl>
              <Button
                variant="outline"
                disabled={!terminal || deleting}
                onClick={() => onDelete(job)}
              >
                {deleting ? "Deleting…" : "Delete run"}
              </Button>
            </section>
          </div>
        </div>
      </div>
    </div>
  );
}

// Run activity lists the job's state transitions, oldest first, with a
// relative time per transition and the duration spent in each exited state.
// The current state keeps a live duration while the run is still in progress.
// Relative times refresh periodically without a full page reload; new
// transitions arrive through the page's existing status refresh.
function RunActivity({ job }) {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    const timer = setInterval(() => setNow(new Date()), 30000);
    return () => clearInterval(timer);
  }, []);
  const rows = activityRows(job, now);
  if (!rows.length)
    return (
      <Card className="space-y-3 p-5 sm:p-6" aria-label="Run activity">
        <h2 className="text-lg font-semibold">Activity</h2>
        <p className="text-sm text-muted-foreground">No activity recorded.</p>
      </Card>
    );
  return (
    <Card className="space-y-3 p-5 sm:p-6" aria-label="Run activity">
      <h2 className="text-lg font-semibold">Activity</h2>
      <ol className="space-y-3 text-sm">
        {rows.map((row, index) => (
          <li
            key={index}
            className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1"
          >
            <time
              className="min-w-0"
              dateTime={row.at}
              title={formatTimestamp(row.at)}
            >
              {row.kind === "transition"
                ? `${stateLabel(row.from)} → ${stateLabel(row.to)}`
                : stateLabel(row.state)}
              <span className="text-muted-foreground"> · {row.ago}</span>
            </time>
            {row.duration && (
              <span className="text-xs text-muted-foreground">
                {stateLabel(row.kind === "transition" ? row.from : row.state)}{" "}
                {row.duration}
              </span>
            )}
          </li>
        ))}
      </ol>
    </Card>
  );
}

// RunSkills shows the skills a run loaded (name, version, scope and injection
// method) and any load warnings, recorded by the worker and returned with the
// run. It renders nothing when the run loaded no skills and reported none.
function RunSkills({ run }) {
  const skills = run?.skills?.skills || [];
  const warnings = run?.skills?.warnings || [];
  if (!skills.length && !warnings.length) return null;
  return (
    <Card className="space-y-3 p-5 sm:p-6" aria-label="Loaded skills">
      <h2 className="text-lg font-semibold">Skills</h2>
      {skills.length > 0 ? (
        <ul className="space-y-2 text-sm">
          {skills.map((skill) => (
            <li key={`${skill.name}:${skill.version}`} className="flex flex-wrap items-center gap-2">
              <span className="font-medium">{skill.name}</span>
              {skill.version && (
                <span className="font-mono text-xs text-muted-foreground" title={skill.version}>
                  {shortVersion(skill.version)}
                </span>
              )}
              {skill.scope && <Badge className="font-normal capitalize">{skill.scope}</Badge>}
              {skill.method && <Badge className="font-normal">{skill.method}</Badge>}
            </li>
          ))}
        </ul>
      ) : (
        <p className="text-sm text-muted-foreground">No skills loaded.</p>
      )}
      {warnings.length > 0 && (
        <ul className="space-y-1 rounded-md border border-warning/35 bg-warning/10 px-3 py-2 text-sm text-warning" aria-label="Skill warnings">
          {warnings.map((warning) => <li key={warning}>{warning}</li>)}
        </ul>
      )}
    </Card>
  );
}

function ExecutionDetails({ run }) {
  return (
    <section
      aria-label="execution details"
      className="text-xs text-muted-foreground"
    >
      <dl className="mt-3 grid gap-3 sm:grid-cols-3">
        <RunMetric label="Started" value={formatTimestamp(run.started_at)} />
        <RunMetric
          label="Completed"
          value={formatTimestamp(run.completed_at)}
        />
        <RunMetric
          label="Exit code"
          value={
            run.exit_code === undefined ? "Unavailable" : String(run.exit_code)
          }
        />
        <RunMetric label="Run ID" value={run.id} mono />
        <RunMetric label="Executor" value={run.executor} />
        <RunMetric label="Worker" value={run.worker_name || "Unassigned"} />
        <RunMetric
          label="Duration"
          value={
            Number.isSafeInteger(run.duration_millis)
              ? formatDurationMillis(run.duration_millis)
              : "Not available"
          }
        />
        <RunMetric label="Model" value={run.model || "Executor default"} />
        {run.wip && <RunMetric label="Saved work" value={run.wip} />}
        <RunMetric
          label="Tokens"
          value={
            formatTokenUsage(run.token_usage) === "Unavailable"
              ? "Not reported"
              : formatTokenUsage(run.token_usage)
          }
        />
      </dl>
    </section>
  );
}

function RunMetric({ label, value, mono = false }) {
  return (
    <div className="min-w-0">
      <dt className="text-xs text-muted-foreground">{label}</dt>
      <dd
        className={cn("mt-0.5 truncate text-sm", mono && "font-mono")}
        title={value}
      >
        {value}
      </dd>
    </div>
  );
}

function CancelJobButton({ job, onCancel }) {
  const [busy, setBusy] = useState(false);
  return (
    <Button
      variant="outline"
      disabled={busy}
      aria-label={`Cancel run ${jobDisplayTitle(job)}`}
      onClick={async () => {
        setBusy(true);
        try {
          await onCancel(job);
        } finally {
          setBusy(false);
        }
      }}
    >
      {busy ? "Cancelling…" : "Cancel run"}
    </Button>
  );
}

// MoveToQueueButton is the one action for a running or stalled attempt
// (MACH-66): it stops the attempt, saves the lane's uncommitted work and puts
// the same job back in the queue as the next attempt. The confirm lives in
// the handler, so every surface asks the same question.
function MoveToQueueButton({ job, onMoveToQueue }) {
  const [busy, setBusy] = useState(false);
  return (
    <Button
      variant="outline"
      disabled={busy}
      aria-label={`Move task ${jobDisplayTitle(job)} back to the queue`}
      onClick={async () => {
        setBusy(true);
        try {
          await onMoveToQueue(job);
        } finally {
          setBusy(false);
        }
      }}
    >
      {busy ? "Moving…" : "Move to queue"}
    </Button>
  );
}

function TaskActions({ job, result, onAction }) {
  const [stopped, setStopped] = useState(false);
  const [requesting, setRequesting] = useState(false);
  const [feedback, setFeedback] = useState("");
  const [busy, setBusy] = useState(false);
  const latest = job.runs.at(-1);
  const prURL = (result || latest)?.summary?.match(
    /https:\/\/github\.com\/[\w.-]+\/[\w.-]+\/pull\/\d+/,
  )?.[0];
  const action = async (name) => {
    setBusy(true);
    try {
      await onAction(
        job,
        name,
        stopped,
        name === "request_changes" ? feedback : "",
      );
    } finally {
      setBusy(false);
    }
  };
  const retry = ["blocked", "failed", "interrupted", "cancelled"].includes(
    job.state,
  );
  return (
    <div className="space-y-3">
      {prURL && (
        <Button asChild variant="outline">
          <a href={prURL} target="_blank" rel="noreferrer">
            Open PR
          </a>
        </Button>
      )}
      {job.state === "awaiting_approval" && (
        <div className="space-y-3">
          <div className="flex flex-wrap gap-2">
            <Button
              disabled={busy || requesting}
              onClick={() => action("approve")}
            >
              Approve and start {friendlyName(latest?.command).toLowerCase()}
            </Button>
            {latest?.reviewed_run_id && (
              <Button
                variant="outline"
                disabled={busy}
                onClick={() => setRequesting(true)}
              >
                Request changes
              </Button>
            )}
          </div>
          {requesting && (
            <div className="space-y-3">
              <label className="block">
                <span className="field-label">What needs to change?</span>
                <textarea
                  className="field-control min-h-24"
                  value={feedback}
                  onChange={(e) => setFeedback(e.target.value)}
                  maxLength={4000}
                  placeholder="Explain what to revise in the previous stage’s result."
                />
              </label>
              <p className="text-xs text-muted-foreground">
                You’ll review the revised result before continuing.
              </p>
              <div className="flex flex-wrap gap-2">
                <Button
                  disabled={busy || !feedback.trim()}
                  onClick={() => action("request_changes")}
                >
                  {busy ? "Submitting…" : "Send feedback and revise"}
                </Button>
                <Button
                  variant="ghost"
                  disabled={busy}
                  onClick={() => setRequesting(false)}
                >
                  Keep reviewing
                </Button>
              </div>
            </div>
          )}
        </div>
      )}
      {job.state === "blocked" && (
        <p className="text-sm text-muted-foreground">
          Update the issue or resolve the blocker, then retry this step.
        </p>
      )}
      {["interrupted", "cancelled"].includes(job.state) && (
        <label className="flex items-start gap-2 text-sm">
          <input
            type="checkbox"
            checked={stopped}
            onChange={(event) => setStopped(event.target.checked)}
          />
          I have verified the previous worker process has stopped. Retrying will
          inspect existing work before continuing.
        </label>
      )}
      {["queued", "running", "awaiting_approval", "blocked", "interrupted"].includes(
        job.state,
      ) && (
        <Button
          variant="ghost"
          className="text-muted-foreground hover:text-danger"
          disabled={busy}
          onClick={() => action("cancel")}
        >
          Cancel run
        </Button>
      )}
      {retry && (
        <Button
          disabled={
            busy ||
            (["interrupted", "cancelled"].includes(job.state) && !stopped)
          }
          onClick={() => action("retry")}
        >
          Retry {friendlyName(latest?.command).toLowerCase()}
        </Button>
      )}
    </div>
  );
}

function resultTitle(job, result) {
  const command = friendlyName(job.runs.at(-1)?.command);
  switch (job.state) {
    case "awaiting_approval":
      return result
        ? `${friendlyName(result.command)} ready for review`
        : `Ready to start ${command.toLowerCase()}`;
    case "running":
      return `${command} in progress`;
    case "queued":
      return `${command} queued`;
    case "succeeded":
      return "Run complete";
    default:
      return `${command} · ${stateLabel(job.state)}`;
  }
}
