"""Shared skills-library loader.

Reviewer passes reference skills by name from the installed library; nothing
is fetched at review time. Lookup order:

1. the versioned library ``<data>/skills/<name>/<active>/SKILL.md`` written by
   ``machinist skills`` (active version from ``manifest.json``),
2. ``~/.agents/skills/<name>/SKILL.md`` (user installs, including
   install-by-URL below),
3. ``review/skills/<name>/SKILL.md`` shipped with the review step,
4. ``review/prompts/<name>.md`` legacy prompt files.

``install`` fetches a SKILL.md from GitHub and stores it under the user
skills directory — the install-by-URL half of the stub.
"""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import re
import urllib.request

from . import core

USER_SKILLS_DIR = Path.home() / ".agents" / "skills"
SKILLS_DIR = Path(__file__).resolve().parent / "skills"
PROMPTS_DIR = Path(__file__).resolve().parent / "prompts"

# --- MACH-50: ordered per-run/ per-review skills configuration --------------
#
# The layers compose like the Go resolver in internal/skills/resolve.go:
# .machinist/skills.yaml at the base ref, then the project setting
# skills.project.<project>, then the global setting skills.default. The first
# layer to name a skill decides it, enabled: false disables an inherited entry,
# and nothing is fetched at review time. One cache serves the whole review.

_CONTEXT: dict = {"repo_dir": None, "base_sha": None, "project": None, "kind": "review", "loaded": None}

# Control-plane settings fetch seam; tests replace it.
_control_plane_settings = None


class RequiredSkillError(RuntimeError):
    """A ``required: true`` skills entry is not installed; the run must fail."""


def configure(repo_dir: str | None = None, base_sha: str | None = None, project: str | None = None, kind: str = "review") -> None:
    """Set the review's repo/project context and drop the cached list."""
    _CONTEXT.update({"repo_dir": repo_dir, "base_sha": base_sha, "project": project, "kind": kind, "loaded": None})


def loaded_skills() -> dict:
    """Return the cached loaded-skills record for the configured review.

    Raises ``RequiredSkillError`` when a ``required: true`` entry is not
    installed; a malformed skills config only warns.
    """
    if _CONTEXT.get("loaded") is None:
        _CONTEXT["loaded"] = _load()
    return _CONTEXT["loaded"]


def _split_name(value: str) -> tuple[str, str]:
    value = (value or "").strip()
    index = value.rfind("@")
    if index > 0:
        return value[:index].strip(), value[index + 1 :].strip()
    return value, ""


