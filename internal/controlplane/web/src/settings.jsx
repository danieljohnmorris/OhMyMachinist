import { useEffect, useRef, useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { PageHeading, QuietState } from "@/components/ui/page-heading";
import { loadReviewInstructions, saveReviewInstructions } from "@/review-settings";

// Settings: per-project review instructions for the built-in PR review step.
// The markdown is stored in Machinist and passed to the review passes.
export function SettingsPage({ repositories = [], csrfToken }) {
  const [project, setProject] = useState("");
  const [text, setText] = useState("");
  const [custom, setCustom] = useState(false);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [saving, setSaving] = useState(false);
  const loadSequence = useRef(0);

  useEffect(() => {
    if (project || !repositories.length) return;
    setProject(repositories[0]);
  }, [project, repositories]);

  useEffect(() => {
    if (!project) return;
    const sequence = ++loadSequence.current;
    const controller = new AbortController();
    setLoaded(false);
    setError("");
    setNotice("");
    loadReviewInstructions(project, controller.signal)
      .then((result) => {
        if (loadSequence.current !== sequence) return;
        setText(result.instructions);
        setCustom(result.custom);
        setLoaded(true);
      })
      .catch((loadError) => {
        if (loadError.name === "AbortError" || loadSequence.current !== sequence) return;
        setError(loadError.message);
        setLoaded(true);
      });
    return () => controller.abort();
  }, [project]);

  const save = async (instructions) => {
    setSaving(true);
    setError("");
    setNotice("");
    try {
      const result = await saveReviewInstructions(project, instructions, csrfToken);
      setText(result.instructions);
      setCustom(result.custom);
      setNotice(result.custom ? "Saved for this project." : "Reverted to the default instructions.");
    } catch (saveError) {
      setError(saveError.message);
    } finally {
      setSaving(false);
    }
  };

  return <div className="mx-auto max-w-[1500px] space-y-6 p-4 sm:p-6 lg:p-8">
    <PageHeading title="Settings" description="Per-project instructions for the built-in pull-request review. The review runs after every PR opens: checks, parallel reviewers, then a docs pass." />
    <Card className="p-4 sm:p-5">
      <div className="flex flex-col gap-3 sm:flex-row sm:items-center">
        <label className="flex flex-1 items-center gap-2">
          <span className="text-sm font-medium">Project</span>
          <select className="field-control w-full max-w-xs" value={project} onChange={(event) => setProject(event.target.value)} aria-label="Project">
            {!repositories.length && <option value="">No repositories</option>}
            {repositories.map((name) => <option key={name} value={name}>{name}</option>)}
          </select>
        </label>
        {loaded && <Badge variant="outline" className={custom ? "" : "text-muted-foreground"}>{custom ? "Custom" : "Default"}</Badge>}
      </div>
      {error && <div role="alert" className="mt-3 rounded-md border border-danger/35 bg-danger/10 px-3 py-2 text-sm text-danger">{error}</div>}
      {notice && <div role="status" className="mt-3 rounded-md border border-success/35 bg-success/10 px-3 py-2 text-sm text-success">{notice}</div>}
      {loaded && <div className="mt-4 space-y-3">
        <label className="block space-y-2">
          <span className="text-sm font-medium">Review instructions (markdown)</span>
          <textarea
            className="field-control min-h-80 resize-y font-mono text-xs"
            value={text}
            onChange={(event) => setText(event.target.value)}
            aria-label="Review instructions markdown"
            spellCheck="false"
          />
        </label>
        <div className="flex flex-wrap gap-2">
          <Button disabled={saving || !project} onClick={() => save(text)}>Save</Button>
          {custom && <Button variant="outline" disabled={saving || !project} onClick={() => save("")}>Revert to default</Button>}
        </div>
        <p className="text-xs text-muted-foreground">Blocking findings send the change back to amend on the same branch; each amend round counts against the retry budget. Auto-merge requires a passing review.</p>
      </div>}
      {!loaded && <QuietState title={project ? "Loading review instructions" : "No repositories yet"} description={project ? "Fetching the markdown for this project." : "Register a worker repository to edit its review instructions."} role="status" />}
    </Card>
  </div>;
}
