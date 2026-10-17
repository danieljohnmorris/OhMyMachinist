import { useEffect, useState } from "react";
import { ArrowLeft, ArrowUpRight, Check, Plus, Ticket, Trash2 } from "lucide-react";
import { Alert } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { PageHeading, QuietState } from "@/components/ui/page-heading";
import { approveTicket, canApproveTicket, commentOnTicket, createTicket, deleteTicket, getTicket, listTickets, loadTrackers, ticketStateLabel, ticketStateTone, updateTicket } from "./tickets-api.js";
import { cardLinks, trackerNames } from "./run-links.js";
import { relativeTime, State } from "./task-display.jsx";
import { linkifyText } from "./linkify.js";

export function TicketsPage({ csrfToken, repositories = [], jobs = [], ticketKey, onChanged }) {
  const [tickets, setTickets] = useState([]);
  const [trackers, setTrackers] = useState([]);
  const [projects, setProjects] = useState({});
  const [tracker, setTracker] = useState("");
  const [selected, setSelected] = useState(null);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [pending, setPending] = useState("");
  const [creating, setCreating] = useState(false);
  const [search, setSearch] = useState("");
  const [revision, setRevision] = useState(0);

  useEffect(() => {
    let cancelled = false;
    let timer;
    setLoaded(false);
    setSelected(null);
    if (ticketKey) setTracker("builtin");
    async function refresh() {
      const [items, providers, detail] = await Promise.allSettled([listTickets(), loadTrackers(), ticketKey ? getTicket(ticketKey) : Promise.resolve(null)]);
      if (cancelled) return;
      const failures = [items, providers, detail].filter(result => result.status === "rejected");
      if (items.status === "fulfilled") setTickets(items.value);
      if (providers.status === "fulfilled") {
        setTrackers(providers.value.trackers); setProjects(providers.value.projects);
        setTracker(current => current || providers.value.default);
      }
      if (detail.status === "fulfilled") setSelected(detail.value);
      setError(failures.length ? failures[0].reason.message : ""); setLoaded(true);
      if (!cancelled) timer = setTimeout(refresh, 5000);
    }
    refresh();
    return () => { cancelled = true; clearTimeout(timer); };
  }, [ticketKey, revision]);

  async function mutate(label, action) {
    setPending(label); setError("");
    try {
      const result = await action();
      await onChanged();
      setRevision(value => value + 1);
      return result;
    } catch (failure) { setError(failure.message); return null; }
    finally { setPending(""); }
  }

  async function create(fields) {
    const ticket = await mutate("create", () => createTicket(csrfToken, fields));
    if (ticket) { setCreating(false); window.location.hash = `#/tickets/${encodeURIComponent(ticket.key)}`; }
  }

  const filtered = tickets.filter(ticket => `${ticket.key} ${ticket.title} ${ticket.repository}`.toLowerCase().includes(search.toLowerCase()));
  const provider = trackers.find(item => item.id === tracker);
  const external = jobs.map(job => ({ job, link: cardLinks(job).ticket })).filter(({ link }) => link?.tracker === tracker);

  return <div className="mx-auto max-w-[1500px] space-y-6 p-4 sm:p-6 lg:p-8">
    <PageHeading title={ticketKey || "Tickets"} description={ticketKey ? "From a clear brief to a delivered change." : "Plan the work. Approve it when it is ready for the factory."}>
      <div className="flex flex-wrap items-center gap-2">
        {ticketKey ? <Button asChild variant="outline"><a href="#/tickets"><ArrowLeft className="size-4" />All tickets</a></Button> : <>
          <label className="sr-only" htmlFor="ticket-tracker">Tracker</label>
          <select id="ticket-tracker" className="field-control w-auto!" value={tracker || "builtin"} onChange={event => { setTracker(event.target.value); setCreating(false); }}>
            {Object.entries(trackerNames).map(([id, name]) => <option key={id} value={id}>{name}</option>)}
          </select>
          {tracker === "builtin" && <Button onClick={() => setCreating(value => !value)}><Plus className="size-4" />New ticket</Button>}
        </>}
      </div>
    </PageHeading>
    {error && <Alert>{error} <button className="ml-2 underline" onClick={() => setRevision(value => value + 1)}>Retry</button></Alert>}
    {!loaded ? <QuietState title="Loading tickets" description="Reading the tracker and its delivery history." role="status" /> : ticketKey ? selected ?
      <TicketDetail key={selected.key} ticket={selected} csrfToken={csrfToken} repositories={repositories} pending={pending} mutate={mutate} /> : <QuietState title="Ticket unavailable" description="Check the key or return to the ticket list." /> : tracker === "builtin" ? <>
      {creating && <Card className="p-5"><h2 className="mb-4 text-base font-semibold">New ticket</h2><TicketForm projects={projects} repositories={repositories} pending={pending === "create"} onSave={create} onCancel={() => setCreating(false)} /></Card>}
      <div className="flex flex-wrap items-center justify-between gap-3">
        <label className="w-full sm:max-w-sm"><span className="sr-only">Search tickets</span><input className="field-control" placeholder="Search title, key, or repository" value={search} onChange={event => setSearch(event.target.value)} /></label>
        <span className="text-xs text-muted-foreground">{filtered.length} ticket{filtered.length === 1 ? "" : "s"} · Built-in</span>
      </div>
      <Card className="overflow-hidden">
        {filtered.length ? <ul className="divide-y divide-border">{filtered.map(ticket => <li key={ticket.key}><a href={`#/tickets/${encodeURIComponent(ticket.key)}`} className="flex flex-wrap items-start gap-3 p-4 hover:bg-muted/35 focus-visible:outline-2 focus-visible:outline-primary sm:p-5">
          <Ticket className="mt-0.5 size-4 shrink-0 text-muted-foreground" />
          <div className="min-w-0 flex-1"><div className="flex flex-wrap items-center gap-2"><span className="font-mono text-xs text-muted-foreground">{ticket.key}</span><Badge tone={ticketStateTone(ticket.state)}>{ticketStateLabel(ticket.state)}</Badge>{ticket.pr_url && <a className="font-mono text-xs text-primary underline" href={ticket.pr_url} target="_blank" rel="noopener noreferrer" aria-label="Pull request">PR<ArrowUpRight className="ml-0.5 inline size-3" /></a>}</div><h2 className="mt-2 break-words text-sm font-medium">{ticket.title}</h2><p className="mt-1 text-xs text-muted-foreground">{ticket.project ? `${ticket.project} · ` : ""}{ticket.repository || "No repository selected"}</p></div>
          <time className="text-xs text-muted-foreground" dateTime={ticket.updated_at}>{relativeTime(ticket.updated_at)}</time>
        </a></li>)}</ul> : <QuietState title={search ? "No matching tickets" : "Your next change starts here"} description={search ? "Try a different title, key, or repository." : "Create a ticket, describe the outcome, and approve it for the factory when ready."} />}
      </Card>
    </> : <>
      <div className="flex flex-wrap items-center justify-between gap-4 border-b border-border pb-5"><div><h2 className="text-base font-semibold">{trackerNames[tracker]} intake</h2><p className="mt-2 max-w-2xl text-sm text-muted-foreground">Create and edit tickets in {trackerNames[tracker]}. Apply your configured factory approval label to submit work. Imported runs and their source tickets appear below.</p></div><Button asChild variant="outline"><a href="#/triggers">{provider?.configured ? "Manage triggers" : "Configure intake"}<ArrowUpRight className="size-4" /></a></Button></div>
      <Card>{external.length ? <ul className="divide-y divide-border">{external.map(({ job, link }) => <li key={job.id} className="flex flex-wrap items-center justify-between gap-3 p-5"><div className="min-w-0"><a className="font-mono text-sm text-primary underline" href={link.href} target="_blank" rel="noopener noreferrer">{link.text}<ArrowUpRight className="ml-1 inline size-3" /></a><p className="mt-1 break-words text-sm">{job.title || job.github_issue_title || job.repository}</p></div><div className="flex items-center gap-3"><State value={job.state} /><a className="text-sm underline" href={`#/runs/${encodeURIComponent(job.id)}`}>View run</a></div></li>)}</ul> : <QuietState title={provider?.configured ? "No imported tickets yet" : `${trackerNames[tracker]} is not configured`} description={provider?.configured ? "Approved tickets appear here after the next intake poll." : "Set the tracker credentials, project, repository and approval label in the server configuration, then restart the server."} />}</Card>
    </>}
  </div>;
}

