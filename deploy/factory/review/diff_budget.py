"""Review-diff budgeting (MACH-58): keep built and generated files out of the
review prompt, and keep the prompt inside the model's context limit.

The factory sends the pull-request diff to the review model. Committed build
output (``web/dist/**``), minified JavaScript, source maps, lockfiles and other
generated files can add hundreds of thousands of tokens and push the prompt
over the model limit, so the review fails instead of reviewing (PR #44). This
module filters those paths out (configurable through the profile), lists what
it skipped, and — when the filtered diff is still over budget — chunks the diff
per file or falls back to a per-file summary, always reporting which mode it
used so an over-budget review is never silent.

Token estimates are cheap (``len(text) // 4``): the budget is a guard rail for
the model's context window, not an exact tokenizer count.
"""

from __future__ import annotations

from dataclasses import dataclass, field
import os
import re
from typing import Any, Iterable

# Paths the review never sends to the model by default (MACH-58): shipped build
# output, minified assets, source maps, lockfiles and generated code. The
# shipped default profile carries the same list; a profile that sets
# ``diff.exclude`` replaces these defaults.
DEFAULT_EXCLUDE_GLOBS = [
    # Committed build output.
    "web/dist/**",
    "**/node_modules/**",
    "**/vendor/**",
    "**/coverage/**",
    # Minified assets and source maps.
    "*.min.js",
    "*.min.css",
    "*.map",
    # Lockfiles.
    "package-lock.json",
    "npm-shrinkwrap.json",
    "yarn.lock",
    "pnpm-lock.yaml",
    "bun.lockb",
    "Gemfile.lock",
    "Cargo.lock",
    "composer.lock",
    "poetry.lock",
    "Pipfile.lock",
    "uv.lock",
    "go.sum",
    "*.lock",
    # Generated code.
    "*.generated.*",
    "**/generated/**",
    "*.pb.go",
    "*_pb2.py",
    "*.g.dart",
    "*.freezed.dart",
]

# Diff budget default: below the 262k-token model limit with headroom for the
# reviewer skills, project instructions and docs files that share the prompt.
DEFAULT_MAX_TOKENS = 200_000
MAX_TOKENS_ENV = "MACHINIST_REVIEW_DIFF_MAX_TOKENS"
DIFF_HEADER_RE = re.compile(
    r'^diff --git (?:"a/(?P<quoted_a>[^"]*)"|a/(?P<plain_a>\S+)) '
    r'(?:"b/(?P<quoted_b>[^"]*)"|b/(?P<plain_b>\S+))\s*$'
)
MODE_FULL = "full"
MODE_CHUNK = "chunk"
MODE_SUMMARY = "summary"


def default_max_tokens(environ: dict[str, str] | None = None) -> int:
    """Diff token budget: MACHINIST_REVIEW_DIFF_MAX_TOKENS, else 200k."""
    env = os.environ if environ is None else environ
    raw = env.get(MAX_TOKENS_ENV, "").strip()
    if raw:
        try:
            value = int(raw)
            if value > 0:
                return value
        except (TypeError, ValueError):
            pass
    return DEFAULT_MAX_TOKENS


def estimate_tokens(text: str) -> int:
    """Cheap token estimate (~4 characters per token) for budget decisions."""
    return (len(text) + 3) // 4


@dataclass
class DiffFile:
    """One ``diff --git`` section of a unified diff."""

    path: str
    text: str


@dataclass
class DiffChunk:
    """One prompt-sized slice of the review diff."""

    label: str
    text: str
    files: list[str] = field(default_factory=list)


@dataclass
class DiffPlan:
    """How the review diff is presented to the model."""

    mode: str
    chunks: list[DiffChunk]
    excluded: list[str] = field(default_factory=list)
    kept: list[str] = field(default_factory=list)
    summarized: list[str] = field(default_factory=list)
    max_tokens: int = DEFAULT_MAX_TOKENS
    unfiltered_tokens: int = 0
    filtered_tokens: int = 0

    def prompt_text(self) -> str:
        """The diff text for a single-prompt review."""
        return self.chunks[0].text if self.chunks else ""

    def summary_text(self) -> str:
        """A small, always-in-budget rendering of the changed files, used for
        the docs pass and when the diff is chunked."""
        lines = [f"changed file: {path}" for path in self.kept]
        lines += [f"changed file (summarised, over budget): {path}" for path in self.summarized]
        lines += [f"excluded file (not reviewed): {path}" for path in self.excluded]
        return "\n".join(lines) + ("\n" if lines else "")

    def files_in_prompt(self) -> list[str]:
        return [path for chunk in self.chunks for path in chunk.files]

    def notes(self) -> list[str]:
        """Review-note lines: what was excluded and which mode was used."""
        notes: list[str] = []
        if self.excluded:
            notes.append(
                f"review diff excludes {len(self.excluded)} file(s) not sent to the model: "
                + _render_paths(self.excluded)
            )
        if self.mode == MODE_FULL:
            notes.append(
                f"review diff mode: full ({self.filtered_tokens} of {self.max_tokens} token budget)"
            )
        elif self.mode == MODE_CHUNK:
            notes.append(
                f"review diff mode: chunked into {len(self.chunks)} chunk(s) "
                f"({self.filtered_tokens} tokens over the {self.max_tokens}-token budget); "
                "reviewers ran once per chunk"
            )
        else:
            notes.append(
                f"review diff mode: summary ({self.filtered_tokens} tokens over the "
                f"{self.max_tokens}-token budget); the largest files are represented by their "
                "diff header and line counts"
            )
        if self.summarized:
            notes.append("review diff summarised file(s): " + _render_paths(self.summarized))
        if self.excluded and not self.kept:
            notes.append("review diff: every changed file was excluded; reviewers saw an empty diff")
        return notes

    def summary(self) -> dict[str, Any]:
        return {
            "mode": self.mode,
            "max_tokens": self.max_tokens,
            "tokens": {"unfiltered": self.unfiltered_tokens, "filtered": self.filtered_tokens},
            "chunks": len(self.chunks),
            "excluded": list(self.excluded),
            "summarized": list(self.summarized),
        }


