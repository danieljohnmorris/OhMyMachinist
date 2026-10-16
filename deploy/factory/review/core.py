"""Pure helpers for the post-pull-request review tool."""

from __future__ import annotations

from dataclasses import dataclass
import json
import os
from pathlib import PurePosixPath
import re
from typing import Any, Iterable


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


# TruffleHog secret scan (MACH-41). The built-in review runs `trufflehog git`
# over the pull-request range (base..head). A verified live credential blocks
# the review; unverified/unknown detections are reported but never block, and
# secret values are only ever surfaced in TruffleHog's redacted form.
TRUFFLEHOG_BINARY = "trufflehog"


def trufflehog_scan_args(repo: str, base_sha: str, head_ref: str, results: str) -> list[str]:
    """The trufflehog command scanning commits in ``base_sha..head_ref``.

    ``results`` is a trufflehog ``--results`` value: ``verified`` for live
    credentials, or ``unverified,unknown`` for detections that do not block.
    """
    return [
        TRUFFLEHOG_BINARY,
        "git",
        f"file://{repo}",
        f"--since-commit={base_sha}",
        f"--branch={head_ref}",
        f"--results={results}",
        "--json",
        "--no-update",
        "--no-color",
    ]


class TruffleHogParseError(ValueError):
    """TruffleHog emitted a structured line that is neither a result nor one of
    its own log lines. Raised instead of dropping the line: an unparsed result
    can hide a live credential behind a clean-looking scan."""


_TRUFFLEHOG_LOG_KEYS = ("level", "logger", "msg")


def parse_trufflehog_output(output: str) -> list[dict[str, Any]]:
    """Parse TruffleHog's newline-delimited JSON results.

    The banner and TruffleHog's own JSON log lines (``level``/``logger``/
    ``msg``) are skipped, but any other structured line must be a result:
    a malformed or schema-drifted line raises ``TruffleHogParseError`` rather
    than being silently discarded."""
    findings: list[dict[str, Any]] = []
    for line in output.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            payload = json.loads(line)
        except json.JSONDecodeError as error:
            raise TruffleHogParseError(f"malformed result line: {redact(line)}") from error
        if not isinstance(payload, dict):
            raise TruffleHogParseError(f"unexpected result: {redact(line)}")
        # Results carry the detector/source metadata; trufflehog's own JSON
        # log lines carry "level"/"logger"/"msg" and nothing else.
        if "DetectorName" in payload or "SourceMetadata" in payload:
            findings.append(payload)
        elif any(key in payload for key in _TRUFFLEHOG_LOG_KEYS):
            continue
        else:
            raise TruffleHogParseError(f"result missing detector metadata: {redact(line)}")
    return findings


def trufflehog_location(payload: dict[str, Any]) -> str:
    """``path:line`` from a TruffleHog result, or ``(unknown)``."""
    metadata = payload.get("SourceMetadata")
    data = metadata.get("Data") if isinstance(metadata, dict) else None
    git = data.get("Git") if isinstance(data, dict) else None
    git = git if isinstance(git, dict) else {}
    path = str(git.get("file") or "").strip()
    line = git.get("line")
    if path:
        return f"{path}:{line}" if isinstance(line, int) and line > 0 else path
    return "(unknown)"


def trufflehog_finding(payload: dict[str, Any], verified: bool) -> dict[str, Any]:
    """A review finding for one TruffleHog result. The secret value is never
    included: only TruffleHog's redacted form (or a placeholder)."""
    detector = str(payload.get("DetectorName") or payload.get("DetectorType") or "secret").strip() or "secret"
    redacted = str(payload.get("Redacted") or "").strip() or "[REDACTED]"
    return {
        "severity": "critical" if verified else "warning",
        "location": trufflehog_location(payload),
        "message": f"{'verified' if verified else 'unverified'} {detector} credential ({redacted})",
        "detector": detector,
        "verified": verified,
        "reviewer": "trufflehog",
        "blocking": verified,
    }