function TicketForm({ ticket, repositories, projects = {}, pending, onSave, onCancel }) {
  const [title, setTitle] = useState(ticket?.title || "");
  const [description, setDescription] = useState(ticket?.description || "");
  const [repository, setRepository] = useState(ticket?.repository || repositories[0] || "");
  const [project, setProject] = useState(ticket?.project || "");
  const projectNames = Object.keys(projects).sort();
  // Project is create-time only: the ticket key derives from it, so edits never
  // send it (the API refuses unknown fields).
  return <form className="space-y-4" onSubmit={event => {
    event.preventDefault();
    const fields = { title, description, repository };
    if (!ticket) fields.project = project;
    onSave(fields);
  }}>
    <label className="block"><span className="field-label">Title</span><input className="field-control" required maxLength={500} value={title} onChange={event => setTitle(event.target.value)} placeholder="What should change?" /></label>
    <label className="block"><span className="field-label">Description</span><textarea className="field-control min-h-40" rows={6} maxLength={262144} value={description} onChange={event => setDescription(event.target.value)} placeholder="Describe the outcome, constraints, and how to verify it." /></label>
    <label className="block"><span className="field-label">Repository</span><input className="field-control" list="ticket-repositories" disabled={Boolean(ticket?.job_id)} value={repository} maxLength={256} onChange={event => setRepository(event.target.value)} placeholder="Worker repository name" /><datalist id="ticket-repositories">{repositories.map(name => <option key={name} value={name} />)}</datalist></label>
    {!ticket && projectNames.length > 0 && <label className="block"><span className="field-label">Project · optional</span><select className="field-control" value={project} onChange={event => {
      const name = event.target.value;
      setProject(name);
      if (name && projects[name]?.repository) setRepository(projects[name].repository);
    }}><option value="">No project (default key prefix)</option>{projectNames.map(name => <option key={name} value={name}>{name}</option>)}</select></label>}
    <div className="flex flex-wrap justify-end gap-2"><Button type="button" variant="ghost" disabled={pending} onClick={onCancel}>Cancel</Button><Button disabled={pending || !title.trim()}>{pending ? "Saving…" : ticket ? "Save changes" : "Create ticket"}</Button></div>
  </form>;
}

