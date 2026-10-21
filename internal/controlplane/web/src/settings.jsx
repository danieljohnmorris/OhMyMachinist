import { useEffect, useRef, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { PageHeading, QuietState } from "@/components/ui/page-heading";
import { Tabs } from "@/components/ui/tabs";
import { WorkflowsSection, WorkersSection } from "@/catalog";
import { FactorySection } from "@/factory";
import { SkillsSection } from "@/skills.jsx";
import { TriggersSection } from "@/triggers";
import { loadReviewProfile, loadReviewProfiles, previewReviewProfile, saveReviewDefaultProfile, saveReviewProfileSetting } from "@/review-settings";
import { createToken, loadTokens, revokeToken } from "@/api-tokens";
import { ProjectSkillsCard, saveSetting } from "@/project-skills-card";

// Settings: per-project review profile for the built-in PR review (MACH-32).
// The review runs from a resolved profile: repo .machinist/review.yaml at the
// PR base ref > this project setting > the global default > the shipped
// default profile.
export function SettingsConfiguration({ repositories = [], csrfToken }) {
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

  return <>
    <Card className="p-4 sm:p-5">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
        <label className="flex flex-1 items-center gap-2">
          <span className="text-sm font-medium">Project</span>
          <select className="field-control w-full max-w-xs" value={project} onChange={(event) => setProject(event.target.value)} aria-label="Project">
            {!repositories.length && <option value="">No repositories</option>}
            {repositories.map((name) => <option key={name} value={name}>{name}</option>)}
          </select>
        </label>
        {loaded && <Badge tone="surface">Resolved from: {source || "default"}</Badge>}
      </div>
      {error && <Alert className="mt-3">{error}</Alert>}
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
    <PlaneIntakeCard csrfToken={csrfToken} project={project} />
    <TokensCard csrfToken={csrfToken} />
  </>;
}

// PlaneIntakeCard edits the built-in Plane intake configuration. Every value
// is a "plane.*" setting read at request time by the intake, so changes apply
// to the next webhook, poll or sync without a restart.
const PLANE_TEXT_FIELDS = [
  ["plane.trigger_label", "Trigger label", "factory"],
  ["plane.approve_label", "Approve label", "approved"],
  ["plane.command", "Command", "build-pr"],
  ["plane.default_repo", "Default repository", ""],
  ["plane.ui_url", "Machinist UI URL", ""],
];

const PLANE_TOGGLE_FIELDS = [
  ["plane.auto_submit", "Auto-submit", "Queue a job when a ticket becomes eligible"],
  ["plane.start_comment", "Start comment", "Post a run link on the ticket when a job is queued"],
  ["plane.shadow", "Shadow mode", "Evaluate every event and record decisions without acting"],
];

const PLANE_JSON_FIELDS = [
  ["plane.repositories", "Project to repository map (JSON)"],
  ["plane.supervisors", "Ticket supervisor map (JSON)"],
  ["plane.merge_policies", "Per-project auto-merge + deploy policies (JSON: {\"MACH\": {\"auto_merge\": true, \"deploy_command\": \"…\", \"rollback_command\": \"…\", \"health_check\": {\"command\": \"…\" | \"url\": \"…\", \"status\": 200, \"expect\": \"…\"}}})"],
  ["plane.prompt_template", "Prompt template"],
];

function PlaneIntakeCard({ csrfToken, project = "" }) {
  const [values, setValues] = useState(null);
  const [raw, setRaw] = useState({});
  const [shadow, setShadow] = useState([]);
  const [scriptJobs, setScriptJobs] = useState([]);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const projectPromptKey = project ? `plane.prompt_template.${project}` : "";
  const [projectPrompt, setProjectPrompt] = useState("");

  useEffect(() => {
    const controller = new AbortController();
    fetch("/api/v1/plane/intake", { headers: { Accept: "application/json", "X-Machinist-CSRF": csrfToken }, signal: controller.signal })
      .then((response) => response.json().then((body) => {
        if (!response.ok) throw new Error(body.error || `Plane intake request failed (${response.status})`);
        const settings = body.settings && typeof body.settings === "object" ? body.settings : {};
        const next = {};
        for (const [key] of PLANE_TEXT_FIELDS) next[key] = settings[key] || "";
        for (const [key] of PLANE_TOGGLE_FIELDS) next[key] = settings[key] || "1";
        for (const [key] of PLANE_JSON_FIELDS) next[key] = settings[key] || "";
        setValues(next);
        setRaw(settings);
        setShadow(Array.isArray(body.shadow_decisions) ? body.shadow_decisions : []);
        setScriptJobs(Array.isArray(body.script_jobs) ? body.script_jobs : []);
      }))
      .catch((loadError) => { if (loadError.name !== "AbortError") setError(loadError.message); });
    return () => controller.abort();
  }, []);

  useEffect(() => {
    setProjectPrompt(projectPromptKey ? raw[projectPromptKey] || "" : "");
  }, [projectPromptKey, raw]);

  const update = (key, value) => setValues((current) => ({ ...current, [key]: value }));

  const save = async () => {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      for (const [key] of [...PLANE_TEXT_FIELDS, ...PLANE_TOGGLE_FIELDS, ...PLANE_JSON_FIELDS]) {
        await saveSetting(key, values[key] || "", csrfToken);
      }
      if (projectPromptKey) await saveSetting(projectPromptKey, projectPrompt, csrfToken);
      setNotice("Saved. Intake reads these settings on the next webhook, poll or sync.");
    } catch (saveError) {
      setError(saveError.message);
    } finally {
      setBusy(false);
    }
  };

  return <Card className="p-4 sm:p-5">
    <div className="space-y-1">
      <h2 className="font-medium">Plane intake</h2>
      <p className="text-sm text-muted-foreground">Webhook and poll intake for Plane tickets. Shadow mode records what intake would do without creating jobs, patching tickets or writing alerts.</p>
    </div>
    {error && <Alert className="mt-3">{error}</Alert>}
    {notice && <div role="status" className="mt-3 rounded-md border border-success/35 bg-success/10 px-3 py-2 text-sm text-success">{notice}</div>}
    {!values ? <QuietState title="Loading Plane intake" description="Fetching the stored intake settings." role="status" />
      : <>
        <div className="mt-4 grid gap-3 sm:grid-cols-2">
          {PLANE_TEXT_FIELDS.map(([key, label, placeholder]) => <label key={key} className="space-y-1">
            <span className="field-label">{label}</span>
            <input className="field-control" value={values[key]} placeholder={placeholder} onChange={(event) => update(key, event.target.value)} aria-label={label} />
          </label>)}
        </div>
        <div className="mt-4 flex flex-wrap gap-4">
          {PLANE_TOGGLE_FIELDS.map(([key, label, hint]) => <label key={key} className="flex items-center gap-2" title={hint}>
            <input type="checkbox" className="size-4 accent-primary" checked={values[key] === "1"} onChange={(event) => update(key, event.target.checked ? "1" : "0")} aria-label={label} />
            <span className="text-sm">{label}</span>
          </label>)}
        </div>
        <div className="mt-4 grid gap-3">
          {PLANE_JSON_FIELDS.filter(([key]) => key !== "plane.prompt_template").map(([key, label]) => <label key={key} className="space-y-1">
            <span className="field-label">{label}</span>
            <textarea className="field-control min-h-24 resize-y font-mono text-xs" value={values[key]} spellCheck="false" onChange={(event) => update(key, event.target.value)} aria-label={label} />
          </label>)}
          {project && <label className="space-y-1">
            <span className="field-label">Prompt template for {project} (blank uses the template above)</span>
            <textarea className="field-control min-h-24 resize-y font-mono text-xs" value={projectPrompt} spellCheck="false" onChange={(event) => setProjectPrompt(event.target.value)} aria-label={`Prompt template for ${project}`} />
          </label>}
          <label className="space-y-1">
            <span className="field-label">Prompt template (all projects)</span>
            <textarea className="field-control min-h-24 resize-y font-mono text-xs" value={values["plane.prompt_template"]} spellCheck="false" onChange={(event) => update("plane.prompt_template", event.target.value)} aria-label="Prompt template" />
          </label>
        </div>
        <div className="mt-3">
          <Button disabled={busy} onClick={save}>{busy ? "Saving…" : "Save intake settings"}</Button>
        </div>
        <div className="mt-4 space-y-3">
          <div>
            <h3 className="text-sm font-medium">Shadow decisions</h3>
            <p className="text-xs text-muted-foreground">What intake would create, patch and alert on, newest first. Compare against the imported script jobs below for the cutover report.</p>
          </div>
          {shadow.length === 0 ? <p className="text-sm text-muted-foreground">No shadow decisions recorded yet.</p>
            : <div className="overflow-x-auto"><table className="w-full min-w-[28rem] border-collapse text-sm">
              <thead><tr className="border-b border-border text-left text-xs uppercase tracking-wide text-muted-foreground">
                <th className="py-2 pr-4 font-medium">Kind</th>
                <th className="py-2 pr-4 font-medium">Ticket</th>
                <th className="py-2 pr-4 font-medium">Decision</th>
                <th className="py-2 font-medium">At</th>
              </tr></thead>
              <tbody>
                {shadow.map((decision) => <tr key={decision.id} className="border-b border-border last:border-b-0">
                  <td className="py-2 pr-4">{decision.kind}</td>
                  <td className="py-2 pr-4 font-mono text-xs">{decision.key}</td>
                  <td className="py-2 pr-4 break-all font-mono text-xs">{decision.decision}</td>
                  <td className="py-2">{decision.at}</td>
                </tr>)}
              </tbody>
            </table></div>}
        </div>
        {scriptJobs.length > 0 && <div className="mt-4">
          <h3 className="text-sm font-medium">Imported script jobs</h3>
          <ul className="mt-1 space-y-1 text-xs text-muted-foreground">
            {scriptJobs.map((job) => <li key={`${job.key}:${job.decision}`} className="font-mono">{job.key} — {job.decision}</li>)}
          </ul>
        </div>}
      </>}
  </Card>;
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
    {error && <Alert className="mt-3">{error}</Alert>}
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
          <td className="py-1 pr-3"><Badge tone="surface">{token.scope}</Badge></td>
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

// Settings gathers configuration that used to be top-level pages: the review
// profile and project settings, workers, triggers, the factory, workflows and
// the skills library. The section lives in the hash, so every tab is a link.
export function SettingsPage({ section = "review", onSectionChange, repositories = [], csrfToken, workers = [], triggers = [], factory, loaded = false, error = "", onChanged }) {
  return <div className="mx-auto max-w-[1500px] space-y-6 p-4 sm:p-6 lg:p-8">
    <PageHeading title="Settings" description="Review profiles, workers, triggers, factory controls, workflows and skills for this shop." />
    <Tabs
      label="Settings sections"
      selected={section}
      onSelect={(next) => onSectionChange?.(next)}
      items={[
        { id: "review", label: "Review", content: <SettingsConfiguration repositories={repositories} csrfToken={csrfToken} /> },
        { id: "workers", label: "Workers", content: <WorkersSection workers={workers} loaded={loaded} error={error} csrfToken={csrfToken} onChanged={onChanged} /> },
        { id: "triggers", label: "Triggers", content: <TriggersSection triggers={triggers} loaded={loaded} error={error} /> },
        { id: "factory", label: "Factory", content: <FactorySection factory={factory} csrfToken={csrfToken} onChanged={onChanged} loaded={loaded} error={error} /> },
        { id: "workflows", label: "Workflows", content: <WorkflowsSection /> },
        { id: "skills", label: "Skills", content: <SkillsSection csrfToken={csrfToken} /> },
      ]}
    />
  </div>;
}
