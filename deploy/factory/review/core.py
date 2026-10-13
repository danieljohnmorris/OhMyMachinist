"""Pure helpers for the post-pull-request review tool."""

from __future__ import annotations

from dataclasses import dataclass
import os
from pathlib import PurePosixPath
import re
from typing import Iterable


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


# Token shapes redacted from anything published (PR comment, run summary).
# Profiles and reports must never echo credentials (MACH-32 hard requirement).
_REDACTION_PATTERNS = (
    (re.compile(r"gh[pousr]_[A-Za-z0-9]{16,}"), None),
    (re.compile(r"github_pat_[A-Za-z0-9_]{16,}"), None),
    (re.compile(r"AKIA[0-9A-Z]{16}"), None),
    (re.compile(r"\bsk-[A-Za-z0-9_-]{20,}\b"), None),
    (re.compile(r"(?i)(authorization\s*[:=]\s*(?:bearer|basic|token)\s+)\S+"), r"\1[REDACTED]"),
    (re.compile(r"(?i)\b((?:api[_-]?key|token|secret|password|passwd)\s*[=:]\s*)[^\s'\"&]{8,}"), r"\1[REDACTED]"),
)


def redact(text: str) -> str:
    """Mask credential-shaped strings in command output before publication."""
    if not text:
        return text
    result = text
    for pattern, replacement in _REDACTION_PATTERNS:
        result = pattern.sub(replacement or "[REDACTED]", result)
    return result
