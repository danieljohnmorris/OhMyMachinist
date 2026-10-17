import { useEffect, useState } from "react";
import { PackagePlus, Pin, RefreshCw, Trash2, Upload } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Alert } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { Card } from "@/components/ui/card";
import { PageHeading, QuietState } from "@/components/ui/page-heading";
import { formatTimestamp } from "./task-display.jsx";
import { installSkill, loadSkills, pinSkill, shortVersion, skillInstalledAt, skillUpdatedAt, uninstallSkill, updateSkill, uploadSkill } from "./skills.js";

// Skills page (MACH-51): the installed versioned skills library. Install from a
// URL, directory or zip (or upload a .zip/SKILL.md), update to the recorded
// source's latest version, pin/rollback the active version, and uninstall.
export function SkillsPage({ csrfToken }) {
  const [skills, setSkills] = useState([]);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [source, setSource] = useState("");
  const [file, setFile] = useState(null);
  const [busy, setBusy] = useState("");
  const [versions, setVersions] = useState({});

  const refresh = async () => {
    const result = await loadSkills();
    setSkills(result);
    setLoaded(true);
  };

  useEffect(() => {
    let active = true;
    loadSkills()
      .then((result) => {
        if (!active) return;
        setSkills(result);
        setLoaded(true);
        setError("");
      })
      .catch((loadError) => {
        if (!active) return;
        setError(loadError.message);
        setLoaded(true);
      });
    return () => {
      active = false;
    };
  }, []);

  const run = async (label, action) => {
    setBusy(label);
    setError("");
    setNotice("");
    try {
      const message = await action();
      await refresh();
      setNotice(message || "Done.");
      return true;
    } catch (actionError) {
      setError(actionError.message);
      return false;
    } finally {
      setBusy("");
    }
  };

  const submitInstall = (event) => {
    event.preventDefault();
    const value = source.trim();
    if (!value) return;
    run(`install:${value}`, async () => {
      const result = await installSkill(value, csrfToken);
      setSource("");
      return `Installed ${result.name} (${shortVersion(result.version)}).`;
    });
  };

  const submitUpload = (event) => {
    event.preventDefault();
    if (!file) return;
    run("upload", async () => {
      const result = await uploadSkill(file, csrfToken);
      setFile(null);
      event.target.reset();
      return `Installed ${result.name} (${shortVersion(result.version)}).`;
    });
  };

  const remove = (skill) => {
    const used = skill.usedBy || [];
    const warning = used.length
      ? `\n\nUsed by: ${used.join(", ")}. Uninstalling removes the skill from these references.`
      : "";
    if (!window.confirm(`Uninstall skill ${skill.name} and every installed version?${warning}`)) return;
    run(`uninstall:${skill.name}`, async () => {
      await uninstallSkill(skill.name, used.length > 0, csrfToken);
      return `Uninstalled ${skill.name}.`;
    });
  };

  return <div className="mx-auto max-w-[1500px] space-y-6 p-4 sm:p-6 lg:p-8">
    <PageHeading title="Skills" description="The versioned skills library runs load at execution time. A run composes repository, project and global lists into the installed versions here." />

    <Card className="space-y-4 p-4 sm:p-5">
      <form className="flex flex-col gap-3 sm:flex-row sm:items-end" onSubmit={submitInstall}>
        <label className="flex-1 space-y-1">
          <span className="field-label">Install from a URL or path</span>
          <input
            className="field-control"
            value={source}
            onChange={(event) => setSource(event.target.value)}
            placeholder="https://github.com/owner/repo/tree/main/skills/reviewer or ./path/to/skill"
            aria-label="Skill source"
          />
        </label>
        <Button type="submit" disabled={busy !== "" || !source.trim()}><PackagePlus className="size-4" />Install</Button>
      </form>
      <form className="flex flex-col gap-3 sm:flex-row sm:items-end" onSubmit={submitUpload}>
        <label className="flex-1 space-y-1">
          <span className="field-label">Or upload a .zip archive or SKILL.md</span>
          <input
            className="field-control"
            type="file"
            accept=".zip,SKILL.md,text/markdown"
            onChange={(event) => setFile(event.target.files?.[0] || null)}
            aria-label="Skill upload"
          />
        </label>
        <Button type="submit" variant="outline" disabled={busy !== "" || !file}><Upload className="size-4" />Upload</Button>
      </form>
      {error && <Alert>{error}</Alert>}
      {notice && <div role="status" className="rounded-md border border-success/35 bg-success/10 px-3 py-2 text-sm text-success">{notice}</div>}
    </Card>

    {!loaded ? <Card><QuietState title="Reading the skills library" description="Loading installed skills and their versions." role="status" /></Card>
      : skills.length === 0 ? <Card><QuietState title="No skills installed" description="Install a skill to make it available to runs and review profiles." /></Card>
        : <Card className="overflow-x-auto">
          <table className="w-full min-w-[52rem] border-collapse text-sm">
            <thead>
              <tr className="border-b border-border text-left text-xs uppercase tracking-wide text-muted-foreground">
                <th className="px-4 py-3 font-medium">Name</th>
                <th className="px-4 py-3 font-medium">Source</th>
                <th className="px-4 py-3 font-medium">Active</th>
                <th className="px-4 py-3 font-medium">Installed / updated</th>
                <th className="px-4 py-3 font-medium">Used by</th>
                <th className="px-4 py-3 font-medium">Actions</th>
              </tr>
            </thead>
            <tbody>
              {skills.map((skill) => <tr key={skill.name} className="border-b border-border align-top last:border-b-0">
                <td className="px-4 py-3 font-medium"><span className="break-all">{skill.name}</span></td>
                <td className="max-w-[18rem] px-4 py-3 text-xs text-muted-foreground"><span className="break-all">{skill.source || "—"}</span></td>
                <td className="px-4 py-3 font-mono text-xs" title={skill.active}>{shortVersion(skill.active) || "—"}</td>
                <td className="px-4 py-3 text-xs text-muted-foreground">
                  <span className="block">Installed {skillInstalledAt(skill) ? formatTimestamp(skillInstalledAt(skill)) : "—"}</span>
                  <span className="block">Updated {skillUpdatedAt(skill) ? formatTimestamp(skillUpdatedAt(skill)) : "—"}</span>
                </td>
                <td className="px-4 py-3">
                  {skill.usedBy?.length
                    ? <span className="flex flex-wrap gap-1">{skill.usedBy.map((usage) => <Badge key={usage} className="font-normal">{usage}</Badge>)}</span>
                    : <span className="text-xs text-muted-foreground">—</span>}
                </td>
                <td className="px-4 py-3">
                  <div className="flex flex-wrap items-center gap-2">
                    <Button
                      variant="outline"
                      size="sm"
                      disabled={busy !== ""}
                      onClick={() => run(`update:${skill.name}`, async () => {
                        const result = await updateSkill(skill.name, csrfToken);
                        return result.added ? `Updated ${skill.name} to ${shortVersion(result.version)}.` : `${skill.name} is already at the latest version.`;
                      })}
                    ><RefreshCw className="size-3.5" />Update</Button>
                    <select
                      className="field-control h-8 w-auto min-w-[7rem] py-0 text-xs"
                      value={versions[skill.name] ?? skill.active}
                      onChange={(event) => setVersions((current) => ({ ...current, [skill.name]: event.target.value }))}
                      aria-label={`Version for ${skill.name}`}
                    >
                      {skill.versions.map((version) => <option key={version.version} value={version.version}>{shortVersion(version.version)}{version.version === skill.active ? " (active)" : ""}</option>)}
                    </select>
                    <Button
                      variant="outline"
                      size="sm"
                      disabled={busy !== "" || (versions[skill.name] ?? skill.active) === skill.active}
                      onClick={() => run(`pin:${skill.name}`, async () => {
                        await pinSkill(skill.name, versions[skill.name] ?? skill.active, csrfToken);
                        return `Pinned ${skill.name} to ${shortVersion(versions[skill.name] ?? skill.active)}.`;
                      })}
                    ><Pin className="size-3.5" />Pin</Button>
                    <Button variant="ghost" size="sm" className="text-danger hover:bg-danger/10 hover:text-danger" disabled={busy !== ""} onClick={() => remove(skill)}><Trash2 className="size-3.5" />Uninstall</Button>
                  </div>
                </td>
              </tr>)}
            </tbody>
          </table>
        </Card>}
  </div>;
}