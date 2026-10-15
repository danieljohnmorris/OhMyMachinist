import { useEffect, useRef, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { PageHeading, QuietState } from "@/components/ui/page-heading";
import {
  loadFactoryCatalog,
  loadFactorySetting,
  resetFactorySetting,
  saveFactorySetting,
} from "@/factory-settings";
import {
  loadReviewProfile,
  loadReviewProfiles,
  previewReviewProfile,
  saveReviewDefaultProfile,
  saveReviewProfileSetting,
} from "@/review-settings";
import { createToken, loadTokens, revokeToken } from "@/api-tokens";

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
    <FactoryCard project={project} csrfToken={csrfToken} />
    <TokensCard csrfToken={csrfToken} />
  </div>;
}

// FactoryCard edits the pluggable factory setup (MACH-37): the tracker, the
// coding and review agents and the per-role models. Empty fields keep whatever
// the trigger or command configured, so an untouched card changes nothing.
const EMPTY_FACTORY_SETTING = {
  tracker: "",
  codingAgent: { kind: "", executor: "", command: "" },
  reviewAgent: { kind: "", executor: "", command: "" },
  models: {
    coding: { model: "", effort: "", endpoint: "", url: "" },
    review: { model: "", effort: "", endpoint: "", url: "" },
    non_coding: { model: "", effort: "", endpoint: "", url: "" },
  },
};

const MODEL_ROLES = ["coding", "review", "non_coding"];

const MODEL_ROLE_LABELS = { coding: "Coding", review: "Review", non_coding: "Non-coding" };

