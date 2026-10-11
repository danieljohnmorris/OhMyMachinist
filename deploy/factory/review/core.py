from __future__ import annotations

from dataclasses import dataclass
import os
from pathlib import PurePosixPath
import re
from typing import Iterable


CODE_REVIEW_ROLES = (
    "feature-dev:code-reviewer",
    "pr-review-toolkit:silent-failure-hunter",
    "pr-review-toolkit:pr-test-analyzer",
)
COMBINED_REVIEW_ROLE = "feature-dev:code-reviewer"
PROSE_REVIEW_ROLE = "write-as-dan"
LANGUAGE_REVIEW_ROLES = {
    "ts": ("typescript-code-review", "typescript-jsdoc"),
    "rust": ("rust-review",),
}
LANGUAGE_EXTENSIONS = {
    ".ts": "ts",
    ".tsx": "ts",
    ".js": "ts",
    ".jsx": "ts",
    ".mjs": "ts",
    ".cjs": "ts",
    ".py": "python",
    ".rs": "rust",
    ".rb": "ruby",
    ".erb": "ruby",
    ".sh": "shell",
    ".bash": "shell",
    ".go": "go",
}
SENSITIVE_PATH_MARKERS = (
    "auth",
    "secrets",
    "token",
    "crypto",
    "permission",
    "sandbox",
    "deploy",
)


@dataclass(frozen=True)
class Finding:
    severity: str
    location: str
    message: str
    unparsed: bool = False


def normalize_path(path: str) -> str:
    return PurePosixPath(path.replace("\\", "/")).as_posix()


def is_prose_file(path: str) -> bool:
    normalized = normalize_path(path)
    suffix = PurePosixPath(normalized).suffix.lower()
    return suffix in {".md", ".mdx"} and re.search(r"(?:^|/)src/content(?:/|$)", normalized) is not None


def classify_changed_files(files: Iterable[str]) -> dict[str, set[str]]:
    file_list = [normalize_path(path) for path in files if str(path).strip()]
    if not file_list:
        return {"prose": set(), "code": set()}
    prose = {path for path in file_list if is_prose_file(path)}
    return {"prose": prose, "code": set(file_list) - prose}


def detect_language(path: str, content: str | None = None) -> str:
    normalized = normalize_path(path)
    if os.path.basename(normalized) == "Gemfile":
        return "ruby"
    suffix = PurePosixPath(normalized).suffix.lower()
    if suffix:
        return LANGUAGE_EXTENSIONS.get(suffix, "other")
    first_line = (content or "").splitlines()[:1]
    shebang = first_line[0] if first_line else ""
    return "shell" if re.search(r"\b(sh|bash)\b", shebang) else "other"


def languages_for_files(files: Iterable[str], contents: dict[str, str] | None = None) -> set[str]:
    contents = contents or {}
    return {detect_language(path, contents.get(path)) for path in files}


def review_roles(
    classified: dict[str, set[str]],
    *,
    small_combined: bool,
) -> list[str]:
    code_files = classified.get("code", set())
    if small_combined and not classified.get("prose"):
        return [COMBINED_REVIEW_ROLE]
    languages = languages_for_files(code_files)
    roles = list(CODE_REVIEW_ROLES)
    for language in ("ts", "rust"):
        if language in languages:
            roles.extend(LANGUAGE_REVIEW_ROLES[language])
    if classified.get("prose"):
        roles.append(PROSE_REVIEW_ROLE)
    return roles


def changed_line_count(diff: str) -> int:
    return sum(
        1
        for line in diff.splitlines()
        if line.startswith(("+", "-"))
        and not line.startswith(("+++", "---"))
    )


def is_sensitive_path(path: str) -> bool:
    normalized = normalize_path(path).lower()
    return any(marker in normalized for marker in SENSITIVE_PATH_MARKERS)


def size_gate(
    diff: str,
    files: Iterable[str],
    *,
    max_changed_lines: int = 200,
    max_files: int = 5,
) -> bool:
    file_list = list(files)
    return (
        len(file_list) <= max_files
        and changed_line_count(diff) <= max_changed_lines
        and not any(is_sensitive_path(path) for path in file_list)
    )


def parse_reviewer_output(output: str) -> list[Finding]:
    if output.strip().upper() == "NO FINDINGS":
        return []
    findings: list[Finding] = []
    for raw_line in output.splitlines():
        line = raw_line.strip()
        if not line:
            continue
        parts = [part.strip() for part in line.split("|")]
        if len(parts) == 3 and parts[0].lower() in {"critical", "bug", "warning", "nit"} and parts[1]:
            findings.append(Finding(parts[0].lower(), parts[1], parts[2]))
        else:
            findings.append(Finding("unparsed", "", line, unparsed=True))
    return findings


def parse_pull_request(reference: str) -> tuple[str, str, int]:
    shorthand_match = re.fullmatch(r"([\w.-]+)/([\w.-]+)#(\d+)", reference.strip())
    if shorthand_match:
        return shorthand_match.group(1), shorthand_match.group(2), int(shorthand_match.group(3))
    url_match = re.search(r"github\.com/([\w.-]+)/([\w.-]+)/(?:pull/|pulls/|pip/)(\d+)", reference)
    if url_match:
        return url_match.group(1), url_match.group(2), int(url_match.group(3))
    raise ValueError(f"unsupported pull request reference: {reference}")


def is_secret_path(path: str) -> bool:
    normalized = normalize_path(path)
    basename = PurePosixPath(normalized).name
    return bool(
        re.match(r"^\.env($|\.)", basename)
        or basename.endswith((".pem", ".key", ".npmrc", ".netrc"))
        or re.match(r"^credentials.*\.json$", basename)
        or basename.startswith("id_")
        or "secret" in basename.lower()
    )
