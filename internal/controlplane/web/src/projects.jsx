import { useEffect, useRef, useState } from "react";
import { FolderGit2, Pencil, Plus } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { PageHeading, QuietState } from "@/components/ui/page-heading";
import { loadSettings, saveSetting, ProjectSkillsCard } from "@/project-skills-card";
import { mergeProjects, serializeProjectTOML } from "@/projects-state.js";

// Projects groups built-in tickets: a ticket filed under SPARKS becomes
// SPARKS-1 and runs with the project's repository, command and model. The
// config file defines projects in [tracker.projects] tables; this view writes
// the same table as a tracker.projects.<NAME> setting, which overlays the file
// the way a config overlay does — the server picks it up on the next request,
// no restart.
export function ProjectsPage({ repositories = [], commands = [], csrfToken }) {
  const [projects, setProjects] = useState([]);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [editing, setEditing] = useState(null);
  const [selected, setSelected] = useState("");
  const sequence = useRef(0);

  const refresh = async (signal) => {
    const current = ++sequence.current;
    const [trackers, settings] = await Promise.all([loadTrackers(signal), loadSettings(signal)]);
    if (sequence.current !== current) return;
    setProjects(mergeProjects(trackers.projects || {}, settings));
    setLoaded(true);
  };

  useEffect(() => {
    const controller = new AbortController();
    setLoaded(false);
    setError("");
    refresh(controller.signal).catch((loadError) => {
      if (loadError.name === "AbortError") return;
      setError(loadError.message);
      setLoaded(true);
    });
    return () => controller.abort();
  }, []);

  const startEdit = (project) => {
    setError("");
    setNotice("");
    setSelected(project?.name || "");
    setEditing(project ? { ...project } : { name: "", repository: "", command: "", model: "", source: "override" });
  };

  const closeEditor = () => {
    setEditing(null);
    setError("");
    setNotice("");
  };

  const save = async () => {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const name = editing.name.trim().toUpperCase();
      await saveSetting(`tracker.projects.${name}`, serializeProjectTOML(editing), csrfToken);
      await refresh();
      setSelected(name);
      setNotice(`Saved. Tickets filed under ${name} now use these settings — no restart needed.`);
    } catch (saveError) {
      setError(saveError.message);
    } finally {
      setBusy(false);
    }
  };

  return <div className="mx-auto max-w-[1500px] space-y-6 p-4 sm:p-6 lg:p-8">
    <PageHeading title="Projects" description="Projects group built-in tickets: a ticket filed under SPARKS becomes SPARKS-1 and runs with the project's repository, command and model. Edits are stored as settings and take effect on the next ticket — no restart.">
      <Button onClick={() => startEdit(null)} disabled={editing !== null}><Plus className="size-4" />New project</Button>
    </PageHeading>

    {editing && <ProjectForm
      project={editing}
      repositories={repositories}
      commands={commands}
      busy={busy}
      existing={projects.some((project) => project.name === editing.name.trim().toUpperCase())}
      onChange={setEditing}
      onCancel={closeEditor}
      onSave={save}
    />}

    {error && <Alert>{error}</Alert>}
    {notice && <div role="status" className="rounded-md border border-success/35 bg-success/10 px-3 py-2 text-sm text-success">{notice}</div>}

    {loaded && projects.length === 0 && !editing && <Card>
      <QuietState
        title="No projects yet"
        description="Every built-in ticket shares one key prefix and defaults. Add a project to give a stream of work its own SPARKS-1 keys, repository and command."
      />
    </Card>}

    {projects.length > 0 && <Card className="overflow-hidden">
      <div className="overflow-x-auto">
        <table className="w-full min-w-[36rem] border-collapse text-sm">
          <thead>
            <tr className="border-b border-border text-left text-xs uppercase tracking-wide text-muted-foreground">
              <th className="px-4 py-2.5 font-medium">Project</th>
              <th className="px-4 py-2.5 font-medium">Repository</th>
              <th className="px-4 py-2.5 font-medium">Command</th>
              <th className="px-4 py-2.5 font-medium">Model</th>
              <th className="px-4 py-2.5 font-medium">Defined by</th>
              <th className="px-4 py-2.5 text-right font-medium"><span className="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            {projects.map((project) => <tr
              key={project.name}
              className={`cursor-pointer border-b border-border last:border-b-0 hover:bg-muted/60 ${selected === project.name ? "bg-muted/40" : ""}`}
              onClick={() => setSelected((current) => (current === project.name ? "" : project.name))}
            >
              <td className="px-4 py-2.5 font-mono">{project.name}</td>
              <td className="px-4 py-2.5 font-mono text-xs" title={project.repository}>{project.repository || <span className="text-muted-foreground">tracker default</span>}</td>
              <td className="px-4 py-2.5">{project.command || <span className="text-muted-foreground">tracker default</span>}</td>
              <td className="px-4 py-2.5 font-mono text-xs" title={project.model}>{project.model || <span className="text-muted-foreground">command default</span>}</td>
              <td className="px-4 py-2.5"><Badge tone="surface">{project.source === "override" ? "setting" : "config file"}</Badge></td>
              <td className="px-4 py-2.5 text-right">
                <Button variant="ghost" size="sm" onClick={(event) => { event.stopPropagation(); startEdit(project); }} aria-label={`Edit project ${project.name}`}>
                  <Pencil className="size-3.5" />Edit
                </Button>
              </td>
            </tr>)}
          </tbody>
        </table>
      </div>
      <p className="border-t border-border px-4 py-3 text-xs text-muted-foreground">Select a row to manage its per-project skills. A project marked <span className="font-mono">override</span> overlays its <span className="font-mono">[tracker.projects]</span> table in config.toml.</p>
    </Card>}

    {!loaded && <QuietState title="Loading projects" description="Reading the resolved tracker configuration." role="status" />}

    {selected && <ProjectSkillsCard project={selected} csrfToken={csrfToken} />}
  </div>;
}

