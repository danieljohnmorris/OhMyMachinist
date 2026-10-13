import React, { useCallback, useEffect, useMemo, useState } from "react";
import { Plus, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { PageHeading, QuietState } from "@/components/ui/page-heading";
import { formatTimestamp, friendlyName, relativeTime } from "@/task-display";
import {
  backlogProjectOptions,
  backlogStateLabel,
  backlogStates,
  createIssuePayload,
  filterBacklogIssues,
  groupIssuesByState,
  issueUpdatePayload,
  startRunPayload,
} from "@/backlog-state";

const POLL_MILLIS = 5000;

async function backlogRequest(path, { method = "GET", csrfToken = "", body } = {}) {
  const response = await fetch(path, {
    method,
    headers: {
      Accept: "application/json",
      ...(body ? { "Content-Type": "application/json", "X-Machinist-CSRF": csrfToken } : {}),
    },
    ...(body ? { body: JSON.stringify(body) } : {}),
  });
  if (!response.ok) {
    const failure = await response.json().catch(() => ({}));
    throw new Error(failure.error || `Backlog request failed (${response.status})`);
  }
  return response.status === 204 ? null : response.json();
}

function issueList(data) {
  if (Array.isArray(data)) return data;
  return Array.isArray(data?.issues) ? data.issues : [];
}

// Deep links look like #/backlog?issue=<id>; anything else yields "".
function issueParam(hash) {
  const query = typeof hash === "string" ? hash.split("?")[1] || "" : "";
  try {
    return new URLSearchParams(query).get("issue") || "";
  } catch {
    return "";
  }
}

export function BacklogPage({ csrfToken, repositories = [], workflows = [], issueID = "" }) {
  const [issues, setIssues] = useState([]);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [actionError, setActionError] = useState("");
  const [projectFilter, setProjectFilter] = useState("");
  const [query, setQuery] = useState("");
  const [composerOpen, setComposerOpen] = useState(false);
  const [drawerID, setDrawerID] = useState("");

  // Deep links (#/backlog?issue=<id>) open the drawer; route changes update it.
  useEffect(() => {
    setDrawerID(issueID || "");
  }, [issueID]);

  useEffect(() => {
    let stopped = false;
    let timer;
    const load = async () => {
      try {
        const data = await backlogRequest("/api/v1/backlog");
        if (!stopped) {
          setIssues(issueList(data));
          setLoaded(true);
          setError("");
        }
      } catch (requestError) {
        if (!stopped) setError(requestError.message);
      } finally {
        if (!stopped) timer = window.setTimeout(load, POLL_MILLIS);
      }
    };
    load();
    return () => {
      stopped = true;
      window.clearTimeout(timer);
    };
  }, []);

  const projectOptions = useMemo(() => backlogProjectOptions(issues, repositories), [issues, repositories]);
  const visibleIssues = useMemo(
    () => filterBacklogIssues(issues, { project: projectFilter, query }),
    [issues, projectFilter, query],
  );
  const grouped = useMemo(() => groupIssuesByState(visibleIssues), [visibleIssues]);
  const drawerIssue = issues.find((issue) => issue.id === drawerID);

  const upsertIssue = useCallback((issue) => {
    if (!issue?.id) return;
    setIssues((current) => {
      const index = current.findIndex((candidate) => candidate.id === issue.id);
      if (index === -1) return [issue, ...current];
      const next = [...current];
      next[index] = issue;
      return next;
    });
  }, []);

  async function createIssue(payload) {
    setActionError("");
    const created = await backlogRequest("/api/v1/backlog", { method: "POST", csrfToken, body: payload });
    setIssues((current) => [created, ...current]);
    setComposerOpen(false);
  }

  async function saveIssue(issue, payload) {
    setActionError("");
    const updated = await backlogRequest(`/api/v1/backlog/${encodeURIComponent(issue.id)}`, {
      method: "PATCH",
      csrfToken,
      body: payload,
    });
    upsertIssue(updated);
  }

  async function startIssue(issue, payload) {
    setActionError("");
    const result = await backlogRequest(`/api/v1/backlog/${encodeURIComponent(issue.id)}/start`, {
      method: "POST",
      csrfToken,
      body: payload,
    });
    if (result?.issue) upsertIssue(result.issue);
    return result?.job_id || "";
  }

  return (
    <div className="mx-auto max-w-[1500px] space-y-6 p-4 sm:p-6 lg:p-8">
      <PageHeading
        title="Backlog"
        description="Native issue tracking. Saving never starts a run — starting one is a separate action."
      >
        <Button className="text-xs!" onClick={() => setComposerOpen(true)}>
          <Plus className="size-4" />
          New issue
        </Button>
      </PageHeading>

      {(error || actionError) && (
        <div role="alert" className="rounded-md border border-danger/35 bg-danger/10 px-3 py-2 text-sm text-danger">
          {error || actionError}
        </div>
      )}

      {composerOpen && <IssueComposer projectOptions={projectOptions} close={() => setComposerOpen(false)} create={createIssue} />}

      <div className="flex flex-wrap items-center gap-3">
        <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
          Project
          <select
            className="field-control h-8 w-auto text-xs"
            value={projectFilter}
            onChange={(event) => setProjectFilter(event.target.value)}
          >
            <option value="">All projects</option>
            {projectOptions.map((project) => (
              <option key={project} value={project}>
                {project}
              </option>
            ))}
          </select>
        </label>
        <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
          Search
          <input
            className="field-control h-8 w-48 text-xs"
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="Key, title, or spec"
          />
        </label>
        <p className="ml-auto text-xs text-muted-foreground">
          {visibleIssues.length} issue{visibleIssues.length === 1 ? "" : "s"}
        </p>
      </div>

      {!loaded ? (
        <Card>
          <QuietState title="Loading the backlog" description="Fetching issues." role="status" />
        </Card>
      ) : !issues.length ? (
        <Card>
          <QuietState title="No issues yet" description="Create the first issue to plan work for your repositories." />
        </Card>
      ) : (
        <div className="grid min-w-0 gap-4 md:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-5">
          {backlogStates.map((state) => (
            <BacklogColumn key={state} state={state} issues={grouped[state]} />
          ))}
        </div>
      )}

      {drawerIssue && (
        <IssueDrawer
          issue={drawerIssue}
          workflows={workflows}
          repositories={repositories}
          onClose={() => {
            setDrawerID("");
            if (issueParam(window.location.hash)) window.location.hash = "#/backlog";
          }}
          onSave={saveIssue}
          onStart={startIssue}
        />
      )}
    </div>
  );
}

function IssueComposer({ projectOptions, close, create }) {
  const [project, setProject] = useState("");
  const [title, setTitle] = useState("");
  const [spec, setSpec] = useState("");
  const [url, setURL] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const listID = React.useId();

  async function submit(event) {
    event.preventDefault();
    setSubmitting(true);
    setError("");
    try {
      await create(createIssuePayload({ project, title, spec, url }));
    } catch (requestError) {
      setError(requestError.message);
      setSubmitting(false);
    }
  }

  return (
    <Card className="overflow-hidden border-primary/25">
      <div className="flex items-center justify-between border-b border-border px-5 py-3">
        <h2 className="text-sm font-semibold">New issue</h2>
        <Button variant="ghost" size="icon" onClick={close} aria-label="Close new issue form">
          <X className="size-4" />
        </Button>
      </div>
      <form onSubmit={submit} className="space-y-5 p-5">
        <div className="grid gap-4 sm:grid-cols-2">
          <label>
            <span className="field-label">Project</span>
            <input
              className="field-control"
              list={listID}
              value={project}
              onChange={(event) => setProject(event.target.value)}
              placeholder="Repository or project key"
              required
            />
            <datalist id={listID}>
              {projectOptions.map((option) => (
                <option key={option} value={option} />
              ))}
            </datalist>
            <span className="mt-1 block text-xs text-muted-foreground">Issues get keys like PROJECT-1 per project.</span>
          </label>
          <label>
            <span className="field-label">Title</span>
            <input
              className="field-control"
              value={title}
              onChange={(event) => setTitle(event.target.value)}
              maxLength={512}
              required
            />
          </label>
        </div>
        <label className="block space-y-2">
          <span className="field-label">Spec · optional</span>
          <textarea
            className="field-control min-h-32 resize-y"
            value={spec}
            onChange={(event) => setSpec(event.target.value)}
            placeholder="Describe the work. Starting a run pins this spec into the job."
          />
        </label>
        <label className="block">
          <span className="field-label">External link · optional</span>
          <input
            type="url"
            className="field-control"
            value={url}
            onChange={(event) => setURL(event.target.value)}
            placeholder="https://…"
          />
        </label>
        {error && <p role="alert" className="text-sm text-danger">{error}</p>}
        <div className="flex items-center justify-between gap-3">
          <p className="text-xs text-muted-foreground">Saving never starts a run.</p>
          <Button disabled={submitting}>{submitting ? "Saving…" : "Create issue"}</Button>
        </div>
      </form>
    </Card>
  );
}

function BacklogColumn({ state, issues }) {
  return (
    <section className="run-column min-w-0 border border-border bg-muted/20" aria-labelledby={`backlog-${state}`}>
      <header className="flex items-center justify-between gap-3 border-b border-border px-3 py-2.5">
        <h2 id={`backlog-${state}`} className="text-sm font-semibold">
          {backlogStateLabel(state)}
        </h2>
        <Badge className="shrink-0 border-border bg-surface text-muted-foreground" aria-label={`${issues.length} ${backlogStateLabel(state).toLowerCase()} issues`}>
          {issues.length}
        </Badge>
      </header>
      <div className="grid min-w-0 gap-2 p-2">
        {issues.length ? (
          issues.map((issue) => <IssueCard key={issue.id} issue={issue} />)
        ) : (
          <p className="px-2 py-8 text-center text-xs text-muted-foreground">No issues</p>
        )}
      </div>
    </section>
  );
}

function IssueCard({ issue }) {
  return (
    <a
      href={`#/backlog?issue=${encodeURIComponent(issue.id)}`}
      className="block min-w-0 space-y-1.5 rounded-lg border border-border bg-surface p-3 transition hover:bg-muted/35 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring/50"
      aria-label={`Open issue ${issue.key}`}
    >
      <p className="break-words text-sm font-medium leading-5" title={issue.title}>
        {issue.title}
      </p>
      <p className="font-mono text-xs text-muted-foreground">
        {issue.key} · rev {issue.revision}
      </p>
      <time
        className="block text-xs text-muted-foreground"
        dateTime={issue.updated_at}
        title={formatTimestamp(issue.updated_at)}
      >
        {relativeTime(issue.updated_at)}
      </time>
    </a>
  );
}

function IssueDrawer({ issue, workflows, repositories, onClose, onSave, onStart }) {
  return (
    <div className="fixed inset-0 z-30 bg-background/60 backdrop-blur-sm" role="dialog" aria-modal="true" aria-label={`Issue ${issue.key}`}>
      <button className="absolute inset-0 cursor-default" onClick={onClose} aria-label="Close issue drawer" tabIndex={-1} />
      <Card className="relative z-10 ml-auto flex h-full w-full max-w-xl flex-col overflow-hidden rounded-none border-l">
        <header className="flex items-center justify-between gap-3 border-b border-border px-5 py-3">
          <div className="min-w-0">
            <h2 className="font-mono text-sm font-semibold">{issue.key}</h2>
            <p className="text-xs text-muted-foreground">
              {issue.project} · rev {issue.revision} · updated{" "}
              <time dateTime={issue.updated_at} title={formatTimestamp(issue.updated_at)}>
                {relativeTime(issue.updated_at)}
              </time>
            </p>
          </div>
          <Button variant="ghost" size="icon" onClick={onClose} aria-label="Close issue drawer">
            <X className="size-4" />
          </Button>
        </header>
        {/* Remount on revision bumps so saved server state resets the form. */}
        <IssueEditor key={`${issue.id}:${issue.revision}`} issue={issue} workflows={workflows} repositories={repositories} onSave={onSave} onStart={onStart} />
      </Card>
    </div>
  );
}

function IssueEditor({ issue, workflows, repositories, onSave, onStart }) {
  const [title, setTitle] = useState(issue.title);
  const [spec, setSpec] = useState(issue.spec || "");
  const [state, setState] = useState(issue.state);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState("");
  const [startOpen, setStartOpen] = useState(false);

  async function submit(event) {
    event.preventDefault();
    setSaving(true);
    setError("");
    setSaved(false);
    try {
      await onSave(issue, issueUpdatePayload({ title, spec, state }));
      setSaved(true);
    } catch (requestError) {
      setError(requestError.message);
    } finally {
      setSaving(false);
    }
  }

  const unchanged = title === issue.title && spec === (issue.spec || "") && state === issue.state;

  return (
    <div className="min-h-0 flex-1 space-y-6 overflow-y-auto p-5">
      <form onSubmit={submit} className="space-y-5">
        <label className="block space-y-2">
          <span className="field-label">Title</span>
          <input
            className="field-control"
            value={title}
            onChange={(event) => {
              setTitle(event.target.value);
              setSaved(false);
            }}
            maxLength={512}
            required
          />
        </label>
        <label className="block space-y-2">
          <span className="field-label">Spec</span>
          <textarea
            className="field-control min-h-40 resize-y"
            value={spec}
            onChange={(event) => {
              setSpec(event.target.value);
              setSaved(false);
            }}
          />
        </label>
        <label className="block">
          <span className="field-label">State</span>
          <select
            className="field-control"
            value={state}
            onChange={(event) => {
              setState(event.target.value);
              setSaved(false);
            }}
          >
            {backlogStates.map((option) => (
              <option key={option} value={option}>
                {backlogStateLabel(option)}
              </option>
            ))}
          </select>
        </label>
        {issue.url && (
          <p className="break-words text-xs text-muted-foreground">
            External link:{" "}
            <a className="text-primary underline" href={issue.url} target="_blank" rel="noopener noreferrer">
              {issue.url}
            </a>
          </p>
        )}
        {error && <p role="alert" className="text-sm text-danger">{error}</p>}
        {saved && !error && <p className="text-xs text-success">Saved. Nothing was enqueued.</p>}
        <div className="flex justify-end">
          <Button disabled={saving || unchanged}>{saving ? "Saving…" : "Save changes"}</Button>
        </div>
      </form>
      <div className="border-t border-border pt-5">
        {startOpen ? (
          <StartRunForm issue={issue} workflows={workflows} repositories={repositories} onStart={onStart} onClose={() => setStartOpen(false)} />
        ) : (
          <div className="flex items-center justify-between gap-3">
            <p className="text-xs text-muted-foreground">Starting a run pins this spec into a new job.</p>
            <Button variant="outline" onClick={() => setStartOpen(true)}>
              Start run
            </Button>
          </div>
        )}
      </div>
    </div>
  );
}

function StartRunForm({ issue, workflows, repositories, onStart, onClose }) {
  const [repository, setRepository] = useState(repositories[0] || "");
  const [workflow, setWorkflow] = useState(workflows[0] || "");
  const [command, setCommand] = useState("");
  const [model, setModel] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [jobID, setJobID] = useState("");
  const [error, setError] = useState("");

  async function submit(event) {
    event.preventDefault();
    setSubmitting(true);
    setError("");
    try {
      setJobID(await onStart(issue, startRunPayload({ repository, workflow, command, model })));
    } catch (requestError) {
      setError(requestError.message);
      setSubmitting(false);
    }
  }

  if (jobID) {
    return (
      <div className="space-y-2">
        <p className="text-sm font-medium text-success">Run started.</p>
        <a className="text-sm text-primary underline" href={`#/runs/${encodeURIComponent(jobID)}`}>
          View the job on the runs board
        </a>
      </div>
    );
  }

  return (
    <form onSubmit={submit} className="space-y-4">
      <h3 className="text-sm font-semibold">Start a run</h3>
      <div className="grid gap-4 sm:grid-cols-2">
        <label>
          <span className="field-label">Repository</span>
          {repositories.length ? (
            <select className="field-control" value={repository} onChange={(event) => setRepository(event.target.value)} required>
              {repositories.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </select>
          ) : (
            <input
              className="field-control"
              value={repository}
              onChange={(event) => setRepository(event.target.value)}
              placeholder="Repository"
              required
            />
          )}
        </label>
        <label>
          <span className="field-label">Workflow</span>
          {workflows.length ? (
            <select className="field-control" value={workflow} onChange={(event) => setWorkflow(event.target.value)} required>
              {workflows.map((name) => (
                <option key={name} value={name}>
                  {friendlyName(name)}
                </option>
              ))}
            </select>
          ) : (
            <input
              className="field-control"
              value={workflow}
              onChange={(event) => setWorkflow(event.target.value)}
              placeholder="Workflow name"
            />
          )}
        </label>
        {!workflows.length && (
          <label>
            <span className="field-label">Command · optional</span>
            <input className="field-control" value={command} onChange={(event) => setCommand(event.target.value)} />
          </label>
        )}
        <label>
          <span className="field-label">Model · optional</span>
          <input
            className="field-control"
            value={model}
            onChange={(event) => setModel(event.target.value)}
            maxLength={128}
            placeholder="Workflow default"
          />
        </label>
      </div>
      {error && <p role="alert" className="text-sm text-danger">{error}</p>}
      <div className="flex justify-end gap-2">
        <Button type="button" variant="outline" size="sm" onClick={onClose}>
          Cancel
        </Button>
        <Button disabled={submitting}>{submitting ? "Starting…" : "Start run"}</Button>
      </div>
    </form>
  );
}
