import { useEffect, useRef, useState } from "react";
import { ArrowDown, ArrowUp, X } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { PageHeading, QuietState } from "@/components/ui/page-heading";
import {
  loadReviewProfile,
  loadReviewProfiles,
  previewReviewProfile,
  saveReviewDefaultProfile,
  saveReviewProfileSetting,
} from "@/review-settings";
import { createToken, loadTokens, revokeToken } from "@/api-tokens";
import { loadSkills, skillNames } from "@/skills.js";
import { composeSkills, parseSkillsSetting, serializeSkillsSetting, unknownSkillNames } from "@/project-skills.js";

// Settings: per-project review profile for the built-in PR review (MACH-32).
// The review runs from a resolved profile: repo .machinist/review.yaml at the
// PR base ref > this project setting > the global default > the shipped
// default profile.
export function SettingsPage({ repositories = [], csrfToken }) {
  const [project, setProject] = useState("");
  const [mode, setMode] = useState("library");
  const [profileName, setProfileName] = useState("");
  const [profileBody, setProfileBody] = useState("");
  const [extraInstructions, setExtraInstructions] = useState("");
  const [library, setLibrary] = useState([]);
  const [defaultName, setDefaultName] = useState("");
  const [source, setSource] = useState("");
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [preview, setPreview] = useState(null);
  const [previewFiles, setPreviewFiles] = useState("");
  const [busy, setBusy] = useState(false);
  const loadSequence = useRef(0);

  useEffect(() => {
    if (project || !repositories.length) return;
    setProject(repositories[0]);
  }, [project, repositories]);

  useEffect(() => {
    const controller = new AbortController();
    loadReviewProfiles(controller.signal)
      .then((result) => {
        setLibrary(result.profiles);
        setDefaultName(result.defaultName);
      })
      .catch(() => setLibrary([]));
    return () => controller.abort();
  }, []);

  useEffect(() => {
    if (!project) return;
    const sequence = ++loadSequence.current;
    const controller = new AbortController();
    setLoaded(false);
    setError("");
    setNotice("");
    setPreview(null);
    loadReviewProfile(project, controller.signal)
      .then((result) => {
        if (loadSequence.current !== sequence) return;
        setSource(result.source);
        setProfileName("");
        setProfileBody("");
        setExtraInstructions(result.extraInstructions);
        setLoaded(true);
      })
      .catch((loadError) => {
        if (loadError.name === "AbortError" || loadSequence.current !== sequence) return;
        setError(loadError.message);
        setLoaded(true);
      });
    return () => controller.abort();
  }, [project]);

  const setting = () => ({
    profile_name: mode === "library" ? profileName : "",
    profile_body: mode === "inline" ? profileBody : "",
    extra_instructions: extraInstructions,
  });

  const save = async () => {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const result = await saveReviewProfileSetting(project, setting(), csrfToken);
      setSource(result.source);
      setNotice(`Saved. This project now resolves its review profile from: ${result.source}.`);
    } catch (saveError) {
      setError(saveError.message);
    } finally {
      setBusy(false);
    }
  };

  const reset = async () => {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      await saveReviewProfileSetting(project, { profile_name: "", profile_body: "", extra_instructions: "" }, csrfToken);
      setProfileName("");
      setProfileBody("");
      setExtraInstructions("");
      setNotice("Reverted: this project uses the global default profile.");
      const result = await loadReviewProfile(project);
      setSource(result.source);
    } catch (saveError) {
      setError(saveError.message);
    } finally {
      setBusy(false);
    }
  };

  const runPreview = async () => {
    setBusy(true);
    setError("");
    setPreview(null);
    try {
      const body = mode === "inline" ? profileBody : "";
      const result = await previewReviewProfile(
        {
          project,
          repo_profile: body,
          files: previewFiles.split("\n").map((line) => line.trim()).filter(Boolean),
        },
        csrfToken,
      );
      setPreview(result);
    } catch (previewError) {
      setError(previewError.message);
    } finally {
      setBusy(false);
    }
  };

  return <div className="mx-auto max-w-[1500px] space-y-6 p-4 sm:p-6 lg:p-8">
    <PageHeading title="Settings" description="Review profiles for the built-in pull-request review. The review runs after every PR opens: lane checks, parallel reviewer passes, then a docs pass — all driven by the resolved profile." />
    <Card className="p-4 sm:p-5">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
        <label className="flex flex-1 items-center gap-2">
          <span className="text-sm font-medium">Project</span>
          <select className="field-control w-full max-w-xs" value={project} onChange={(event) => setProject(event.target.value)} aria-label="Project">
            {!repositories.length && <option value="">No repositories</option>}
            {repositories.map((name) => <option key={name} value={name}>{name}</option>)}
          </select>
        </label>
        {loaded && <Badge variant="outline" className="text-muted-foreground">Resolved from: {source || "default"}</Badge>}
      </div>
      {error && <div role="alert" className="mt-3 whitespace-pre-wrap rounded-md border border-danger/35 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</div>}
      {notice && <div role="status" className="mt-3 rounded-md border border-success/35 bg-success/10 px-3 py-2 text-sm text-success">{notice}</div>}
      {loaded && <div className="mt-4 space-y-4">
        <div className="flex flex-wrap items-center gap-3">
          <label className="flex items-center gap-2">
            <span className="text-sm font-medium">Profile</span>
            <select className="field-control w-full max-w-xs" value={mode} onChange={(event) => setMode(event.target.value)} aria-label="Profile source">
              <option value="library">Pick from the profile library</option>
              <option value="inline">Inline YAML profile</option>
            </select>
          </label>
          {mode === "library" && <select className="field-control w-full max-w-xs" value={profileName} onChange={(event) => setProfileName(event.target.value)} aria-label="Library profile">
            <option value="">Global default ({defaultName})</option>
            {library.map((profile) => <option key={profile.name} value={profile.name}>{profile.name}{profile.builtIn ? " (shipped)" : ""}</option>)}
          </select>}
        </div>
        {mode === "inline" && <label className="block space-y-2">
          <span className="text-sm font-medium">Profile YAML</span>
          <textarea
            className="field-control min-h-80 resize-y font-mono text-xs"
            value={profileBody}
            onChange={(event) => setProfileBody(event.target.value)}
            aria-label="Review profile YAML"
            spellCheck="false"
            placeholder={"name: my-profile\nversion: 1\n…"}
          />
        </label>}
        <label className="block space-y-2">
          <span className="text-sm font-medium">Extra instructions (appended to every reviewer prompt)</span>
          <textarea
            className="field-control min-h-24 resize-y font-mono text-xs"
            value={extraInstructions}
            onChange={(event) => setExtraInstructions(event.target.value)}
            aria-label="Extra review instructions"
            spellCheck="false"
          />
        </label>
        <div className="flex flex-wrap gap-2">
          <Button disabled={busy || !project} onClick={save}>Save</Button>
          <Button variant="outline" disabled={busy || !project} onClick={reset}>Revert to global default</Button>
        </div>
        <div className="space-y-2 rounded-md border p-3">
          <div className="flex flex-wrap items-end gap-3">
            <label className="flex flex-1 flex-col gap-1">
              <span className="text-sm font-medium">Preview (no execution): paste changed-file paths</span>
              <textarea
                className="field-control min-h-16 resize-y font-mono text-xs"
                value={previewFiles}
                onChange={(event) => setPreviewFiles(event.target.value)}
                aria-label="Preview changed files"
                spellCheck="false"
                placeholder={"internal/foo.ts\ndocs/README.md"}
              />
            </label>
            <Button variant="outline" disabled={busy || !project} onClick={runPreview}>Preview resolved profile</Button>
          </div>
          {preview && <PreviewResult preview={preview} />}
        </div>
        <p className="text-xs text-muted-foreground">Precedence: a .machinist/review.yaml at the PR base ref wins, then this project setting, then the global default profile, then the shipped default. Blocking findings send the change back to amend on the same branch; each round counts against the profile's blocking.amend_rounds budget.</p>
      </div>}
      {!loaded && <QuietState title={project ? "Loading review profile" : "No repositories yet"} description={project ? "Fetching the resolved profile for this project." : "Register a worker repository to edit its review profile."} role="status" />}
    </Card>
    <ProjectSkillsCard project={project} csrfToken={csrfToken} />
    <TokensCard csrfToken={csrfToken} />
  </div>;
}

