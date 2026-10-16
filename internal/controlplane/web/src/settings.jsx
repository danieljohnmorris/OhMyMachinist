import { useEffect, useRef, useState } from "react";
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
import {
  loadWorkflow,
  saveWorkflow,
  sessionOptions,
  stageKindOptions,
  workflowFromPayload,
} from "@/workflow-settings";

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
    <WorkflowsCard project={project} csrfToken={csrfToken} />
    <TokensCard csrfToken={csrfToken} />
  </div>;
}

// WorkflowsCard edits a project's configurable workflow stages: job types, each
// type's ordered stages (agent, model, session mode, human approval, review
// profile), the default type and the Plane label mapping. The same config is
// readable and writable through the HTTP API and MCP.
function WorkflowsCard({ project, csrfToken }) {
  const [workflow, setWorkflow] = useState(() => workflowFromPayload(null));
  const [selected, setSelected] = useState("");
  const [loaded, setLoaded] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const loadSequence = useRef(0);

  useEffect(() => {
    if (!project) {
      setLoaded(false);
      return undefined;
    }
    const sequence = ++loadSequence.current;
    const controller = new AbortController();
    setLoaded(false);
    setError("");
    setNotice("");
    loadWorkflow(project, controller.signal)
      .then((result) => {
        if (loadSequence.current !== sequence) return;
        setWorkflow(result);
        setSelected(result.jobTypes[0]?.name || "");
        setLoaded(true);
      })
      .catch((loadError) => {
        if (loadError.name === "AbortError" || loadSequence.current !== sequence) return;
        setError(loadError.message);
        setLoaded(true);
      });
    return () => controller.abort();
  }, [project]);

  const selectedType = workflow.jobTypes.find((jobType) => jobType.name === selected) || null;

  const updateType = (name, mutate) => {
    setWorkflow((current) => ({
      ...current,
      jobTypes: current.jobTypes.map((jobType) => (jobType.name === name ? mutate(jobType) : jobType)),
    }));
  };

  const updateStage = (index, mutate) => {
    if (!selectedType) return;
    updateType(selectedType.name, (jobType) => ({
      ...jobType,
      stages: jobType.stages.map((stage, stageIndex) => (stageIndex === index ? mutate(stage) : stage)),
    }));
  };

  const moveStage = (index, delta) => {
    if (!selectedType) return;
    updateType(selectedType.name, (jobType) => {
      const stages = [...jobType.stages];
      const target = index + delta;
      if (target < 0 || target >= stages.length) return jobType;
      [stages[index], stages[target]] = [stages[target], stages[index]];
      return { ...jobType, stages };
    });
  };

  const addStage = () => {
    if (!selectedType) return;
    updateType(selectedType.name, (jobType) => ({
      ...jobType,
      stages: [...jobType.stages, { id: "coding", name: "Coding", kind: "coding", agent: "", model: "", session: "continue", humanApproval: false, reviewProfile: "" }],
    }));
  };

  const removeStage = (index) => {
    if (!selectedType) return;
    updateType(selectedType.name, (jobType) => ({ ...jobType, stages: jobType.stages.filter((_, stageIndex) => stageIndex !== index) }));
  };

  const addType = () => {
    let index = workflow.jobTypes.length + 1;
    let name = `type_${index}`;
    while (workflow.jobTypes.some((jobType) => jobType.name === name)) {
      index += 1;
      name = `type_${index}`;
    }
    setWorkflow((current) => ({ ...current, jobTypes: [...current.jobTypes, { name, stages: [{ id: "queued", name: "Queued", kind: "queued", agent: "", model: "", session: "continue", humanApproval: false, reviewProfile: "" }, { id: "finished", name: "Finished", kind: "finished", agent: "", model: "", session: "continue", humanApproval: false, reviewProfile: "" }] }] }));
    setSelected(name);
  };

  const renameType = (name, next) => {
    setWorkflow((current) => ({
      ...current,
      defaultType: current.defaultType === name ? next : current.defaultType,
      jobTypes: current.jobTypes.map((jobType) => (jobType.name === name ? { ...jobType, name: next } : jobType)),
      labelMappings: current.labelMappings.map((mapping) => (mapping.type === name ? { ...mapping, type: next } : mapping)),
    }));
    setSelected(next);
  };

  const removeType = (name) => {
    setWorkflow((current) => {
      const jobTypes = current.jobTypes.filter((jobType) => jobType.name !== name);
      const defaultType = current.defaultType === name ? jobTypes[0]?.name || "" : current.defaultType;
      return { ...current, jobTypes, defaultType, labelMappings: current.labelMappings.filter((mapping) => mapping.type !== name) };
    });
    setSelected((current) => (current === name ? workflow.jobTypes.find((jobType) => jobType.name !== name)?.name || "" : current));
  };

  const save = async () => {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const saved = await saveWorkflow(project, workflow, csrfToken);
      setWorkflow(saved);
      setSelected((current) => (saved.jobTypes.some((jobType) => jobType.name === current) ? current : saved.jobTypes[0]?.name || ""));
      setNotice("Saved. New cards for this project use this configuration.");
    } catch (saveError) {
      setError(saveError.message);
    } finally {
      setBusy(false);
    }
  };

  return <Card className="p-4 sm:p-5">
    <div className="space-y-1">
      <h2 className="font-medium">Workflow stages</h2>
      <p className="text-sm text-muted-foreground">Job types and their ordered stages for this project. Each card picks a type from a Plane label (e.g. type:config) or the default below. Stages set the agent, model and session mode; the built-in review runs automatically and UAT waits for a human before a merged pull request or a Done ticket finishes the card.</p>
    </div>
    {error && <div role="alert" className="mt-3 whitespace-pre-wrap rounded-md border border-danger/35 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</div>}
    {notice && <div role="status" className="mt-3 rounded-md border border-success/35 bg-success/10 px-3 py-2 text-sm text-success">{notice}</div>}
    {!loaded && <QuietState title={project ? "Loading workflow stages" : "No repositories yet"} description={project ? "Fetching this project's job types." : "Register a worker repository to edit its workflow."} role="status" />}
    {loaded && <div className="mt-4 space-y-4">
      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1">
          <span className="text-sm font-medium">Default job type</span>
          <select className="field-control w-full max-w-xs" value={workflow.defaultType} onChange={(event) => setWorkflow((current) => ({ ...current, defaultType: event.target.value }))} aria-label="Default job type">
            {workflow.jobTypes.map((jobType) => <option key={jobType.name} value={jobType.name}>{jobType.name}</option>)}
          </select>
        </label>
        <Button variant="outline" onClick={addType} disabled={busy}>Add job type</Button>
      </div>

      <div className="flex flex-wrap gap-2" role="tablist" aria-label="Job types">
        {workflow.jobTypes.map((jobType) => <Button key={jobType.name} size="sm" variant={jobType.name === selected ? "default" : "outline"} aria-pressed={jobType.name === selected} onClick={() => setSelected(jobType.name)}>{jobType.name}</Button>)}
      </div>

      {selectedType && <div className="space-y-3 rounded-md border p-3">
        <div className="flex flex-wrap items-end gap-3">
          <label className="flex flex-col gap-1">
            <span className="text-sm font-medium">Job type name</span>
            <input className="field-control w-56" value={selectedType.name} onChange={(event) => renameType(selectedType.name, event.target.value)} aria-label="Job type name" />
          </label>
          <Button variant="outline" disabled={busy || workflow.jobTypes.length < 2} onClick={() => removeType(selectedType.name)}>Delete job type</Button>
        </div>

        <div className="space-y-2">
          <div className="flex items-center justify-between">
            <span className="text-sm font-medium">Stages</span>
            <Button size="sm" variant="outline" onClick={addStage} disabled={busy}>Add stage</Button>
          </div>
          {selectedType.stages.map((stage, index) => <div key={index} className="grid gap-2 rounded-md border p-2 sm:grid-cols-2 lg:grid-cols-4">
            <label className="flex flex-col gap-1"><span className="text-xs text-muted-foreground">Name</span><input className="field-control" value={stage.name} onChange={(event) => updateStage(index, (current) => ({ ...current, name: event.target.value }))} aria-label={`Stage ${index + 1} name`} /></label>
            <label className="flex flex-col gap-1"><span className="text-xs text-muted-foreground">Kind</span><select className="field-control" value={stage.kind} onChange={(event) => updateStage(index, (current) => ({ ...current, kind: event.target.value }))} aria-label={`Stage ${index + 1} kind`}>{stageKindOptions.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</select></label>
            <label className="flex flex-col gap-1"><span className="text-xs text-muted-foreground">Agent</span><input className="field-control" value={stage.agent} onChange={(event) => updateStage(index, (current) => ({ ...current, agent: event.target.value }))} placeholder="Project default" aria-label={`Stage ${index + 1} agent`} /></label>
            <label className="flex flex-col gap-1"><span className="text-xs text-muted-foreground">Model</span><input className="field-control" value={stage.model} onChange={(event) => updateStage(index, (current) => ({ ...current, model: event.target.value }))} placeholder="Project default" aria-label={`Stage ${index + 1} model`} /></label>
            <label className="flex flex-col gap-1"><span className="text-xs text-muted-foreground">Session</span><select className="field-control" value={stage.session || "continue"} onChange={(event) => updateStage(index, (current) => ({ ...current, session: event.target.value }))} aria-label={`Stage ${index + 1} session`}>{sessionOptions.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</select></label>
            <label className="flex flex-col gap-1"><span className="text-xs text-muted-foreground">Review profile</span><input className="field-control" value={stage.reviewProfile} onChange={(event) => updateStage(index, (current) => ({ ...current, reviewProfile: event.target.value }))} placeholder="Default" aria-label={`Stage ${index + 1} review profile`} /></label>
            <label className="flex items-center gap-2 pt-5 text-sm"><input type="checkbox" checked={stage.humanApproval} onChange={(event) => updateStage(index, (current) => ({ ...current, humanApproval: event.target.checked }))} aria-label={`Stage ${index + 1} needs human approval`} />Human approval</label>
            <div className="flex items-end gap-1">
              <Button size="sm" variant="ghost" disabled={index === 0} aria-label={`Move stage ${index + 1} earlier`} onClick={() => moveStage(index, -1)}>↑</Button>
              <Button size="sm" variant="ghost" disabled={index === selectedType.stages.length - 1} aria-label={`Move stage ${index + 1} later`} onClick={() => moveStage(index, 1)}>↓</Button>
              <Button size="sm" variant="outline" disabled={selectedType.stages.length < 2} aria-label={`Remove stage ${index + 1}`} onClick={() => removeStage(index)}>Remove</Button>
            </div>
          </div>)}
        </div>
      </div>}

      <div className="space-y-2">
        <div className="flex items-center justify-between">
          <span className="text-sm font-medium">Plane label mapping</span>
          <Button size="sm" variant="outline" onClick={() => setWorkflow((current) => ({ ...current, labelMappings: [...current.labelMappings, { label: "", type: current.defaultType }] }))} disabled={busy}>Add mapping</Button>
        </div>
        {workflow.labelMappings.map((mapping, index) => <div key={index} className="flex flex-wrap items-center gap-2">
          <input className="field-control w-56" value={mapping.label} placeholder="type:config" onChange={(event) => setWorkflow((current) => ({ ...current, labelMappings: current.labelMappings.map((item, itemIndex) => (itemIndex === index ? { ...item, label: event.target.value } : item)) }))} aria-label={`Label mapping ${index + 1} label`} />
          <span aria-hidden="true">→</span>
          <select className="field-control w-56" value={mapping.type} onChange={(event) => setWorkflow((current) => ({ ...current, labelMappings: current.labelMappings.map((item, itemIndex) => (itemIndex === index ? { ...item, type: event.target.value } : item)) }))} aria-label={`Label mapping ${index + 1} job type`}>
            {workflow.jobTypes.map((jobType) => <option key={jobType.name} value={jobType.name}>{jobType.name}</option>)}
          </select>
          <Button size="sm" variant="outline" onClick={() => setWorkflow((current) => ({ ...current, labelMappings: current.labelMappings.filter((_, itemIndex) => itemIndex !== index) }))}>Remove</Button>
        </div>)}
        {!workflow.labelMappings.length && <p className="text-xs text-muted-foreground">No mappings. Add one to route e.g. type:config cards to a non-standard job type.</p>}
      </div>

      <div className="flex flex-wrap items-center gap-3">
        <Button disabled={busy || !project} onClick={save}>Save workflow</Button>
        <span className="text-xs text-muted-foreground">Validated before saving and recorded in the audit log.</span>
      </div>
    </div>}
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