def _render_paths(paths: Iterable[str], limit: int = 50) -> str:
    items = list(paths)
    rendered = ", ".join(f"`{path}`" for path in items[:limit])
    if len(items) > limit:
        rendered += f" and {len(items) - limit} more"
    return rendered


def parse_diff(diff_text: str) -> list[DiffFile]:
    """Split a unified diff into one ``DiffFile`` per ``diff --git`` section."""
    files: list[DiffFile] = []
    path: str | None = None
    buffer: list[str] = []
    for line in diff_text.splitlines(keepends=True):
        if line.startswith("diff --git "):
            if path is not None and buffer:
                files.append(DiffFile(path, "".join(buffer)))
            path = diff_path(line)
            buffer = [line]
        elif path is not None:
            buffer.append(line)
    if path is not None and buffer:
        files.append(DiffFile(path, "".join(buffer)))
    return files


def diff_path(header_line: str) -> str:
    """The ``b/`` path from a ``diff --git a/... b/...`` header line."""
    match = DIFF_HEADER_RE.match(header_line.rstrip("\n"))
    if not match:
        return ""
    return match.group("quoted_b") or match.group("plain_b") or ""


def path_excluded(path: str, patterns: Iterable[str]) -> bool:
    """Whether ``path`` matches any exclude glob (profile glob semantics)."""
    from . import profile as profile_mod

    return any(profile_mod.glob_match(str(pattern), path) for pattern in patterns)


def _line_stats(text: str) -> tuple[int, int]:
    added = deleted = 0
    for line in text.splitlines():
        if line.startswith("+++") or line.startswith("---"):
            continue
        if line.startswith("+"):
            added += 1
        elif line.startswith("-"):
            deleted += 1
    return added, deleted


def summarize_file(file: DiffFile, max_tokens: int) -> str:
    """A bounded stand-in for a file whose diff alone exceeds the budget: the
    diff header, line counts and as much of the start as fits."""
    added, deleted = _line_stats(file.text)
    budget = max(64, max_tokens // 2)
    body: list[str] = []
    used = 0
    for line in file.text.splitlines():
        cost = estimate_tokens(line + "\n")
        if body and used + cost > budget:
            break
        body.append(line)
        used += cost
    tail = f"… ({added} added, {deleted} deleted lines; remainder omitted to fit the review budget)"
    return "\n".join([*body, tail]) + "\n"


def _pack_chunks(files: list[DiffFile], max_tokens: int) -> tuple[list[DiffChunk], list[str]]:
    """Greedily pack files into chunks of at most ``max_tokens``; a file too
    large to fit alone becomes its own summary chunk."""
    chunks: list[DiffChunk] = []
    summarized: list[str] = []
    pending: list[DiffFile] = []
    pending_tokens = 0

    def flush() -> None:
        nonlocal pending, pending_tokens
        if pending:
            chunks.append(
                DiffChunk(
                    label=f"{len(chunks) + 1}",
                    text="".join(file.text for file in pending),
                    files=[file.path for file in pending],
                )
            )
        pending = []
        pending_tokens = 0

    for file in files:
        cost = estimate_tokens(file.text)
        if cost > max_tokens:
            flush()
            chunks.append(DiffChunk(label=f"summary:{file.path}", text=summarize_file(file, max_tokens), files=[file.path]))
            summarized.append(file.path)
            continue
        if pending and pending_tokens + cost > max_tokens:
            flush()
        pending.append(file)
        pending_tokens += cost
    flush()
    return chunks, summarized


def plan_diff(
    diff_text: str,
    exclude_globs: Iterable[str],
    max_tokens: int | None = None,
) -> DiffPlan:
    """Filter the review diff and decide the prompt mode.

    ``full`` keeps the whole filtered diff; ``chunk`` splits it so each chunk
    fits the budget (reviewers run once per chunk); ``summary`` replaces files
    that are too large on their own with a bounded summary.
    """
    budget = default_max_tokens() if max_tokens is None else max(1, int(max_tokens))
    files = parse_diff(diff_text)
    kept: list[DiffFile] = []
    excluded_paths: list[str] = []
    for file in files:
        if not file.path or path_excluded(file.path, exclude_globs):
            # An unparseable ``diff --git`` header leaves ``path`` empty; fail
            # closed rather than silently bypassing the exclude globs.
            excluded_paths.append(file.path or "<unparsed diff header>")
        else:
            kept.append(file)
    excluded = sorted(dict.fromkeys(excluded_paths))
    filtered_text = "".join(file.text for file in kept)
    unfiltered_tokens = estimate_tokens(diff_text)
    filtered_tokens = estimate_tokens(filtered_text)
    if filtered_tokens <= budget:
        chunks = [DiffChunk(label="all", text=filtered_text, files=[file.path for file in kept])]
        summarized: list[str] = []
        mode = MODE_FULL
    else:
        chunks, summarized = _pack_chunks(kept, budget)
        # Each summarized file becomes its own summary chunk, so an
        # all-summarized plan can have multiple chunks; it is still ``summary``.
        mode = MODE_SUMMARY if len(chunks) == len(summarized) else MODE_CHUNK
    return DiffPlan(
        mode=mode,
        chunks=chunks,
        excluded=excluded,
        kept=[file.path for file in kept],
        summarized=summarized,
        max_tokens=budget,
        unfiltered_tokens=unfiltered_tokens,
        filtered_tokens=filtered_tokens,
    )