function ProjectForm({ project, repositories, commands, busy, existing, onChange, onCancel, onSave }) {
  const name = project.name.trim().toUpperCase();
  const invalidName = name !== "" && !/^[A-Z][A-Z0-9]{0,9}$/.test(name);
  return <Card className="space-y-4 p-4 sm:p-5">
    <div className="flex items-center gap-2">
      <FolderGit2 className="size-4 text-muted-foreground" aria-hidden="true" />
      <h2 className="text-lg font-semibold">{existing ? `Edit ${name}` : "New project"}</h2>
      {existing && <Badge tone="surface">saved as an override setting</Badge>}
    </div>
    <div className="grid gap-4 sm:grid-cols-2">
      <label className="space-y-1">
        <span className="field-label">Name (the ticket key prefix)</span>
        <input
          className="field-control font-mono"
          value={project.name}
          onChange={(event) => onChange({ ...project, name: event.target.value.toUpperCase() })}
          placeholder="SPARKS"
          aria-label="Project name"
          aria-invalid={invalidName || undefined}
          autoComplete="off"
          spellCheck="false"
          autoFocus={!existing}
        />
        <span className="block text-xs text-muted-foreground">1–10 uppercase letters or digits starting with a letter. Tickets become {name || "NAME"}-1, -2, …</span>
      </label>
      <label className="space-y-1">
        <span className="field-label">Repository</span>
        <input
          className="field-control font-mono"
          list="project-repositories"
          value={project.repository}
          onChange={(event) => onChange({ ...project, repository: event.target.value })}
          placeholder="tracker default"
          aria-label="Repository"
          autoComplete="off"
          spellCheck="false"
        />
        <datalist id="project-repositories">{repositories.map((name) => <option key={name} value={name} />)}</datalist>
      </label>
      <label className="space-y-1">
        <span className="field-label">Command</span>
        <select
          className="field-control"
          value={project.command}
          onChange={(event) => onChange({ ...project, command: event.target.value })}
          aria-label="Command"
        >
          <option value="">Tracker default</option>
          {commands.map((name) => <option key={name} value={name}>{name}</option>)}
        </select>
      </label>
      <label className="space-y-1">
        <span className="field-label">Model override</span>
        <input
          className="field-control font-mono"
          value={project.model}
          onChange={(event) => onChange({ ...project, model: event.target.value })}
          placeholder="command default"
          aria-label="Model override"
          autoComplete="off"
          spellCheck="false"
        />
      </label>
    </div>
    <p className="text-xs text-muted-foreground">Saved as <span className="font-mono">tracker.projects.{name || "NAME"}</span>{existing ? ", overlaying the config file table" : ""}. Leave a field empty to fall back to the tracker or command default.</p>
    <div className="flex flex-wrap gap-2">
      <Button disabled={busy || invalidName || name === ""} onClick={onSave}>{busy ? "Saving…" : "Save"}</Button>
      <Button variant="outline" disabled={busy} onClick={onCancel}>Cancel</Button>
    </div>
  </Card>;
}

// loadTrackers reads the resolved tracker configuration, which already merges
// the config file with any tracker.projects settings the server applied.
async function loadTrackers(signal) {
  const response = await fetch("/api/v1/trackers", { headers: { Accept: "application/json" }, signal });
  if (!response.ok) throw new Error(`Trackers request failed (${response.status})`);
  const body = await response.json().catch(() => ({}));
  return body && typeof body === "object" ? body : {};
}
