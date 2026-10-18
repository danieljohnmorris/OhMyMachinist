// ImportSection is the Tickets-page card that one-shot copies a configured
// Plane or Linear project into the built-in tickets table. It previews
// without writing (dry-run), then imports and reports created/skipped counts.

import { Download } from "lucide-react";
import { useState } from "react";
import { Alert } from "@/components/ui/alert";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { previewImport, runImport, ticketStateLabel } from "./tickets-api.js";

const importSources = [
  { id: "plane", label: "Plane" },
  { id: "linear", label: "Linear" },
];

// importRowLabel names what an import does with one remote issue: the mapped
// ticket state, or why it is skipped.
export function importRowLabel(ticket) {
  if (ticket.action === "skip") {
    return ticket.reason === "completed" ? "Completed" : "Already exists";
  }
  return ticketStateLabel(ticket.state);
}

export function ImportSection({ csrfToken, trackers = [], onImported }) {
  const [open, setOpen] = useState(false);
  const [source, setSource] = useState("");
  const [projects, setProjects] = useState([]);
  const [project, setProject] = useState("");
  const [tickets, setTickets] = useState([]);
  const [counts, setCounts] = useState(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const available = importSources.filter(({ id }) => trackers.some((tracker) => tracker.id === id && tracker.configured));
  if (!available.length) return null;

  async function chooseSource(nextSource) {
    setSource(nextSource);
    setProject("");
    setTickets([]);
    setCounts(null);
    setError("");
    setBusy(true);
    try {
      const roster = await previewImport(csrfToken, nextSource);
      setProjects(roster.projects || []);
    } catch (failure) {
      setError(failure.message);
      setProjects([]);
    } finally {
      setBusy(false);
    }
  }

  async function chooseProject(nextProject) {
    setProject(nextProject);
    setCounts(null);
    setError("");
    if (!nextProject) { setTickets([]); return; }
    setBusy(true);
    try {
      const plan = await previewImport(csrfToken, source, nextProject);
      setTickets(plan.tickets || []);
    } catch (failure) {
      setError(failure.message);
      setTickets([]);
    } finally {
      setBusy(false);
    }
  }

  async function importNow() {
    setBusy(true);
    setError("");
    try {
      const result = await runImport(csrfToken, source, project);
      setTickets(result.tickets || []);
      setCounts({ created: result.created, skipped: result.skipped });
      await onImported?.();
    } catch (failure) {
      setError(failure.message);
    } finally {
      setBusy(false);
    }
  }

  const importable = projects.filter((item) => item.repository);

  return <Card className="p-5">
    <div className="flex flex-wrap items-center justify-between gap-3">
      <div>
        <h2 className="text-base font-semibold">Import from {available.map(({ label }) => label).join(" or ")}</h2>
        <p className="mt-1 max-w-2xl text-sm text-muted-foreground">Copy every issue of a project into the ticket list. Existing keys and completed work are skipped; nothing is written until you import.</p>
      </div>
      <Button variant={open ? "ghost" : "outline"} disabled={busy} onClick={() => setOpen((value) => !value)}>
        <Download className="size-4" />{open ? "Hide import" : "Import issues…"}
      </Button>
    </div>
    {open && <div className="mt-4 space-y-4">
      {error && <Alert>{error}</Alert>}
      <div className="flex flex-wrap items-end gap-3">
        <label className="block"><span className="field-label">Source</span>
          <select className="field-control w-auto!" value={source} disabled={busy} onChange={(event) => chooseSource(event.target.value)}>
            <option value="">Choose a source</option>
            {available.map(({ id, label }) => <option key={id} value={id}>{label}</option>)}
          </select>
        </label>
        <label className="block"><span className="field-label">Project</span>
          <select className="field-control w-auto!" value={project} disabled={busy || !source} onChange={(event) => chooseProject(event.target.value)}>
            <option value="">Choose a project</option>
            {importable.map((item) => <option key={item.id} value={item.name}>{item.name}</option>)}
          </select>
        </label>
        <Button disabled={busy || !project || !tickets.length} onClick={importNow}>{busy ? "Working…" : "Import"}</Button>
        {counts && <span className="text-sm text-muted-foreground">Created {counts.created}, skipped {counts.skipped}.</span>}
      </div>
      {tickets.length > 0 && <div className="overflow-x-auto">
        <table className="w-full min-w-[32rem] border-collapse text-sm">
          <thead>
            <tr className="border-b border-border text-left text-xs uppercase tracking-wide text-muted-foreground">
              <th className="px-4 py-2.5 font-medium">Key</th>
              <th className="px-4 py-2.5 font-medium">Title</th>
              <th className="px-4 py-2.5 font-medium">Plan</th>
            </tr>
          </thead>
          <tbody>
            {tickets.map((ticket) => <tr key={ticket.key} className="border-b border-border last:border-b-0">
              <td className="px-4 py-2.5 font-mono text-xs">{ticket.key}</td>
              <td className="px-4 py-2.5">{ticket.title}</td>
              <td className="px-4 py-2.5"><Badge tone={ticket.action === "skip" ? "neutral" : "surface"}>{importRowLabel(ticket)}</Badge></td>
            </tr>)}
          </tbody>
        </table>
      </div>}
      {source && !busy && !tickets.length && project && <p className="text-sm text-muted-foreground">Nothing to import — every issue already exists or is completed.</p>}
    </div>}
  </Card>;
}