function FactoryCard({ project, csrfToken }) {
  const [setting, setSetting] = useState(EMPTY_FACTORY_SETTING);
  const [catalog, setCatalog] = useState({ trackers: [], agents: [], roles: [], endpoints: [] });
  const [source, setSource] = useState("");
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const loadSequence = useRef(0);

  useEffect(() => {
    const controller = new AbortController();
    loadFactoryCatalog(controller.signal)
      .then((result) => setCatalog(result))
      .catch(() => {});
    return () => controller.abort();
  }, []);

  useEffect(() => {
    if (!project) return;
    const sequence = ++loadSequence.current;
    const controller = new AbortController();
    setLoaded(false);
    setError("");
    setNotice("");
    loadFactorySetting(project, controller.signal)
      .then((result) => {
        if (loadSequence.current !== sequence) return;
        setSetting(result.setting);
        setSource(result.source);
        setLoaded(true);
      })
      .catch((loadError) => {
        if (loadError.name === "AbortError" || loadSequence.current !== sequence) return;
        setError(loadError.message);
        setLoaded(true);
      });
    return () => controller.abort();
  }, [project]);

  const patchAgent = (name, changes) => setSetting((current) => ({ ...current, [name]: { ...current[name], ...changes } }));

  const patchModel = (role, changes) => setSetting((current) => ({
    ...current,
    models: { ...current.models, [role]: { ...(current.models[role] || {}), ...changes } },
  }));

  const save = async () => {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const result = await saveFactorySetting(project, setting, csrfToken);
      setSetting(result.setting);
      setSource(result.source);
      setNotice(`Saved. This project now resolves its factory setup from: ${result.source}.`);
    } catch (saveError) {
      setError(saveError.message);
    } finally {
      setBusy(false);
    }
  };

  const revert = async () => {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const result = await resetFactorySetting(project, csrfToken);
      setSetting(result.setting);
      setSource(result.source);
      setNotice(`Reverted. This project now resolves its factory setup from: ${result.source}.`);
    } catch (resetError) {
      setError(resetError.message);
    } finally {
      setBusy(false);
    }
  };

  const roles = catalog.roles.length ? catalog.roles : MODEL_ROLES;
  const roleLabel = (role) => MODEL_ROLE_LABELS[role] || role;

  return <Card className="p-4 sm:p-5">
    <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
      <div className="flex-1 space-y-1">
        <h2 className="font-medium">Factory setup</h2>
        <p className="text-sm text-muted-foreground">The tracker, the coding and review agents and the per-role models this project runs with. Empty fields keep whatever the trigger or command configured.</p>
      </div>
      {loaded && <Badge variant="outline" className="text-muted-foreground">Resolved from: {source || "built-in"}</Badge>}
    </div>
    {error && <div role="alert" className="mt-3 whitespace-pre-wrap rounded-md border border-danger/35 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</div>}
    {notice && <div role="status" className="mt-3 rounded-md border border-success/35 bg-success/10 px-3 py-2 text-sm text-success">{notice}</div>}
    {loaded && <div className="mt-4 space-y-4">
      <label className="flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium">Tracker</span>
        <select
          className="field-control w-full max-w-xs"
          value={setting.tracker}
          onChange={(event) => setSetting((current) => ({ ...current, tracker: event.target.value }))}
          aria-label="Factory tracker"
        >
          <option value="">(keep trigger default)</option>
          {catalog.trackers.map((tracker) => <option key={tracker} value={tracker}>{tracker}</option>)}
        </select>
      </label>
      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1">
          <span className="text-sm font-medium">Coding agent</span>
          <select
            className="field-control w-full max-w-xs"
            value={setting.codingAgent.kind}
            onChange={(event) => patchAgent("codingAgent", { kind: event.target.value })}
            aria-label="Coding agent kind"
          >
            <option value="">(keep trigger default)</option>
            {catalog.agents.map((agent) => <option key={agent} value={agent}>{agent}</option>)}
          </select>
        </label>
        <label className="flex min-w-56 flex-1 flex-col gap-1">
          <span className="text-sm font-medium">Coding executor</span>
          <input
            className="field-control w-full"
            value={setting.codingAgent.executor}
            onChange={(event) => patchAgent("codingAgent", { executor: event.target.value })}
            aria-label="Coding agent executor"
            placeholder="worker executor name"
          />
        </label>
      </div>
      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1">
          <span className="text-sm font-medium">Review agent</span>
          <select
            className="field-control w-full max-w-xs"
            value={setting.reviewAgent.kind}
            onChange={(event) => patchAgent("reviewAgent", { kind: event.target.value })}
            aria-label="Review agent kind"
          >
            <option value="">(keep trigger default)</option>
            {catalog.agents.map((agent) => <option key={agent} value={agent}>{agent}</option>)}
          </select>
        </label>
        <label className="flex min-w-56 flex-1 flex-col gap-1">
          <span className="text-sm font-medium">Review command</span>
          <input
            className="field-control w-full font-mono text-xs"
            value={setting.reviewAgent.command}
            onChange={(event) => patchAgent("reviewAgent", { command: event.target.value })}
            aria-label="Review agent command"
            placeholder="codex exec --model {{model}}"
            spellCheck="false"
          />
        </label>
      </div>
      <div className="space-y-3 rounded-md border p-3">
        <div className="text-sm font-medium">Models</div>
        {roles.map((role) => <div key={role} className="flex flex-wrap items-end gap-3">
          <label className="flex min-w-40 flex-1 flex-col gap-1">
            <span className="text-xs text-muted-foreground">{roleLabel(role)} model</span>
            <input
              className="field-control w-full"
              value={setting.models[role]?.model ?? ""}
              onChange={(event) => patchModel(role, { model: event.target.value })}
              aria-label={`${roleLabel(role)} model`}
            />
          </label>
          <label className="flex flex-col gap-1">
            <span className="text-xs text-muted-foreground">Effort</span>
            <input
              className="field-control w-32"
              value={setting.models[role]?.effort ?? ""}
              onChange={(event) => patchModel(role, { effort: event.target.value })}
              aria-label={`${roleLabel(role)} effort`}
              placeholder="high"
            />
          </label>
          <label className="flex flex-col gap-1">
            <span className="text-xs text-muted-foreground">Endpoint</span>
            <select
              className="field-control w-32"
              value={setting.models[role]?.endpoint ?? ""}
              onChange={(event) => patchModel(role, { endpoint: event.target.value })}
              aria-label={`${roleLabel(role)} endpoint`}
            >
              <option value="">(unset)</option>
              {catalog.endpoints.map((endpoint) => <option key={endpoint} value={endpoint}>{endpoint}</option>)}
            </select>
          </label>
          <label className="flex min-w-40 flex-1 flex-col gap-1">
            <span className="text-xs text-muted-foreground">URL (local endpoints)</span>
            <input
              className="field-control w-full"
              value={setting.models[role]?.url ?? ""}
              onChange={(event) => patchModel(role, { url: event.target.value })}
              aria-label={`${roleLabel(role)} endpoint URL`}
              placeholder="http://127.0.0.1:8080"
            />
          </label>
        </div>)}
      </div>
      <div className="flex flex-wrap gap-2">
        <Button disabled={busy || !project} onClick={save}>Save</Button>
        <Button variant="outline" disabled={busy || !project} onClick={revert}>Revert to default</Button>
      </div>
      <p className="text-xs text-muted-foreground">Precedence: this project's setting wins, then the global default, then the control-plane config, then the built-in empty setup. Every empty field falls back to the agent and model the trigger or command already uses, so a local endpoint needs its URL.</p>
    </div>}
    {!loaded && <QuietState title={project ? "Loading factory setup" : "No repositories yet"} description={project ? "Fetching the resolved factory setup for this project." : "Register a worker repository to edit its factory setup."} role="status" />}
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
