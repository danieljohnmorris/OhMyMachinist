"""Minimal skills-library stub (MACH-32) ahead of the shared loader (MACH-33).

Reviewer passes reference skills by name from the installed library; nothing
is fetched at review time. Lookup order:

1. ``~/.agents/skills/<name>/SKILL.md`` (user installs, including
   install-by-URL below),
2. ``review/skills/<name>/SKILL.md`` shipped with the review step,
3. ``review/prompts/<name>.md`` legacy prompt files.

``install`` fetches a SKILL.md from GitHub and stores it under the user
skills directory — the install-by-URL half of the stub.
"""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
import re
import urllib.request

from . import core

USER_SKILLS_DIR = Path.home() / ".agents" / "skills"
SKILLS_DIR = Path(__file__).resolve().parent / "skills"
PROMPTS_DIR = Path(__file__).resolve().parent / "prompts"


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


def resolve(name: str) -> tuple[str, str] | None:
    """Return (skill text, version) for an installed skill, or None."""
    key = _key(name)
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