def _repo_file(repo_dir: str, base_sha: str, path: str) -> str | None:
    """Read a repository file at base_sha with git show, or None."""
    import subprocess

    try:
        result = subprocess.run(
            ["git", "-C", repo_dir, "show", f"{base_sha}:{path}"],
            capture_output=True,
            text=True,
            timeout=30,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    if result.returncode != 0:
        return None
    return result.stdout


def _repo_skill(repo_dir: str, base_sha: str, name: str) -> str | None:
    for candidate in (name, _key(name)):
        text = _repo_file(repo_dir, base_sha, f".machinist/skills/{candidate}/SKILL.md")
        if text is not None:
            return text
    return None


def _parse_entries(text: str | None) -> list[dict]:
    import yaml

    if not text or not text.strip():
        return []
    document = yaml.safe_load(text)
    if document is None:
        return []
    if isinstance(document, dict):
        document = document.get("skills") or []
    if not isinstance(document, list):
        raise ValueError("skills config must be a list or {skills: [...]}")
    entries: list[dict] = []
    for raw in document:
        if not isinstance(raw, dict):
            continue
        name = str(raw.get("name") or "").strip()
        if not name:
            continue
        applies = raw.get("applies_to") or []
        if isinstance(applies, str):
            applies = [applies]
        entries.append(
            {
                "name": name,
                "applies_to": [str(value).strip().lower() for value in applies],
                "rule": str(raw.get("rule") or ""),
                "required": bool(raw.get("required")),
                "enabled": raw.get("enabled"),
            }
        )
    return entries


def _applies(entry: dict, kind: str) -> bool:
    applies = entry.get("applies_to") or []
    return not kind or not applies or kind.lower() in applies


def _compose(repo: list[dict], project: list[dict], global_: list[dict], kind: str) -> list[dict]:
    seen: set[str] = set()
    chosen: list[dict] = []
    for entries, scope in ((repo, "repo"), (project, "repo"), (global_, "global")):
        for entry in entries:
            if not _applies(entry, kind):
                continue
            full = str(entry["name"]).strip()
            base, _ = _split_name(full)
            if not base or base in seen:
                continue
            seen.add(base)
            if entry.get("enabled") is False:
                continue
            # Keep the full name (name@version) so _load resolves the pin.
            chosen.append({**entry, "name": full, "scope": scope})
    return chosen


def _settings(environ: dict | None = None, timeout: int = 30) -> dict:
    """Read the instance settings; an unreachable control plane yields {}."""
    if _control_plane_settings is not None:
        return _control_plane_settings(environ, timeout)
    import urllib.parse
    import urllib.request

    environ = environ if environ is not None else os.environ
    base_url = (environ.get("MACHINIST_CONTROL_PLANE_URL") or "").strip().rstrip("/")
    if not base_url:
        return {}
    request = urllib.request.Request(f"{base_url}/api/v1/settings")
    token = environ.get("MACHINIST_CONTROL_PLANE_TOKEN")
    if token:
        request.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            payload = json.loads(response.read().decode("utf-8"))
    except Exception:  # noqa: BLE001 - a missing settings endpoint must not fail the review
        return {}
    values = payload.get("settings") if isinstance(payload, dict) else None
    return values if isinstance(values, dict) else {}


def _load() -> dict:
    repo_dir = _CONTEXT.get("repo_dir")
    base_sha = _CONTEXT.get("base_sha")
    project = _CONTEXT.get("project")
    kind = _CONTEXT.get("kind") or "review"
    import yaml

    try:
        repo_entries = _parse_entries(_repo_file(repo_dir, base_sha, ".machinist/skills.yaml")) if repo_dir and base_sha else []
        settings = _settings()
        project_entries = _parse_entries(settings.get(f"skills.project.{project}")) if project else []
        global_entries = _parse_entries(settings.get("skills.default"))
    except (ValueError, yaml.YAMLError) as error:
        return {"skills": [], "warnings": [str(error)]}
    skills: list[dict] = []
    warnings: list[str] = []
    for entry in _compose(repo_entries, project_entries, global_entries, kind):
        name, _ = _split_name(entry["name"])
        resolved = resolve(entry["name"], repo_dir=repo_dir, base_sha=base_sha)
        if resolved is None:
            if entry.get("required"):
                raise RequiredSkillError(f'required skill "{entry["name"]}" is not installed')
            message = f'skill "{entry["name"]}" is not installed; skipping'
            warnings.append(message)
            continue
        text, version = resolved
        skills.append(
            {
                "name": name,
                "version": version,
                "scope": entry["scope"],
                "method": "prompt",
                "rule": entry.get("rule") or "",
                "required": bool(entry.get("required")),
            }
        )
    return {"skills": skills, "warnings": warnings}


def _library_root() -> Path:
    """The versioned skills library root: ``<data>/skills``."""
    data_dir = os.environ.get("MACHINIST_DATA_DIR", "").strip()
    base = Path(data_dir) if data_dir else Path.home() / ".machinist"
    return base / "skills"


def _library_lookup(key: str, version: str = "") -> tuple[str, str] | None:
    """Return (skill text, version) from the versioned library.

    ``version`` selects an exact installed version; empty reads the active one.
    """
    directory = _library_root() / key
    try:
        manifest = json.loads((directory / "manifest.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    if version:
        installed = {str(entry.get("version") or "") for entry in manifest.get("versions") or [] if isinstance(entry, dict)}
        if version not in installed or "/" in version or ".." in version:
            return None
        active = version
    else:
        active = str(manifest.get("active") or "").strip()
    if not active or "/" in active or ".." in active:
        return None
    try:
        text = (directory / active / "SKILL.md").read_text(encoding="utf-8")
    except OSError:
        return None
    return text, active


def _key(name: str) -> str:
    """Map a namespaced skill (feature-dev:code-reviewer) to a directory name."""
    return name.strip().lower().replace(":", "_")


def _frontmatter_name(text: str, fallback: str) -> str:
    match = re.match(r"^---\s*\n(.*?)\n---", text, re.DOTALL)
    if match:
        name_match = re.search(r"^name:\s*['\"]?([\w:.-]+)", match.group(1), re.MULTILINE)
        if name_match:
            return name_match.group(1)
    return fallback


def _version(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()[:12]


def resolve(name: str, *, repo_dir: str | None = None, base_sha: str | None = None) -> tuple[str, str] | None:
    """Return (skill text, version) for an installed skill, or None.

    A ``name@version`` pins an exact library version. A repo-bundled
    ``.machinist/skills/<name>/SKILL.md`` at ``base_sha`` wins over the library,
    and its version is the commit. Otherwise the versioned library
    (``<data>/skills``) wins, then the older lookup order below.
    """
    context = _CONTEXT
    repo_dir = repo_dir if repo_dir is not None else context.get("repo_dir")
    base_sha = base_sha if base_sha is not None else context.get("base_sha")
    base, version = _split_name(name)
    if repo_dir and base_sha:
        bundled = _repo_skill(repo_dir, base_sha, base)
        if bundled is not None:
            return bundled, base_sha
    key = _key(base)
    library = _library_lookup(key, version)
    if library is not None:
        return library
    if version:
        return None
    candidates = [
        USER_SKILLS_DIR / key / "SKILL.md",
        SKILLS_DIR / key / "SKILL.md",
        PROMPTS_DIR / f"{key}.md",
    ]
    for candidate in candidates:
        try:
            text = candidate.read_text(encoding="utf-8")
        except OSError:
            continue
        return text, _version(text)
    return None


def installed_names() -> list[str]:
    names: set[str] = set()
    for directory in (USER_SKILLS_DIR, SKILLS_DIR):
        try:
            for child in sorted(directory.iterdir()):
                if child.is_dir() and (child / "SKILL.md").is_file():
                    names.add(child.name)
        except OSError:
            continue
    try:
        for prompt in sorted(PROMPTS_DIR.glob("*.md")):
            names.add(prompt.stem)
    except OSError:
        return sorted(names)
    return sorted(names)


def install(url: str, timeout: int = 60) -> tuple[str, str]:
    """Install a SKILL.md from a GitHub URL into the user skills directory.

    Returns (name, version). Raises RuntimeError on any failure."""
    match = re.fullmatch(
        r"https?://(?:www\.)?github\.com/([\w.-]+)/([\w.-]+)(?:/(?:blob|raw)/([^/]+))?(?:/(.+))?/SKILL\.md/?",
        url.strip().rstrip("/"),
    )
    if not match:
        raise RuntimeError(f"unsupported skill URL: {url}")
    owner, repo, ref, directory = match.group(1), match.group(2), match.group(3) or "HEAD", match.group(4) or ""
    raw = f"https://raw.githubusercontent.com/{owner}/{repo}/{ref}/"
    if directory:
        raw += f"{directory.strip('/')}/"
    raw += "SKILL.md"
    request = urllib.request.Request(raw, headers={"User-Agent": "machinist-review"})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            text = response.read().decode("utf-8")
    except (urllib.error.URLError, OSError) as error:
        raise RuntimeError(f"could not fetch {raw}: {error}") from error
    name = _frontmatter_name(text, _key(directory.rsplit("/", 1)[-1] if directory else repo))
    target = USER_SKILLS_DIR / _key(name)
    target.mkdir(parents=True, exist_ok=True)
    (target / "SKILL.md").write_text(text, encoding="utf-8")
    return name, _version(text)
