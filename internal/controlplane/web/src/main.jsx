import { TaskDetail } from "./task-detail.jsx";
import { State, friendlyName, relativeTime, stateLabel } from "./task-display.jsx";
import React, { useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import "@fontsource-variable/manrope";
import { Activity, BarChart3, Bot, Factory, GitBranch, LayoutDashboard, Moon, Play, Plus, Server, Settings, Sparkles, Sun, Table2, TimerReset, X } from "lucide-react";
import { Analytics } from "@/analytics";
import { CommandsPage, WorkersPage } from "@/catalog";
import { FactoryControls, FactoryPage } from "@/factory";
import { SettingsPage } from "@/settings";
import { SkillsPage } from "@/skills.jsx";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { PageHeading } from "@/components/ui/page-heading";
import { cn } from "@/lib/utils";
import { routeFromHash } from "@/routes";
import { attemptTime, boardCardTimestamp, boardColumns, boardDateFilters, boardFiltersFromHash, boardHash, canCancelJob, canMoveToQueue, collapseIssueAttempts, collapseJobsByTicket, currentRun, filterBoardJobs, filterJobsByDate, groupEntriesByBoardColumn, groupJobsByBoardColumn, jobCounts, jobDisplayTitle, resolutionLabel } from "@/runs-board";
import { groupEntriesByProject, groupJobsByProject } from "@/project";
import { concurrencyLabel } from "@/concurrency";
import { createStatusLoader } from "@/status-loader";
import { TriggersPage } from "@/triggers";
import "./styles.css";


function App() {
  const [status, setStatus] = useState({ jobs: [], workers: [], commands: [], repositories: [], triggers: [], csrf_token: "" });
  const [selection, setSelection] = useState("");
  const [repository, setRepository] = useState("");
  const [prompt, setPrompt] = useState("");
 const [title,setTitle]=useState("");
 const [sourceURL,setSourceURL]=useState("");
  const [model, setModel] = useState("");
  const [statusError, setStatusError] = useState("");
  const [statusLoaded, setStatusLoaded] = useState(false);
  const [submitError, setSubmitError] = useState("");
  const [taskActionError, setTaskActionError] = useState("");
  const [deletingJob, setDeletingJob] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [composerOpen, setComposerOpen] = useState(false);
  // Board filters restore from the URL hash query so refresh and
  // back/forward keep them; defaults are omitted from the URL.
  const [filters, setFilters] = useState(() => boardFiltersFromHash(window.location.hash));
  const [dark, setDark] = useState(() => localStorage.getItem("machinist-theme") !== "light");
  const [route, setRoute] = useState(() => routeFromHash(window.location.hash));
  const view = route.view;
  const statusLoader = useRef(null);
  const refreshStatus = () => statusLoader.current.refresh();
  if (!statusLoader.current) statusLoader.current = createStatusLoader({
    request: async () => {
      const response = await fetch("/api/v1/status", { headers: { Accept: "application/json" } });
      if (!response.ok) throw new Error(`Status request failed (${response.status})`);
      return response.json();
    },
    apply: (result) => {
      if (result.kind === "error") {
        setStatusError(result.message);
        return;
      }
      const next = result.status;
      setStatus(next);
      setStatusError("");
      setStatusLoaded(true);
      const available = new Set(selectionChoices(next).map((choice) => choice.value));
      setSelection((current) => available.has(current) ? current : available.has(localStorage.getItem("machinist-workflow")) ? localStorage.getItem("machinist-workflow") : firstSelection(next));
      const availableRepositories = next.repositories || [];
      setRepository((current) => availableRepositories.includes(current) ? current : availableRepositories.includes(localStorage.getItem("machinist-repository")) ? localStorage.getItem("machinist-repository") : availableRepositories[0] || "");
    },
  });

  useEffect(() => {
    document.documentElement.classList.toggle("dark", dark);
    localStorage.setItem("machinist-theme", dark ? "dark" : "light");
  }, [dark]);

  useEffect(() => {
    const updateView = () => {
      setTaskActionError("");
      setRoute(routeFromHash(window.location.hash));
    };
    window.addEventListener("hashchange", updateView);
    return () => window.removeEventListener("hashchange", updateView);
  }, []);

  // Board filters follow the URL: browser back/forward and hash edits restore them.
  useEffect(() => {
    if (view !== "runs") return;
    setFilters(boardFiltersFromHash(window.location.hash));
  }, [route]);

  useEffect(() => {
    let stopped = false;
    let timer;
    const load = async () => {
      await statusLoader.current.refresh();
      if (!stopped) timer = window.setTimeout(load, 2000);
    };
    load();
    return () => {
      stopped = true;
      statusLoader.current.cancel();
      window.clearTimeout(timer);
    };
  }, []);

  const choices = useMemo(() => selectionChoices(status), [status.commands, status.workflows]);

  const repositories = status.repositories;

  const displayJobs = useMemo(() => collapseIssueAttempts(status.jobs), [status.jobs]);
  const counts = useMemo(() => jobCounts(displayJobs), [displayJobs]);
  const visibleJobs = useMemo(() => filterBoardJobs(displayJobs, filters.showSucceeded), [filters.showSucceeded, displayJobs]);
  // Board: one card per Plane ticket (latest attempt); date/state filters apply to the latest.
  const boardEntries = useMemo(() => {
    const datedIds = new Set(filterJobsByDate(status.jobs, filters.dateFilter).map((job) => job.id));
    return collapseJobsByTicket(status.jobs).filter((entry) =>
      datedIds.has(entry.latest.id) && filterBoardJobs([entry.latest], filters.showSucceeded).length > 0);
  }, [filters.dateFilter, filters.showSucceeded, status.jobs]);

  const connectedWorkers = status.workers.filter((worker) => worker.connected).length;
  const selectedJob = route.jobID ? status.jobs.find((job) => job.id === route.jobID) : undefined;
  // ?attempt=<jobID> switches the page to that attempt's job; a missing or
  // stale param falls back to the job in the URL.
  const attemptJob = route.attempt ? status.jobs.find((job) => job.id === route.attempt) : undefined;
  const displayJob = attemptJob || selectedJob;

  async function submit(event) {
    event.preventDefault();
    setSubmitting(true);
    setSubmitError("");
    try {
      const response = await fetch("/api/v1/jobs", {
        method: "POST",
        headers: { "Content-Type": "application/json", "X-Machinist-CSRF": status.csrf_token },
        body: JSON.stringify({ repository, model: model.trim(), ...(selection.startsWith("workflow:") ? { workflow: selection.slice(9),title: title || prompt.trim().split("\n")[0].slice(0,100),source_url:sourceURL || (/^https?:\/\/\S+$/.test(prompt.trim()) ? prompt.trim() : ""),spec:/^https?:\/\/\S+$/.test(prompt.trim()) ? "" : prompt } : { command: selection.slice(8),prompt }) }),
      });
      if (!response.ok) {
        const body = await response.json().catch(() => ({}));
        throw new Error(body.error || `Submission failed (${response.status})`);
      }
      localStorage.setItem("machinist-workflow",selection); localStorage.setItem("machinist-repository",repository);
      const created = await response.json();
      setPrompt(""); setTitle(""); setSourceURL("");
      setComposerOpen(false);
      await statusLoader.current.refresh();
      window.location.hash = `#/runs/${created.id}`;
    } catch (requestError) {
      setSubmitError(requestError.message);
    } finally {
      setSubmitting(false);
    }
  }

  async function workflowAction(job, action, stopped = false, feedback = "") {
    setTaskActionError("");
    try {
      const response = await fetch(`/api/v1/jobs/${encodeURIComponent(job.id)}/${action}`, {
        method: "POST", headers: { "Content-Type": "application/json", "X-Machinist-CSRF": status.csrf_token },
        body: JSON.stringify({ run_id: job.runs.at(-1)?.id, previous_process_stopped: stopped, feedback }),
      });
      if (!response.ok) { const body = await response.json().catch(() => ({})); throw new Error(body.error || "Unable to update job"); }
      await statusLoader.current.refresh();
    } catch (error) { setTaskActionError(error.message); }
  }

  async function cancelJob(job) {
    setTaskActionError("");
    try {
      const response = await fetch(`/api/v1/jobs/${encodeURIComponent(job.id)}/cancel`, {
        method: "POST", headers: { "X-Machinist-CSRF": status.csrf_token },
      });
      if (!response.ok) { const body = await response.json().catch(() => ({})); throw new Error(body.error || "Unable to cancel job"); }
      await statusLoader.current.refresh();
    } catch (error) { setTaskActionError(error.message); }
  }

  // Move to queue (MACH-66): one button stops the current attempt and queues
  // the same job again as the next attempt on the same card. It asks first,
  // because it stops a running agent, and says what happens to the work.
  async function moveJobToQueue(job) {
    const title = jobDisplayTitle(job);
    if (!window.confirm(`Move ${title} back to the queue?\n\nThis stops the current attempt, saves its uncommitted work in the checkout, and queues the same job again as the next attempt.`)) return;
    setTaskActionError("");
    try {
      const response = await fetch(`/api/v1/jobs/${encodeURIComponent(job.id)}/move-to-queue`, {
        method: "POST", headers: { "X-Machinist-CSRF": status.csrf_token },
      });
      if (!response.ok) { const body = await response.json().catch(() => ({})); throw new Error(body.error || "Unable to move the job to the queue"); }
      await statusLoader.current.refresh();
    } catch (error) { setTaskActionError(error.message); }
  }

  async function deleteJob(job) {
    if (!window.confirm(`Delete task ${shortId(job.id)} and all of its stored run data?`)) return;
    setDeletingJob(job.id);
    setTaskActionError("");
    try {
      const response = await fetch(`/api/v1/jobs/${encodeURIComponent(job.id)}`, {
        method: "DELETE",
        headers: { "X-Machinist-CSRF": status.csrf_token },
      });
      if (!response.ok) {
        const body = await response.json().catch(() => ({}));
        throw new Error(body.error || `Delete failed (${response.status})`);
      }
      await statusLoader.current.refresh();
      window.location.hash = "#/runs";
    } catch (requestError) {
      setTaskActionError(requestError.message);
    } finally {
      setDeletingJob("");
    }
  }

  function taskHash(jobID, tab, attempt) {
    const params = new URLSearchParams();
    if (tab && tab !== "transcript") params.set("tab", tab);
    if (attempt && attempt !== jobID) params.set("attempt", attempt);
    const query = params.toString();
    return `#/runs/${encodeURIComponent(jobID)}${query ? `?${query}` : ""}`;
  }

  function selectTaskTab(next) {
    const hash = taskHash(route.jobID, next, route.attempt);
    window.history.replaceState(null, "", hash);
    setRoute({ ...route, tab: next });
  }

  function selectTaskAttempt(next) {
    const hash = taskHash(route.jobID, route.tab, next);
    window.history.replaceState(null, "", hash);
    setRoute({ ...route, attempt: next });
  }

  // Filter changes update the URL hash (a history entry, so back/forward work)
  // and the state immediately; the hashchange listener then confirms them.
  function updateBoardFilters(next) {
    const merged = { ...filters, ...next };
    setFilters(merged);
    window.location.hash = boardHash(merged);
  }

  return (
    <div className="app-shell min-h-screen bg-background text-foreground md:flex">
      <aside className="app-sidebar sticky top-0 z-20 flex shrink-0 items-center border-b border-border bg-sidebar px-3 py-2 md:h-screen md:w-56 md:flex-col md:items-stretch md:border-b-0 md:border-r md:px-4 md:py-5">
        <div className="brand-lockup flex h-10 items-center px-1">
          <a href="#/runs" className="brand-logo-link" aria-label="Oh My Machinist home">
            <img src="/logo-light-transparent.png" alt="oh my machinist" className="brand-logo brand-logo-light" width="116" height="30" />
            <img src="/logo-dark-transparent.png" alt="oh my machinist" className="brand-logo brand-logo-dark" width="115" height="30" />
          </a>
        </div>
        <nav className="ml-4 flex flex-1 gap-1 overflow-x-auto md:ml-0 md:mt-9 md:block md:overflow-visible" aria-label="Primary">
          <a href="#/runs" aria-current={view === "runs" || view === "task" ? "page" : undefined} className={cn("nav-item", (view === "runs" || view === "task") && "nav-item-active")}><Activity className="size-4" /><span>Tasks</span><span className="ml-auto text-xs text-muted-foreground">{counts.all}</span></a>
          <a href="#/analytics" aria-current={view === "analytics" ? "page" : undefined} className={cn("nav-item", view === "analytics" && "nav-item-active")}><BarChart3 className="size-4" /><span>Analytics</span></a>
          <a href="#/workers" aria-current={view === "workers" ? "page" : undefined} className={cn("nav-item", view === "workers" && "nav-item-active")}><Server className="size-4" /><span>Workers</span></a>
          <a href="#/triggers" aria-current={view === "triggers" ? "page" : undefined} className={cn("nav-item", view === "triggers" && "nav-item-active")}><TimerReset className="size-4" /><span>Triggers</span><span className="ml-auto text-xs text-muted-foreground">{status.triggers?.length || 0}</span></a>
          <a href="#/factory" aria-current={view === "factory" ? "page" : undefined} className={cn("nav-item", view === "factory" && "nav-item-active")}><Factory className="size-4" /><span>Factory</span></a>
          <a href="#/workflows" aria-current={["commands", "workflows"].includes(view) ? "page" : undefined} className={cn("nav-item", ["commands", "workflows"].includes(view) && "nav-item-active")}><Bot className="size-4" /><span>Workflows</span></a>
          <a href="#/skills" aria-current={view === "skills" ? "page" : undefined} className={cn("nav-item", view === "skills" && "nav-item-active")}><Sparkles className="size-4" /><span>Skills</span></a>
          <a href="#/settings" aria-current={view === "settings" ? "page" : undefined} className={cn("nav-item", view === "settings" && "nav-item-active")}><Settings className="size-4" /><span>Settings</span></a>
        </nav>
        <div className="hidden border-t border-border pt-3 md:block">
          <div className="nav-item" title={`Workers are connected lanes that can take a job. The limit is how many jobs may run at once. ${connectedWorkers} connected · ${status.workers.length} registered`}><Server className="size-4 shrink-0" /><span className="min-w-0 truncate whitespace-nowrap">{`${concurrencyLabel(status.max_concurrent_jobs, status.jobs.filter((job) => job.state === "running").length)} · ${connectedWorkers ? `${connectedWorkers} worker${connectedWorkers === 1 ? "" : "s"} online` : "No workers online"}`}</span></div>
          <div className="px-2.5 pb-1 pt-2"><FactoryControls factory={status.factory} csrfToken={status.csrf_token} onChanged={refreshStatus} compact /></div>
          <button onClick={() => setDark((value) => !value)} className="nav-item w-full" aria-label={`Switch to ${dark ? "light" : "dark"} theme`}>
            {dark ? <Moon className="size-4" /> : <Sun className="size-4" />}<span>{dark ? "Dark" : "Light"} theme</span>
          </button>
        </div>
        <button onClick={() => setDark((value) => !value)} className="mobile-theme ml-auto grid size-9 place-items-center text-muted-foreground md:hidden" aria-label={`Switch to ${dark ? "light" : "dark"} theme`}>
          {dark ? <Moon className="size-4" /> : <Sun className="size-4" />}
        </button>
      </aside>

      <main className="workshop min-w-0 flex-1">
        {view === "task" ? <TaskDetail csrfToken={status.csrf_token} job={displayJob} jobs={status.jobs} loaded={statusLoaded} error={statusError || taskActionError} deleting={deletingJob === route.jobID} onDelete={deleteJob} onWorkflowAction={workflowAction} onCancelJob={cancelJob} onMoveToQueue={moveJobToQueue} onAttemptChange={selectTaskAttempt} tab={route.tab} onTabChange={selectTaskTab} /> : view === "analytics" ? <Analytics jobs={status.jobs} loaded={statusLoaded} error={statusError} /> : view === "workers" ? <WorkersPage workers={status.workers} loaded={statusLoaded} error={statusError} /> : view === "factory" ? <FactoryPage factory={status.factory} csrfToken={status.csrf_token} onChanged={refreshStatus} loaded={statusLoaded} error={statusError} /> : view === "triggers" ? <TriggersPage triggers={status.triggers || []} loaded={statusLoaded} error={statusError} /> : view === "skills" ? <SkillsPage csrfToken={status.csrf_token} /> : view === "settings" ? <SettingsPage repositories={status.repositories || []} csrfToken={status.csrf_token} /> : ["commands", "workflows"].includes(view) ? <CommandsPage /> : <div className="mx-auto max-w-[1500px] space-y-6 p-4 sm:p-6 lg:p-8">
          <PageHeading title="Tasks" description="Describe the work. Review the result.">
            <div className="flex items-center gap-2">
              <Button className="text-xs!" onClick={() => setComposerOpen(true)}><Plus className="size-4" />New task</Button>
            </div>
          </PageHeading>

          {composerOpen && <RunComposer title={title} setTitle={setTitle} sourceURL={sourceURL} setSourceURL={setSourceURL} choices={choices} repositories={repositories} selection={selection} setSelection={setSelection} repository={repository} setRepository={setRepository} prompt={prompt} setPrompt={setPrompt} model={model} setModel={setModel} submitting={submitting} submit={submit} close={() => setComposerOpen(false)} />}
          {(statusError || submitError || taskActionError) && <div role="alert" className="rounded-md border border-danger/35 bg-danger/10 px-3 py-2 text-sm text-danger">{submitError || statusError || taskActionError}</div>}

          <section>
            <div className="mb-3 flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
              <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
                <input type="checkbox" className="size-3.5 accent-primary" checked={filters.showSucceeded} onChange={(event) => updateBoardFilters({ showSucceeded: event.target.checked })} />
                Show succeeded
              </label>
              <div className="flex flex-wrap items-center justify-between gap-3 lg:justify-end">
                <p className="text-xs text-muted-foreground">{counts.all} task{counts.all === 1 ? "" : "s"}</p>
                <div className="inline-flex rounded-lg bg-muted/60 p-1" role="group" aria-label="Runs view">
                  <Button variant="ghost" size="sm" className={cn("h-7 border-transparent px-2.5 text-xs!", filters.runsView === "board" && "bg-surface text-foreground shadow-xs")} aria-pressed={filters.runsView === "board"} onClick={() => updateBoardFilters({ runsView: "board" })}><LayoutDashboard className="size-3.5" />Board</Button>
                  <Button variant="ghost" size="sm" className={cn("h-7 border-transparent px-2.5 text-xs!", filters.runsView === "table" && "bg-surface text-foreground shadow-xs")} aria-pressed={filters.runsView === "table"} onClick={() => updateBoardFilters({ runsView: "table" })}><Table2 className="size-3.5" />List</Button>
                </div>
                {filters.runsView === "board" && <div className="inline-flex rounded-lg bg-muted/60 p-1" role="group" aria-label="Board date filter">
                  {boardDateFilters.map(({ id, label }) => (
                    <Button key={id} variant="ghost" size="sm" className={cn("h-7 border-transparent px-2.5 text-xs!", filters.dateFilter === id && "bg-surface text-foreground shadow-xs")} aria-pressed={filters.dateFilter === id} onClick={() => updateBoardFilters({ dateFilter: id })}>{label}</Button>
                  ))}
                </div>}
                <div className="inline-flex rounded-lg bg-muted/60 p-1" role="group" aria-label="Board layout">
                  <Button variant="ghost" size="sm" className={cn("h-7 border-transparent px-2.5 text-xs!", filters.boardLayout === "all" && "bg-surface text-foreground shadow-xs")} aria-pressed={filters.boardLayout === "all"} onClick={() => updateBoardFilters({ boardLayout: "all" })}>All</Button>
                  <Button variant="ghost" size="sm" className={cn("h-7 border-transparent px-2.5 text-xs!", filters.boardLayout === "projects" && "bg-surface text-foreground shadow-xs")} aria-pressed={filters.boardLayout === "projects"} onClick={() => updateBoardFilters({ boardLayout: "projects" })}>By project</Button>
                </div>
              </div>
            </div>

            {filters.runsView === "board" ? (filters.boardLayout === "projects" ? <ProjectRunBoard entries={boardEntries} onCancelJob={cancelJob} onMoveToQueue={moveJobToQueue} /> : <RunBoard entries={boardEntries} onCancelJob={cancelJob} onMoveToQueue={moveJobToQueue} />) : <Card className="overflow-hidden">
              {visibleJobs.length ? visibleJobs.map((job) => <RunRow key={job.id} job={job} onCancelJob={cancelJob} onMoveToQueue={moveJobToQueue} />) : <EmptyRuns filtered={!filters.showSucceeded} openComposer={() => setComposerOpen(true)} />}
            </Card>}
          </section>

        </div>}
      </main>
    </div>
  );
}

function RunComposer({ title,setTitle,sourceURL,setSourceURL,choices,repositories,selection,setSelection,repository,setRepository,prompt,setPrompt,model,setModel,submitting,submit,close }) {
  const specHintID=React.useId();
  const isTask=selection.startsWith("workflow:");
  return <Card className="overflow-hidden border-primary/25">
    <div className="flex items-center justify-between border-b border-border px-5 py-3"><h2 className="text-sm font-semibold">New task</h2><Button variant="ghost" size="icon" onClick={close} aria-label="Close new task form"><X className="size-4" /></Button></div>
    <form onSubmit={submit} className="space-y-5 p-5">
      <label className="block space-y-2"><span className="text-sm font-medium">What do you want done?</span><textarea autoFocus aria-describedby={specHintID} className="field-control min-h-32 resize-y" value={prompt} onChange={e=>setPrompt(e.target.value)} placeholder="Describe a change or paste an issue link…" required={!sourceURL.trim() || !isTask} /></label>
      <p id={specHintID} className="text-xs leading-5 text-muted-foreground">{isTask ? <>This is your task’s spec: describe what to build and what counts as done. Prompt templates reference it as <code>{"{{task.spec}}"}</code>. A link on its own is saved as the source instead. <a href="#/workflows" className="text-primary underline">Template variables</a></> : "Write the instructions for this command. Its prompt template receives them as {{machinist.prompt}}."}</p>
      <section className="text-sm">
        <div className="grid gap-4 sm:grid-cols-2">
          <label><span className="field-label">{isTask ? "Workflow" : "Command"}</span><select className="field-control" value={selection} onChange={e=>setSelection(e.target.value)} required>{choices.map(c=><option key={c.value} value={c.value}>{c.label}</option>)}</select></label>
          <label><span className="field-label">Repository</span><select className="field-control" value={repository} onChange={e=>setRepository(e.target.value)} required>{!repositories.length && <option value="">No repositories available</option>}{repositories.map(r=><option key={r} value={r}>{r}</option>)}</select></label>
          {isTask && <><label><span className="field-label">Title · optional</span><input className="field-control" value={title} onChange={e=>setTitle(e.target.value)} maxLength={512} placeholder="From your instructions by default" /><span className="mt-1 block text-xs text-muted-foreground">Template: <code>{"{{task.title}}"}</code></span></label><label><span className="field-label">Source link · optional</span><input type="url" className="field-control" value={sourceURL} onChange={e=>setSourceURL(e.target.value)} placeholder="https://github.com/…" /><span className="mt-1 block text-xs text-muted-foreground">Template: <code>{"{{task.source_url}}"}</code></span></label></>}
          <label><span className="field-label">Model · optional</span><input className="field-control" value={model} onChange={e=>setModel(e.target.value)} maxLength={128} placeholder="Workflow default" /></label>
        </div>
      </section>
      {!choices.length && <p role="alert" className="text-sm text-danger">Configure a workflow before starting a task.</p>}
      <div className="flex justify-end"><Button disabled={submitting || !selection || !repository}>{submitting ? "Starting…" : "Start task"}<Play className="size-3.5" /></Button></div>
    </form>
  </Card>;
}

function RunBoard({ entries, onCancelJob, onMoveToQueue }) {
  const groupedEntries = groupEntriesByBoardColumn(entries);
  return <div className="grid min-w-0 gap-4 md:grid-cols-2 xl:grid-cols-4">
    {boardColumns.map((column) => <RunColumn key={column.id} column={column} entries={groupedEntries[column.id]} idPrefix="board" onCancelJob={onCancelJob} onMoveToQueue={onMoveToQueue} />)}
  </div>;
}

function ProjectRunBoard({ entries, onCancelJob, onMoveToQueue }) {
  const projectGroups = groupEntriesByProject(entries);
  return <div className="grid min-w-0 gap-6">
    {projectGroups.map(([project, projectEntries]) => {
      const groupedEntries = groupEntriesByBoardColumn(projectEntries);
      return <section key={project} className="min-w-0" aria-labelledby={`project-${project}`}>
        <header className="mb-2 flex items-center justify-between gap-3">
          <h2 id={`project-${project}`} className="text-base font-semibold">{project}</h2>
          <Badge className="shrink-0 border-border bg-surface text-muted-foreground" aria-label={`${projectEntries.length} visible ${project} runs`}>{projectEntries.length}</Badge>
        </header>
        <div className="grid min-w-0 gap-4 md:grid-cols-2 xl:grid-cols-4">
          {boardColumns.map((column) => <RunColumn key={column.id} column={column} entries={groupedEntries[column.id]} idPrefix={`project-${project}`} onCancelJob={onCancelJob} onMoveToQueue={onMoveToQueue} />)}
        </div>
      </section>;
    })}
  </div>;
}

function RunColumn({ column, entries, idPrefix, onCancelJob, onMoveToQueue }) {
  return <section className="run-column min-w-0 border border-border bg-muted/20" aria-labelledby={`${idPrefix}-${column.id}`}>
    <header className="flex items-center justify-between gap-3 border-b border-border px-3 py-2.5">
      <div className="min-w-0"><h2 id={`${idPrefix}-${column.id}`} className="text-sm font-semibold">{column.title}</h2><p className="break-words text-xs text-muted-foreground">{column.description}</p></div>
      <Badge className="shrink-0 border-border bg-surface text-muted-foreground" aria-label={`${entries.length} visible ${column.title.toLowerCase()} runs`}>{entries.length}</Badge>
    </header>
    <div className="grid min-w-0 gap-2 p-2">
      {entries.length ? entries.map((entry) => <RunCard key={entry.latest.id} job={entry.latest} priors={entry.priors} onCancelJob={onCancelJob} onMoveToQueue={onMoveToQueue} />) : <p className="px-2 py-8 text-center text-xs text-muted-foreground">No runs</p>}
    </div>
  </section>;
}

function attemptBadges(job) {
  if (!job.attempt_count || job.attempt_count < 2) return null;
  return <div className="flex flex-wrap items-center gap-1.5">
    <Badge className="border-border bg-surface text-muted-foreground">attempt {job.attempt_count}</Badge>
    {job.prior_failed_count > 0 && <Badge className="border-danger/25 bg-danger/10 text-danger">{job.prior_failed_count === 1 ? "1 prior fail" : `${job.prior_failed_count} prior fails`}</Badge>}
  </div>;
}

// MoveToQueueButton stops the current attempt and queues the job again as the
// next attempt on the same card (MACH-66). The confirm lives in the handler,
// so every surface asks the same question.
function MoveToQueueButton({ job, onMoveToQueue }) {
  const [busy, setBusy] = useState(false);
  return <Button size="sm" variant="outline" disabled={busy} aria-label={`Move task ${jobDisplayTitle(job)} back to the queue`}
    onClick={async () => { setBusy(true); try { await onMoveToQueue(job); } finally { setBusy(false); } }}>
    {busy ? "Moving…" : "Move to queue"}
  </Button>;
}

function RunCard({ job, priors = [], onCancelJob, onMoveToQueue }) {
  const title = jobDisplayTitle(job);
  const run = currentRun(job);
  const timestamp = boardCardTimestamp(job);
  const resolution = resolutionLabel(job);
  const attemptCount = priors.length > 0 ? priors.length + 1 : (job.attempt_count || 0);
  return <Card className="overflow-hidden"><a href={`#/runs/${encodeURIComponent(job.id)}`} className="block min-w-0 space-y-3 p-4 transition hover:bg-muted/35 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring/50" aria-label={`Open task ${title}`}>
      <p className="break-words text-sm font-medium leading-5" title={title}>{title}</p>
      <p className="text-xs text-muted-foreground">{job.repository} · {friendlyName(run?.command || job.command)}</p>
      <div className="flex flex-wrap items-center justify-between gap-2">
        <State value={job.state} />
        <time className="text-xs text-muted-foreground" dateTime={timestamp} title={timestamp ? new Date(timestamp).toLocaleString() : undefined}>{relativeTime(timestamp)}</time>
      </div>
      {resolution && <p className="text-xs text-success">{resolution}</p>}
      {priors.length === 0 && attemptBadges(job)}
    </a>{priors.length > 0 && <div className="flex flex-wrap items-center gap-1.5 border-t border-border px-4 py-2.5">
      <Badge className="border-border bg-surface text-muted-foreground" aria-label={`${attemptCount} attempts`}>{attemptCount} attempts</Badge>
      {priors.map((prior) => <a key={prior.id} href={`#/runs/${encodeURIComponent(prior.id)}`} className="text-xs text-muted-foreground underline hover:text-foreground" aria-label={`Open prior attempt ${stateLabel(prior.state)} ${relativeTime(attemptTime(prior))}`}>{stateLabel(prior.state)} · {relativeTime(attemptTime(prior))}</a>)}
    </div>}{canCancelJob(job) && <div className="flex justify-end px-4 pb-4"><Button size="sm" variant="outline" aria-label={`Cancel task ${title}`} onClick={() => onCancelJob(job)}>Cancel</Button></div>}{canMoveToQueue(job) && <div className="flex justify-end px-4 pb-4"><MoveToQueueButton job={job} onMoveToQueue={onMoveToQueue} /></div>}</Card>;
}

function RunRow({ job, onCancelJob, onMoveToQueue }) {
  const title=jobDisplayTitle(job);
  const run=currentRun(job);
  const resolution=resolutionLabel(job);
  return <article className="border-b border-border last:border-b-0"><a href={`#/runs/${encodeURIComponent(job.id)}`} className="flex items-center justify-between gap-4 px-5 py-4 hover:bg-muted/35" aria-label={`Open task ${title}`}><div className="min-w-0"><p className="break-words text-sm font-medium" title={title}>{title}</p><p className="mt-1 text-xs text-muted-foreground">{job.repository} · {friendlyName(run?.command || job.command)}</p>{resolution && <p className="mt-1 text-xs text-success">{resolution}</p>}{attemptBadges(job)}</div><div className="flex shrink-0 flex-col items-end gap-1"><State value={job.state} /><time className="text-xs text-muted-foreground" dateTime={job.created_at}>{relativeTime(job.created_at)}</time>{canCancelJob(job) && <Button size="sm" variant="outline" aria-label={`Cancel task ${title}`} onClick={() => onCancelJob(job)}>Cancel</Button>}{canMoveToQueue(job) && <MoveToQueueButton job={job} onMoveToQueue={onMoveToQueue} />}</div></a></article>;
}

function EmptyRuns({ filtered, openComposer }) {
  return <div className="grid place-items-center px-6 py-16 text-center"><span className="grid size-10 place-items-center rounded-full bg-muted text-muted-foreground"><GitBranch className="size-5" /></span><h3 className="mt-3 text-sm font-semibold">{filtered ? "No matching tasks" : "No tasks yet"}</h3><p className="mt-1 max-w-sm text-xs leading-5 text-muted-foreground">{filtered ? "Try a different state filter." : "Describe the work or paste an issue link to get started."}</p>{!filtered && <Button variant="outline" size="sm" className="mt-4" onClick={openComposer}><Plus className="size-3.5" />New task</Button>}</div>;
}


function selectionChoices(status) {
 const workflows=status.workflows || [];
 return workflows.length ? workflows.map(name=>({value:`workflow:${name}`,label:friendlyName(name)})) : (status.commands || []).map(name=>({value:`command:${name}`,label:friendlyName(name)}));
}
function firstSelection(status) { return selectionChoices(status)[0]?.value || ""; }
function shortId(id) { const [, value = id] = id.split("_", 2); return value.slice(0, 8); }
export const appRoot = createRoot(document.getElementById("root"));
appRoot.render(<App />);
