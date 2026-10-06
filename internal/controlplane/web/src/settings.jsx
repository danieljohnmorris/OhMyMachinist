import { Moon, Sun } from "lucide-react";
import { Usage } from "@/analytics";
import { TemplateHelp, useDefinitions } from "@/catalog";
import { Button } from "@/components/ui/button";
import { ErrorBanner, TopBar } from "@/components/ui/page-heading";
import { Spinner } from "@/components/ui/status-icon";
import { humanize } from "@/runs-board";

export function SettingsPage({ status, loaded, error, dark, setDark }) {
  const definitions = useDefinitions();
  const data = definitions.value;
  const workflows = Object.entries(data.workflows || {});
  return <>
    <TopBar title="Settings" />
    {error && <ErrorBanner>{error}</ErrorBanner>}
    <div className="pane-scroll">
      <Usage jobs={status.jobs} loaded={loaded} error={error} />

      <section aria-label="Repositories">
        <h2 className="group-header">Repositories<span className="font-normal text-faint">{status.repositories?.length || 0}</span></h2>
        {status.repositories?.length ? status.repositories.map((name) => <div key={name} className="row"><span className="row-title">{name}</span></div>)
          : <p className="row text-faint">{loaded ? "No repositories. Add [repositories.NAME] to a worker's worker.toml." : "Loading"}</p>}
      </section>

      <section aria-label="Agents">
        <h2 className="group-header">Agents<span className="font-normal text-faint">Defined in config.toml</span></h2>
        {definitions.loading ? <p className="row text-faint"><Spinner />Loading the latest configuration.</p>
          : definitions.error ? <p role="alert" className="row text-danger">{definitions.error}</p>
          : data.commands.length ? data.commands.map((command) => <details key={command.name} className="border-b border-border">
            <summary className="row cursor-pointer border-b-0"><span className="row-main"><span className="row-title">{command.name}</span><span className="row-reason font-mono">{command.executor} · {command.timeout}</span></span></summary>
            <pre tabIndex={0} aria-label={`${command.name} prompt template`} className="log-block mx-4 mb-3 max-h-80 overflow-auto">{command.prompt || "Uses the task instructions directly."}</pre>
          </details>)
          : <p className="row text-faint">Configuration added on the control plane will appear here.</p>}
      </section>

      {workflows.length > 0 && <section aria-label="Workflows">
        <h2 className="group-header">Workflows<span className="font-normal text-faint">Steps run in order</span></h2>
        {workflows.map(([name, steps]) => <div key={name} className="row"><span className="row-main"><span className="row-title">{humanize(name)}</span><span className="row-reason">{steps.map((step) => `${humanize(step.name)}${step.approval ? " (approval first)" : ""}`).join(" → ")}</span></span></div>)}
      </section>}

      <details className="border-b border-border">
        <summary className="group-header static cursor-pointer">Prompt template variables</summary>
        <div className="px-4 py-3"><TemplateHelp /></div>
      </details>

      <section aria-label="Appearance">
        <h2 className="group-header">Appearance</h2>
        <div className="row"><span className="row-title flex-1">Theme</span><Button variant="outline" onClick={() => setDark((value) => !value)} aria-label={`Switch to ${dark ? "light" : "dark"} theme`}>{dark ? <Moon className="size-3.5" /> : <Sun className="size-3.5" />}{dark ? "Dark" : "Light"}</Button></div>
      </section>
    </div>
  </>;
}