// ProjectSkillsCard edits the ordered skills list for one project, stored in the
// skills.project.<project> setting. Runs compose it with the repository
// .machinist/skills.yaml and the global skills.default setting. Names are
// validated against the installed library, and Resolve now previews the
// composed list with the versions each run would load.
function ProjectSkillsCard({ project, csrfToken }) {
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
    {error && <div role="alert" className="whitespace-pre-wrap rounded-md border border-danger/35 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</div>}
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
async function loadSettings(signal) {
  const response = await fetch("/api/v1/settings", { headers: { Accept: "application/json" }, signal });
  if (!response.ok) throw new Error(`Settings request failed (${response.status})`);
  const body = await response.json().catch(() => ({}));
  return body.settings && typeof body.settings === "object" ? body.settings : {};
}

// saveSetting writes one setting value.
async function saveSetting(key, value, csrfToken) {
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

// TokensCard manages scoped API tokens: the machine clients (and MCP tools)
// use these bearer credentials. Write-scope tokens may queue, cancel and
// requeue jobs and change settings; read-scope tokens may only read.
function TokensCard({ csrfToken }) {
  const [tokens, setTokens] = useState([]);
  const [name, setName] = useState("");
  const [scope, setScope] = useState("read");
  const [secret, setSecret] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const [loaded, setLoaded] = useState(false);

  const refresh = async () => {
    try {
      setTokens(await loadTokens(csrfToken));
      setError("");
    } catch (loadError) {
      setError(loadError.message);
    } finally {
      setLoaded(true);
    }
  };

  useEffect(() => {
    refresh();
  }, []);

  const create = async () => {
    setBusy(true);
    setError("");
    setSecret("");
    try {
      const result = await createToken(csrfToken, { name, scope });
      setSecret(result.secret);
      setName("");
      await refresh();
    } catch (createError) {
      setError(createError.message);
    } finally {
      setBusy(false);
    }
  };

  const revoke = async (id) => {
    setBusy(true);
    setError("");
    try {
      await revokeToken(csrfToken, id);
      await refresh();
    } catch (revokeError) {
      setError(revokeError.message);
    } finally {
      setBusy(false);
    }
  };

  return <Card className="p-4 sm:p-5">
    <div className="space-y-1">
      <h2 className="font-medium">API tokens</h2>
      <p className="text-sm text-muted-foreground">Bearer credentials for machine clients and MCP tools. The secret is shown once; store it in your bot's environment, never in the repository.</p>
    </div>
    {error && <div role="alert" className="mt-3 whitespace-pre-wrap rounded-md border border-danger/35 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</div>}
    {secret && <div role="status" className="mt-3 space-y-1 rounded-md border border-success/35 bg-success/10 px-3 py-2 text-sm text-success">
      <div>New token secret (copy now, shown once):</div>
      <code className="block break-all font-mono text-xs">{secret}</code>
    </div>}
    <div className="mt-3 flex flex-wrap items-center gap-2">
      <input className="field-control w-56" value={name} onChange={(event) => setName(event.target.value)} placeholder="Token name" aria-label="Token name" />
      <select className="field-control w-32" value={scope} onChange={(event) => setScope(event.target.value)} aria-label="Token scope">
        <option value="read">read</option>
        <option value="write">write</option>
      </select>
      <Button disabled={busy || !name.trim()} onClick={create}>Create token</Button>
    </div>
    {loaded && <table className="mt-4 w-full text-sm">
      <thead>
        <tr className="text-left text-muted-foreground">
          <th className="py-1 pr-3 font-medium">Name</th>
          <th className="py-1 pr-3 font-medium">Scope</th>
          <th className="py-1 pr-3 font-medium">ID</th>
          <th className="py-1 pr-3 font-medium">Status</th>
          <th className="py-1 font-medium"><span className="sr-only">Actions</span></th>
        </tr>
      </thead>
      <tbody>
        {tokens.map((token) => <tr key={token.id} className="border-t">
          <td className="py-1 pr-3">{token.name}</td>
          <td className="py-1 pr-3"><Badge variant="outline">{token.scope}</Badge></td>
          <td className="py-1 pr-3 font-mono text-xs">{token.id}</td>
          <td className="py-1 pr-3">{token.revokedAt ? "revoked" : "active"}</td>
          <td className="py-1 text-right">
            {!token.revokedAt && <Button variant="outline" disabled={busy} onClick={() => revoke(token.id)}>Revoke</Button>}
          </td>
        </tr>)}
        {!tokens.length && <tr className="border-t"><td className="py-2 text-muted-foreground" colSpan={5}>No tokens yet.</td></tr>}
      </tbody>
    </table>}
  </Card>;
}

function PreviewResult({ preview }) {
  return <div className="space-y-2 text-sm">
    <div>Source: <strong>{preview.source}</strong> · Profile: <strong>{preview.name}</strong></div>
    {preview.notes?.map((note) => <div key={note} className="text-muted-foreground">{note}</div>)}
    {preview.lanes?.length > 0 && <div>
      <div className="font-medium">Lanes</div>
      <ul className="list-disc pl-5">
        {preview.lanes.map((lane) => <li key={lane.lane}>
          {lane.lane}: {lane.files.length} matching file{lane.files.length === 1 ? "" : "s"} · {lane.checks.length || 0} command{lane.checks.length === 1 ? "" : "s"}
        </li>)}
      </ul>
    </div>}
    {preview.reviewers?.length > 0 && <div>
      <div className="font-medium">Reviewer passes</div>
      <ul className="list-disc pl-5">
        {preview.reviewers.map((pass) => <li key={pass.name}>
          {pass.name}{pass.skills?.length ? ` (${pass.skills.join(", ")})` : ""} — {pass.reason}
        </li>)}
      </ul>
    </div>}
  </div>;
}