function TicketDetail({ ticket, csrfToken, repositories, pending, mutate }) {
  const [editing, setEditing] = useState(false);
  const [comment, setComment] = useState("");
  const [confirmDelete, setConfirmDelete] = useState(false);
  const states = ticket.job_id ? ["in_review", "done", "cancelled"] : ["backlog", "todo", "done", "cancelled"];
  const options = [...new Set([ticket.state, ...states])];
  async function save(fields) { if (await mutate("save", () => updateTicket(csrfToken, ticket.key, fields))) setEditing(false); }
  async function remove() {
    // DELETE returns no JSON; use a boolean so success is distinguishable from an error.
    if (await mutate("delete", async () => { await deleteTicket(csrfToken, ticket.key); return true; })) window.location.hash = "#/tickets";
  }
  return <div className="grid min-w-0 items-start gap-6 lg:grid-cols-[minmax(0,1fr)_280px]">
    <div className="min-w-0 space-y-6">
      <Card className="p-5 sm:p-6">{editing ? <TicketForm ticket={ticket} repositories={repositories} pending={pending === "save"} onSave={save} onCancel={() => setEditing(false)} /> : <>
        <div className="flex items-start justify-between gap-4"><h2 className="min-w-0 break-words text-xl font-semibold">{ticket.title}</h2><Button size="sm" variant="outline" onClick={() => setEditing(true)}>Edit</Button></div>
        <p className="mt-5 max-w-prose whitespace-pre-wrap break-words text-sm leading-6">{ticket.description || "No description yet. Add the context and acceptance criteria before approving this ticket."}</p>
      </>}</Card>
      <section aria-labelledby="ticket-activity"><h2 id="ticket-activity" className="mb-4 text-base font-semibold">Activity</h2>
        <ol className="space-y-4">{ticket.comments.map(item => <li key={item.id} className="border-b border-border pb-4"><time className="text-xs text-muted-foreground" dateTime={item.created_at}>{new Date(item.created_at).toLocaleString()}</time><p className="mt-2 whitespace-pre-wrap break-words text-sm leading-6">{linkifyText(item.body)}</p></li>)}</ol>
        <form className="mt-5 space-y-3" onSubmit={async event => { event.preventDefault(); if (await mutate("comment", () => commentOnTicket(csrfToken, ticket.key, comment))) setComment(""); }}><label className="block"><span className="field-label">Add a comment</span><textarea className="field-control" rows={3} maxLength={65536} required value={comment} onChange={event => setComment(event.target.value)} placeholder="Add context or record a decision" /></label><Button variant="outline" disabled={Boolean(pending) || !comment.trim()}>{pending === "comment" ? "Posting…" : "Post comment"}</Button></form>
      </section>
    </div>
    <aside className="space-y-5 border-t border-border pt-5 lg:border-t-0 lg:pt-0" aria-label="Ticket details">
      <Badge tone={ticketStateTone(ticket.state)}>{ticketStateLabel(ticket.state)}</Badge>
      <label className="block"><span className="field-label">Status</span><select className="field-control" value={ticket.state} disabled={Boolean(pending)} onChange={event => mutate("state", () => updateTicket(csrfToken, ticket.key, { state: event.target.value }))}>{options.map(state => <option key={state} value={state} disabled={!states.includes(state)}>{ticketStateLabel(state)}</option>)}</select></label>
      <dl className="space-y-4 text-sm"><div><dt className="text-xs text-muted-foreground">Repository</dt><dd className="mt-1 break-words font-mono text-xs">{ticket.repository || "Not selected"}</dd></div>{ticket.project && <div><dt className="text-xs text-muted-foreground">Project</dt><dd className="mt-1 font-mono text-xs">{ticket.project}</dd></div>}{ticket.pr_url && <div><dt className="text-xs text-muted-foreground">Pull request</dt><dd className="mt-1 break-words"><a className="text-primary underline" href={ticket.pr_url} target="_blank" rel="noopener noreferrer">{ticket.pr_url}<ArrowUpRight className="ml-1 inline size-3" /></a></dd></div>}<div><dt className="text-xs text-muted-foreground">Tracker</dt><dd className="mt-1">Built-in</dd></div><div><dt className="text-xs text-muted-foreground">Updated</dt><dd className="mt-1">{relativeTime(ticket.updated_at)}</dd></div></dl>
      {ticket.job_id ? <Button asChild variant="outline" className="w-full"><a href={`#/runs/${encodeURIComponent(ticket.job_id)}`}>View factory run<ArrowUpRight className="size-4" /></a></Button> : <>
        <Button className="w-full" disabled={Boolean(pending) || !canApproveTicket(ticket)} onClick={() => mutate("approve", () => approveTicket(csrfToken, ticket.key))}><Check className="size-4" />{pending === "approve" ? "Approving…" : "Approve for factory"}</Button>
        {!ticket.repository && <p className="text-xs text-muted-foreground">Choose a repository before approving.</p>}
        {confirmDelete ? <div className="space-y-2"><p className="text-sm">Delete this draft and its comments?</p><div className="flex flex-wrap gap-2"><Button variant="outline" disabled={Boolean(pending)} onClick={remove}>Confirm delete</Button><Button variant="ghost" onClick={() => setConfirmDelete(false)}>Keep ticket</Button></div></div> : <Button variant="ghost" className="text-danger" onClick={() => setConfirmDelete(true)}><Trash2 className="size-4" />Delete draft</Button>}
      </>}
    </aside>
  </div>;
}
