import { useEffect, useRef, useState } from "react";
import { ArrowDown, ArrowUp, X } from "lucide-react";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { QuietState } from "@/components/ui/page-heading";
import { loadSkills, skillNames } from "@/skills.js";
import { composeSkills, parseSkillsSetting, serializeSkillsSetting, unknownSkillNames } from "@/project-skills.js";

// ProjectSkillsCard edits the ordered skills list for one project, stored in the
// skills.project.<project> setting. Runs compose it with the repository
// .machinist/skills.yaml and the global skills.default setting. Names are
// validated against the installed library, and Resolve now previews the
// composed list with the versions each run would load.
// Shared by the Settings and Projects views.
export function ProjectSkillsCard({ project, csrfToken }) {
  const [entries, setEntries] = useState([]);
  const [library, setLibrary] = useState([]);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState("");
  const [preview, setPreview] = useState(null);
  const [kind, setKind] = useState("");
  const sequence = useRef(0);

  useEffect(() => {
    const controller = new AbortController();
    loadSkills(controller.signal).then(setLibrary).catch(() => setLibrary([]));
    return () => controller.abort();
  }, []);

  useEffect(() => {
    if (!project) {
      setEntries([]);
      setLoaded(false);
      return;
    }
    const current = ++sequence.current;
    const controller = new AbortController();
    setLoaded(false);
    setError("");
    setNotice("");
    setPreview(null);
    loadSettings(controller.signal)
      .then((settings) => {
        if (sequence.current !== current) return;
        setEntries(parseSkillsSetting(settings[`skills.project.${project}`] || ""));
        setLoaded(true);
      })
      .catch((loadError) => {
        if (sequence.current !== current || loadError.name === "AbortError") return;
        setError(loadError.message);
        setLoaded(true);
      });
    return () => controller.abort();
  }, [project]);

  const names = skillNames(library);
  const unknown = unknownSkillNames(entries, library);

  const update = (index, patch) => setEntries((current) => current.map((entry, position) => (position === index ? { ...entry, ...patch } : entry)));
  const addEntry = () => setEntries((current) => [...current, { name: "", appliesTo: [], required: false }]);
  const removeEntry = (index) => setEntries((current) => current.filter((_, position) => position !== index));
  const moveEntry = (index, delta) => setEntries((current) => {
    const target = index + delta;
    if (target < 0 || target >= current.length) return current;
    const next = [...current];
    [next[index], next[target]] = [next[target], next[index]];
    return next;
  });

  const toggleKind = (index, value) => setEntries((current) => current.map((entry, position) => {
    if (position !== index) return entry;
    const appliesTo = entry.appliesTo || [];
    return { ...entry, appliesTo: appliesTo.includes(value) ? appliesTo.filter((item) => item !== value) : [...appliesTo, value] };
  }));

  const save = async () => {
    setBusy("save");
    setError("");
    setNotice("");
    try {
      await saveSetting(`skills.project.${project}`, serializeSkillsSetting(entries), csrfToken);
      setNotice(`Saved. Runs for ${project} compose this list with the repository skills.yaml and the global default.`);
    } catch (saveError) {
      setError(saveError.message);
    } finally {
      setBusy("");
    }
  };

  const resolve = async () => {
    setBusy("resolve");
    setError("");
    setPreview(null);
    try {
      const settings = await loadSettings();
      const globalEntries = parseSkillsSetting(settings["skills.default"] || "");
      setPreview(composeSkills({ projectEntries: entries, globalEntries, kind, skills: library }));
    } catch (previewError) {
      setError(previewError.message);
    } finally {
      setBusy("");
    }
  };

  return <Card className="space-y-4 p-4 sm:p-5">
    <div>
      <h2 className="text-lg font-semibold">Project skills</h2>
      <p className="mt-1 text-sm text-muted-foreground">An ordered list of skills every run for {project || "this project"} loads. Repository <code>.machinist/skills.yaml</code> composes first, then this list, then the global default. Untick both kinds to apply to every run.</p>
    </div>
    {error && <Alert>{error}</Alert>}
    {notice && <div role="status" className="rounded-md border border-success/35 bg-success/10 px-3 py-2 text-sm text-success">{notice}</div>}
    {!loaded ? <QuietState title={project ? "Loading project skills" : "No repositories yet"} description={project ? "Fetching the stored skills list." : "Register a worker repository to edit its skills."} role="status" />
      : <>
        <datalist id="skill-library-names">{names.map((name) => <option key={name} value={name} />)}</datalist>
        {entries.length === 0 && <p className="text-sm text-muted-foreground">No project skills yet. Add one to load it on every run.</p>}
        <ol className="space-y-3">
          {entries.map((entry, index) => <li key={index} className="flex flex-col gap-3 rounded-md border border-border p-3 sm:flex-row sm:items-center">
            <div className="flex items-center gap-2 sm:flex-col">
              <Button variant="ghost" size="icon" className="size-7" aria-label={`Move ${entry.name || "entry"} up`} disabled={index === 0} onClick={() => moveEntry(index, -1)}><ArrowUp className="size-3.5" /></Button>
              <Button variant="ghost" size="icon" className="size-7" aria-label={`Move ${entry.name || "entry"} down`} disabled={index === entries.length - 1} onClick={() => moveEntry(index, 1)}><ArrowDown className="size-3.5" /></Button>
            </div>
            <label className="min-w-0 flex-1 space-y-1">
              <span className="field-label">Skill name{unknown.includes(entry.name) ? " · not installed" : ""}</span>
              <input
                className="field-control"
                list="skill-library-names"
                value={entry.name}
                onChange={(event) => update(index, { name: event.target.value })}
                placeholder="feature-dev:code-reviewer or name@version"
                aria-label={`Skill ${index + 1} name`}
              />
            </label>
            <div className="flex flex-wrap items-center gap-4 text-sm">
              <label className="flex items-center gap-1.5"><input type="checkbox" className="size-3.5 accent-primary" checked={(entry.appliesTo || []).includes("build")} onChange={() => toggleKind(index, "build")} />Build</label>
              <label className="flex items-center gap-1.5"><input type="checkbox" className="size-3.5 accent-primary" checked={(entry.appliesTo || []).includes("review")} onChange={() => toggleKind(index, "review")} />Review</label>
              <label className="flex items-center gap-1.5"><input type="checkbox" className="size-3.5 accent-primary" checked={entry.required} onChange={(event) => update(index, { required: event.target.checked })} />Required</label>
            </div>
            <Button variant="ghost" size="icon" className="size-7 text-danger hover:bg-danger/10 hover:text-danger" aria-label={`Remove ${entry.name || "entry"}`} onClick={() => removeEntry(index)}><X className="size-4" /></Button>
          </li>)}
        </ol>
        {unknown.length > 0 && <p role="status" className="text-xs text-warning">Not installed in the library: {unknown.join(", ")}. A repository-bundled skill may still resolve at run time.</p>}
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="outline" size="sm" onClick={addEntry}>Add skill</Button>
          <Button size="sm" disabled={busy !== "" || !project} onClick={save}>{busy === "save" ? "Saving…" : "Save"}</Button>
          <label className="flex items-center gap-2">
            <span className="text-sm text-muted-foreground">Preview</span>
            <select className="field-control h-8 w-auto py-0 text-xs" value={kind} onChange={(event) => setKind(event.target.value)} aria-label="Preview run kind">
              <option value="">All runs</option>
              <option value="build">Build runs</option>
              <option value="review">Review runs</option>
            </select>
          </label>
          <Button variant="outline" size="sm" disabled={busy !== "" || !project} onClick={resolve}>{busy === "resolve" ? "Resolving…" : "Resolve now"}</Button>
        </div>
        {preview && <div className="space-y-2 rounded-md border p-3">
          <h3 className="text-sm font-medium">Composed list</h3>
          {preview.skills.length === 0 ? <p className="text-sm text-muted-foreground">No skills resolve for this selection.</p>
            : <div className="overflow-x-auto"><table className="w-full min-w-[32rem] border-collapse text-sm">
              <thead><tr className="border-b border-border text-left text-xs uppercase tracking-wide text-muted-foreground">
                <th className="py-2 pr-4 font-medium">Name</th>
                <th className="py-2 pr-4 font-medium">Version</th>
                <th className="py-2 pr-4 font-medium">Scope</th>
                <th className="py-2 pr-4 font-medium">Status</th>
              </tr></thead>
              <tbody>
                {preview.skills.map((skill) => <tr key={`${skill.scope}:${skill.name}`} className="border-b border-border last:border-b-0">
                  <td className="py-2 pr-4">{skill.name}{skill.required && <span className="ml-1 text-xs text-muted-foreground">required</span>}</td>
                  <td className="py-2 pr-4 font-mono text-xs" title={skill.version}>{skill.version || "—"}</td>
                  <td className="py-2 pr-4 capitalize">{skill.scope}</td>
                  <td className="py-2 pr-4">{skill.status === "installed" ? "installed" : skill.status === "required" ? "missing (required)" : "not installed"}</td>
                </tr>)}
              </tbody>
            </table></div>}
          {preview.warnings.length > 0 && <ul className="space-y-1 text-xs text-warning">{preview.warnings.map((warning) => <li key={warning}>{warning}</li>)}</ul>}
        </div>}
      </>}
  </Card>;
}

// loadSettings reads every stored setting key/value pair.
export async function loadSettings(signal) {
  const response = await fetch("/api/v1/settings", { headers: { Accept: "application/json" }, signal });
  if (!response.ok) throw new Error(`Settings request failed (${response.status})`);
  const body = await response.json().catch(() => ({}));
  return body.settings && typeof body.settings === "object" ? body.settings : {};
}

// saveSetting writes one setting value.
export async function saveSetting(key, value, csrfToken) {
  const response = await fetch(`/api/v1/settings/${encodeURIComponent(key)}`, {
    method: "PUT",
    headers: { "Content-Type": "application/json", "X-Machinist-CSRF": csrfToken },
    body: JSON.stringify({ value }),
  });
  if (!response.ok) {
    const body = await response.json().catch(() => ({}));
    throw new Error(body.error || `Setting save failed (${response.status})`);
  }
}
