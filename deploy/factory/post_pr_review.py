#!/usr/bin/env python3
"""Machinist post-pull-request review, driven by a review profile (MACH-32).

Everything the review does — lanes and checks, reviewer passes, routing,
docs handling, blocking policy, amend rounds — comes from the resolved
profile (repo `.machinist/review.yaml` at the base ref > project > global
default > shipped default). No reviewer roles, checks or routing are
hard-coded here any more.
"""
from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
import argparse
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import sys
import tempfile
import time
from typing import Any

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

try:
    from deploy.factory.review import core, profile as profile_mod, skill_library
    from deploy.factory.review.runner import (
        CommandResult,
        executor_command,
        run_command,
        run_executor,
        timeout_from_env,
    )
except ModuleNotFoundError:
    # Flat install: deliver-pr.py, post_pr_review.py and review/ sit side by
    # side with no deploy package, so import review/ from the script directory.
    if __package__ in {None, ""}:
        sys.path.insert(0, str(Path(__file__).resolve().parent))
    from review import core, profile as profile_mod, skill_library
    from review.runner import (
        CommandResult,
        executor_command,
        run_command,
        run_executor,
        timeout_from_env,
    )


# MACH-31: INCOMPLETE (crashed review, no executor, unavailable skill) must
# not fail the delivery job, so it exits 0 like PASS; the machinist/review
# status and the JSON summary still record the INCOMPLETE verdict. Only
# BLOCKED returns 2.
EXIT_CODES = {"PASS": 0, "BLOCKED": 2, "INCOMPLETE": 0}
STATUS_STATES = {"PASS": "success", "BLOCKED": "failure", "INCOMPLETE": "error"}


class ReviewError(Exception):
    pass


@dataclass
class ReviewOptions:
    repo: Path
    pr: str
    base: str | None
    executor_cmd: str | None
    dry_run: bool
    json_out: str | None
    timeout: int
    amend_rounds: int | None = None
    profile_file: str | None = None
    authorized: set[str] = field(default_factory=set)


def parse_args(argv: list[str] | None = None) -> argparse.Namespace:
    class ReviewArgumentParser(argparse.ArgumentParser):
        def error(self, message: str) -> None:
            raise ReviewError(message)

    parser = ReviewArgumentParser(description="Run a Machinist post-pull-request review.")
    parser.add_argument("--repo", required=True, help="Checkout path")
    parser.add_argument("--pr", required=True, help="GitHub pull-request URL or owner/name#number")
    parser.add_argument("--base", help="Fallback base branch")
    parser.add_argument("--executor-cmd", help='Executor command that reads a prompt on stdin, e.g. "codex exec"')
    parser.add_argument("--timeout-seconds", type=int, help="Per-command timeout; default 900")
    parser.add_argument("--dry-run", action="store_true", help="Do not call GitHub write APIs, push, or commit")
    parser.add_argument("--json-out", help="Write a machine-readable summary to this path")
    parser.add_argument(
        "--amend-rounds",
        type=int,
        help="Retry budget override for amend rounds; default profile blocking.amend_rounds or MACHINIST_REVIEW_AMEND_ROUNDS",
    )
    parser.add_argument(
        "--profile-file",
        help="Path to a review profile YAML; overrides MACHINIST_REVIEW_PROFILE_FILE (rarely used; the profile is resolved from the repo base ref or the control plane)",
    )
    parser.add_argument(
        "--authorized",
        default="",
        help="Comma-separated authorizations granted to this run (e.g. deploy); also MACHINIST_REVIEW_AUTHORIZED",
    )
    return parser.parse_args(argv)


def run_git(repo: Path, args: list[str], timeout: int) -> CommandResult:
    return run_command(["git", *args], cwd=repo, timeout=timeout)


def run_gh(repo: Path, args: list[str], timeout: int, stdin: str | None = None) -> CommandResult:
    return run_command(["gh", *args], cwd=repo, timeout=timeout, stdin=stdin)


def normalize_repo_url(url: str) -> str:
    value = url.strip()
    value = value.rstrip("/")
    if value.endswith(".git"):
        value = value[:-4]
    if value.startswith("git@"):
        value = value.replace("git@", "", 1).replace(":", "/", 1)
    for prefix in ("https://", "http://", "ssh://"):
        if value.startswith(prefix):
            value = value[len(prefix):]
    parts = [part for part in value.split("/") if part]
    if len(parts) < 2:
        raise ReviewError(f"cannot determine repository from origin: {url}")
    return f"{parts[-2].lower()}/{parts[-1].lower()}"


def pull_request_metadata(repo: Path, owner: str, name: str, number: str, timeout: int) -> dict[str, Any]:
    result = run_gh(
        repo,
        [
            "pr",
            "view",
            number,
            "--repo",
            f"{owner}/{name}",
            "--json",
            "headRefName,headRefOid,baseRefName,baseRefOid,body,url,labels",
        ],
        timeout,
    )
    if not result.ok:
        raise ReviewError(f"cannot read pull-request metadata: {result.output}")
    try:
        metadata = json.loads(result.stdout)
    except json.JSONDecodeError as error:
        raise ReviewError(f"cannot parse pull-request metadata: {error}") from error
    metadata["labels"] = [
        label.get("name", "")
        for label in (metadata.get("labels") or [])
        if isinstance(label, dict)
    ]
    return metadata


def ensure_checkout(repo: Path, owner: str, name: str, metadata: dict[str, Any], timeout: int) -> str:
    origin_result = run_git(repo, ["remote", "get-url", "origin"], timeout)
    if not origin_result.ok:
        raise ReviewError(f"cannot read origin: {origin_result.output}")
    origin = normalize_repo_url(origin_result.stdout.strip())
    if origin != f"{owner}/{name}".lower():
        raise ReviewError(f"origin {origin} does not match {owner}/{name}")
    status = run_git(repo, ["status", "--porcelain"], timeout)
    if status.stdout.strip():
        raise ReviewError("working tree is not clean; refusing to review in place")
    branch_result = run_git(repo, ["branch", "--show-current"], timeout)
    starting_branch = branch_result.stdout.strip()
    switch = run_git(repo, ["switch", metadata["headRefName"]], timeout)
    if not switch.ok:
        raise ReviewError(f"cannot check out {metadata['headRefName']}: {switch.output}")
    return starting_branch


def restore_branch(repo: Path, starting_branch: str, timeout: int) -> None:
    if starting_branch:
        run_git(repo, ["switch", starting_branch], timeout)


def changed_file_list(
    repo: Path,
    owner: str,
    name: str,
    number: str,
    metadata: dict[str, Any],
    base: str | None,
    timeout: int,
) -> tuple[list[str], bool]:
    """Changed files plus whether the list could be read (routing fallbacks
    when it cannot)."""
    result = run_gh(repo, ["pr", "diff", number, "--repo", f"{owner}/{name}", "--name-only"], timeout)
    if result.ok and result.stdout.strip():
        return [line.strip() for line in result.stdout.splitlines() if line.strip()], True
    git_result = run_git(
        repo,
        ["diff", "--name-only", f"{base or metadata['baseRefOid']}...HEAD"],
        timeout,
    )
    if git_result.ok and git_result.stdout.strip():
        return [line.strip() for line in git_result.stdout.splitlines() if line.strip()], True
    return [], False


def pull_request_diff(repo: Path, owner: str, name: str, number: str, timeout: int) -> str:
    result = run_gh(repo, ["pr", "diff", number, "--repo", f"{owner}/{name}"], timeout)
    if not result.ok:
        raise ReviewError(f"cannot read the pull-request diff: {result.output}")
    return result.stdout


def repo_file_list(repo: Path, timeout: int) -> list[str]:
    result = run_git(repo, ["ls-files"], timeout)
    if not result.ok:
        return []
    return [line.strip() for line in result.stdout.splitlines() if line.strip()]


def classify_failure(
    repo: Path,
    check: profile_mod.ReviewCheck,
    base_sha: str,
    timeout: int,
) -> str:
    worktree = Path(tempfile.mkdtemp(prefix="machinist-review-"))
    add = run_git(repo, ["worktree", "add", "--detach", worktree.as_posix(), base_sha], timeout)
    if not add.ok:
        shutil.rmtree(worktree, ignore_errors=True)
        raise ReviewError(f"cannot create base worktree: {add.output}")
    try:
        base_result = run_command(check.args, cwd=worktree / check.cwd, timeout=timeout)
        return "pre-existing, not blocking" if not base_result.ok else "NEW"
    finally:
        run_git(repo, ["worktree", "remove", "--force", worktree.as_posix()], timeout)
        shutil.rmtree(worktree, ignore_errors=True)


def execute_checks(
    repo: Path,
    checks: list[profile_mod.ReviewCheck],
    base_sha: str,
    timeout: int,
    blocking_mode: str = "new_failures",
) -> list[profile_mod.ReviewCheck]:
    for check in checks:
        if check.classification != "not run" or not check.args:
            continue  # blocked / pending authorization / assumed pass
        check.result = run_command(check.args, cwd=repo / check.cwd, timeout=timeout)
        if check.ok:
            check.classification = "passed"
            continue
        if blocking_mode == "all":
            check.classification = "failing"
            continue
        check.classification = classify_failure(repo, check, base_sha, timeout)
    return checks


def redact_value(value: Any) -> Any:
    """Redact every string in a JSON-able structure. Reviewer findings, error
    strings and docs bodies are derived from the diff and may echo secrets."""
    if isinstance(value, str):
        return core.redact(value)
    if isinstance(value, dict):
        return {key: redact_value(item) for key, item in value.items()}
    if isinstance(value, list):
        return [redact_value(item) for item in value]
    return value


def run_secret_scan(repo: Path, base_sha: str, head_ref: str, timeout: int) -> dict[str, Any]:
    """Run TruffleHog over the PR range (base..head) for the built-in review.

    A verified live credential is a blocking finding; unverified/unknown
    detections are reported but never block. A missing binary or a failed scan
    reports an error so the review is INCOMPLETE instead of silently passing.
    """
    scan: dict[str, Any] = {"ran": False, "verified": [], "unverified": [], "error": None}
    if shutil.which(core.TRUFFLEHOG_BINARY) is None:
        scan["error"] = f"{core.TRUFFLEHOG_BINARY} not installed; secret scan skipped"
        return scan
    verified = run_command(
        core.trufflehog_scan_args(repo.as_posix(), base_sha, head_ref, "verified"),
        cwd=repo,
        timeout=timeout,
    )
    if verified.timed_out:
        scan["error"] = "trufflehog scan timed out"
        return scan
    if not verified.ok:
        scan["error"] = core.redact(verified.output) or "trufflehog scan failed"
        return scan
    try:
        verified_payloads = core.parse_trufflehog_output(verified.stdout)
    except core.TruffleHogParseError as error:
        scan["error"] = f"trufflehog verified scan: {error}"
        return scan
    scan["verified"] = [core.trufflehog_finding(payload, True) for payload in verified_payloads]
    unverified = run_command(
        core.trufflehog_scan_args(repo.as_posix(), base_sha, head_ref, "unverified,unknown"),
        cwd=repo,
        timeout=timeout,
    )
    if unverified.timed_out:
        scan["error"] = "trufflehog unverified scan timed out"
        return scan
    if not unverified.ok:
        scan["error"] = core.redact(unverified.output) or "trufflehog unverified scan failed"
        return scan
    try:
        unverified_payloads = core.parse_trufflehog_output(unverified.stdout)
    except core.TruffleHogParseError as error:
        scan["error"] = f"trufflehog unverified scan: {error}"
        return scan
    scan["unverified"] = [core.trufflehog_finding(payload, False) for payload in unverified_payloads]
    scan["ran"] = True
    return scan


def lane_changes(repo: Path, timeout: int) -> tuple[list[str], str | None]:
    """Dirty paths plus an error string when ``git status`` itself failed.

    ``changed_paths`` flattens a failed status to an empty list, which reads
    exactly like a clean tree. The lane-WIP path must tell the two apart: a
    status failure reported as "nothing to save" leaves the lane dirty and the
    branch switch then fails with an unrelated error.
    """
    result = run_git(repo, ["status", "--porcelain"], timeout)
    if not result.ok:
        return [], result.output or "git status failed"
    paths = []
    for line in result.stdout.splitlines():
        if len(line) > 3:
            paths.append(line[3:].strip().strip('"'))
    return paths, None


def changed_paths(repo: Path, timeout: int) -> list[str]:
    paths, _ = lane_changes(repo, timeout)
    return paths


def commit_allowed(repo: Path, paths: list[str], message: str, timeout: int) -> bool:
    allowed = [path for path in paths if not core.is_secret_path(path)]
    if not allowed or allowed != paths:
        return False
    for path in allowed:
        add_result = run_git(repo, ["add", "--", path], timeout)
        if not add_result.ok:
            return False
    commit = run_git(repo, ["commit", "-m", message], timeout)
    return commit.ok


def push_branch(repo: Path, branch: str, timeout: int) -> CommandResult:
    return run_git(repo, ["push", "origin", f"HEAD:refs/heads/{branch}"], timeout)


# MACH-59: the review runs in the lane checkout (docs pass, amend rounds). An
# edit left behind makes the final branch switch fail and then fails every
# queued job behind it, so before switching we commit the wanted edits or stash
# them with a labelled stash plus a patch outside the published run artifacts.
WIP_STASH_PREFIX = "machinist-review-wip"


def wip_patch_dir() -> Path:
    """Where a labelled WIP patch goes: never the published run artifacts.

    ``MACHINIST_OUTPUT_DIR`` is the run's published task folder (uploaded and
    shown to the user). The stash path deliberately covers files a commit
    refuses (an untracked ``.env``, say), so a patch there would publish the
    very secret the stash exists to keep out. Prefer the unpublishable scratch
    dir, then the Machinist data dir.
    """
    scratch = os.environ.get("MACHINIST_SCRATCH_DIR", "").strip()
    if scratch:
        return Path(scratch)
    data_dir = os.environ.get("MACHINIST_DATA_DIR", "").strip()
    base = Path(data_dir) if data_dir else Path.home() / ".machinist"
    return base / "review-wip"


def current_branch(repo: Path, timeout: int) -> str:
    result = run_git(repo, ["rev-parse", "--abbrev-ref", "HEAD"], timeout)
    return result.stdout.strip() if result.ok else ""


def save_lane_wip(
    repo: Path,
    timeout: int,
    *,
    message: str,
    label: str | None = None,
    artifact_dir: Path | None = None,
    prefer_stash: bool = False,
    paths: list[str] | None = None,
) -> dict[str, Any]:
    """Commit wanted edits, else stash them with a labelled stash and patch.

    Returns a report with the committed/stashed paths and, when the work was
    stashed, the patch written outside the published run artifacts. The lane is
    left clean either way so the branch switch cannot fail on a dirty checkout.
    ``paths`` limits the work to the wanted delta (the docs pass passes its
    before/after diff), so pre-existing lane dirt is never committed to the PR
    branch. ``prefer_stash`` skips the commit for leftovers that may include
    unrelated build output, so only the docs/fix passes ever push. A failed
    ``git status`` is reported as an error, never as a clean tree.
    """
    report: dict[str, Any] = {"committed": [], "stashed": [], "stash": None, "patch": None, "error": None}
    dirty, status_error = lane_changes(repo, timeout)
    if status_error:
        report["error"] = f"could not read lane status: {status_error}"
        return report
    if paths is None:
        wanted = dirty
    else:
        dirty_set = set(dirty)
        wanted = [path for path in paths if path in dirty_set]
    if not wanted:
        return report
    if not prefer_stash and not any(core.is_secret_path(path) for path in wanted):
        # Wanted edits go back to the PR branch; a commit is the cleanest exit.
        if commit_allowed(repo, wanted, message, timeout):
            report["committed"] = wanted
            branch = current_branch(repo, timeout)
            if not branch or not push_branch(repo, branch, timeout).ok:
                report["error"] = "committed review changes but could not push them"
            return report
    name = label or f"{WIP_STASH_PREFIX}-{time.strftime('%Y%m%d-%H%M%S')}"
    stash = run_git(repo, ["stash", "push", "--include-untracked", "-m", name, "--", *wanted], timeout)
    if not stash.ok:
        report["error"] = f"could not stash review changes: {stash.output}"
        return report
    report["stash"] = name
    report["stashed"] = wanted
    patch = run_git(repo, ["stash", "show", "--patch", "--include-untracked", "stash@{0}"], timeout)
    if patch.ok and patch.stdout:
        target = artifact_dir or wip_patch_dir()
        try:
            target.mkdir(parents=True, exist_ok=True)
            patch_path = target / f"{name}.patch"
            patch_path.write_text(patch.stdout, encoding="utf-8")
            report["patch"] = str(patch_path)
        except OSError as error:
            report["error"] = f"could not write review WIP patch: {error}"
    return report


def rerun_new_failures(
    repo: Path,
    checks: list[profile_mod.ReviewCheck],
    executor_args: list[str],
    timeout: int,
    branch: str,
    dry_run: bool,
    initial_changes: set[str] | None = None,
) -> bool:
    failing = [check for check in checks if check.classification == "NEW"]
    if not failing or executor_args is None:
        return False
    output = "\n\n".join(
        f"$ {shlex.join(check.args)}\n{check.result.output if check.result else ''}" for check in failing
    )
    prompt = f"{output}\n\nFix only these new errors, change nothing else."
    run_executor(executor_args, prompt=prompt, timeout=timeout)
    for check in failing:
        check.result = run_command(check.args, cwd=repo / check.cwd, timeout=timeout)
        check.classification = "passed" if check.ok else "NEW"
    if all(check.ok for check in failing):
        paths = [path for path in changed_paths(repo, timeout) if path not in (initial_changes or set())]
        if paths and not dry_run:
            if not commit_allowed(repo, paths, "Fix new lint/build errors from review", timeout):
                raise ReviewError("could not commit review fixes")
            if not push_branch(repo, branch, timeout).ok:
                raise ReviewError("could not push review fixes")
    return True


def amend_prompt(checks: list[profile_mod.ReviewCheck], findings: list[dict[str, Any]]) -> str:
    """Prompt sent back to the executor to amend the pull request on the
    current branch after blocking findings."""
    parts = [
        "A Machinist review found blocking problems in this pull request.",
        "Amend the pull request on the current branch to resolve every item below; change nothing unrelated.",
        "Do not force-push and do not rewrite published history.",
    ]
    failing = [check for check in checks if check.classification == "NEW"]
    if failing:
        parts.append("## Failing checks (new relative to the base branch)")
        for check in failing:
            output = check.result.output[-4000:] if check.result else ""
            parts.append(f"### {check.name}\n\n$ {shlex.join(check.args)}\n\n```\n{output}\n```")
    if findings:
        parts.append("## Blocking review findings")
        for finding in findings:
            location = finding.get("location") or "(unparsed)"
            parts.append(f"- {finding['severity']} | {location} | {finding['message']} (found by {finding.get('reviewer', finding.get('role', ''))})")
    return "\n\n".join(parts) + "\n"


def max_reviewer_workers(reviewers: list[dict[str, Any]], concurrency: int, environ: dict[str, str] | None = None) -> int:
    """Waves under the profile concurrency cap; MACHINIST_REVIEW_MAX_PARALLEL
    can lower it further, never drop a required pass."""
    env = os.environ if environ is None else environ
    cap = max(1, len(reviewers) or 1)
    if concurrency >= 1:
        cap = min(cap, concurrency)
    try:
        env_cap = int(env.get("MACHINIST_REVIEW_MAX_PARALLEL", ""))
        if env_cap >= 1:
            cap = min(cap, env_cap)
    except (TypeError, ValueError):
        pass
    return max(1, cap)


def run_reviewers(
    reviewers: list[dict[str, Any]],
    executor_args: list[str],
    context: dict[str, str],
    extra_instructions: str,
    timeout: int,
    concurrency: int,
) -> list[dict[str, Any]]:
    """Run the selected reviewer passes (waves under the concurrency cap)."""

    def run_one(reviewer: dict[str, Any]) -> dict[str, Any]:
        name = str(reviewer.get("name", "reviewer"))
        prompt, error = profile_mod.reviewer_prompt(reviewer, context, extra_instructions)
        if error:
            return {
                "name": name,
                "skills": reviewer.get("skills") or reviewer.get("skill") or [],
                "ran": False,
                "skipped": bool(reviewer.get("optional")),
                "findings": [],
                "error": error,
                "output": "",
            }
        try:
            result = run_executor(executor_args, prompt=prompt, timeout=timeout)
        except Exception as error:  # noqa: BLE001 - one bad pass must not kill the review
            return {"name": name, "skills": reviewer.get("skills") or [], "ran": False, "skipped": False, "findings": [], "error": f"reviewer failed: {error}", "output": ""}
        if not result.ok or not result.stdout.strip():
            reason = "timed out" if result.timed_out else result.output or "empty reviewer output"
            return {
                "name": name,
                "skills": reviewer.get("skills") or [],
                "ran": False,
                "skipped": False,
                "findings": [],
                "error": f"reviewer failed: {reason}",
                "output": "",
            }
        findings = [finding.__dict__ for finding in core.parse_reviewer_output(result.stdout)]
        return {
            "name": name,
            "skills": reviewer.get("skills") or [],
            "model": reviewer.get("model"),
            "effort": reviewer.get("effort"),
            "ran": True,
            "skipped": False,
            "findings": findings,
            "output": result.stdout,
        }

    if len(reviewers) <= 1:
        return [run_one(reviewer) for reviewer in reviewers]
    workers = max_reviewer_workers(reviewers, concurrency)
    with ThreadPoolExecutor(max_workers=workers) as pool:
        futures = [pool.submit(run_one, reviewer) for reviewer in reviewers]
        return [future.result() for future in as_completed(futures)]


def docs_prompt(diff: str, pr_body: str, repo: Path, files: list[str], instructions: str = "") -> str:
    resolved = skill_library.resolve("docs-reviewer")
    prompt = resolved[0] if resolved else ""
    if instructions.strip():
        prompt += "\n\nPROJECT REVIEW INSTRUCTIONS (from Machinist settings; follow these in addition to the skill above):\n\n" + instructions.strip() + "\n"
    prompt += "\n\nReturn a suggested pull-request description between <PR_BODY> and </PR_BODY> using the house format.\n"
    sections = [f"PR DIFF (data):\n{diff}", f"CURRENT PR BODY (data):\n{pr_body}"]
    for filename in files or ("README.md", "CLAUDE.md", "AGENTS.md"):
        path = repo / filename
        if path.is_file():
            sections.append(f"{filename} (data):\n{path.read_text(encoding='utf-8', errors='replace')}")
    return prompt + "\n\n" + "\n\n".join(sections) + "\n"


def parse_suggested_body(output: str) -> str | None:
    match = re.search(r"<PR_BODY>\s*(.*?)\s*</PR_BODY>", output, re.DOTALL)
    return match.group(1).strip() if match else None


def run_docs(
    repo: Path,
    owner: str,
    name: str,
    number: str,
    metadata: dict[str, Any],
    args: list[str],
    diff: str,
    timeout: int,
    dry_run: bool,
    docs_config: dict[str, Any],
    instructions: str = "",
) -> tuple[dict[str, Any], list[str]]:
    docs: dict[str, Any] = {"ran": False, "changed": [], "pr_description_updated": False, "error": None}
    files = [str(path) for path in docs_config.get("files") or []]
    before = set(changed_paths(repo, timeout))
    result = run_executor(args, prompt=docs_prompt(diff, metadata["body"], repo, files, instructions), timeout=timeout)
    docs["raw"] = result.stdout
    if not result.ok:
        docs["error"] = result.output or "docs reviewer failed"
        return docs, []
    docs["ran"] = True
    after = set(changed_paths(repo, timeout))
    # MACH-59: every file the docs pass touched is a wanted edit, not just the
    # configured files list (the incident edit was deploy/factory/README.md).
    docs_changed = sorted(after - before)
    docs["changed"] = docs_changed
    stale_line = next((line for line in result.stdout.splitlines() if line.startswith("PR_DESCRIPTION_STALE:")), "")
    docs["pr_description_stale"] = "yes" in stale_line.lower()
    if docs_changed and not dry_run:
        saved = save_lane_wip(
            repo,
            timeout,
            message="Update docs from PR review",
            label=f"{WIP_STASH_PREFIX}-docs-{time.strftime('%Y%m%d-%H%M%S')}",
            paths=docs_changed,
        )
        docs["saved"] = {key: saved[key] for key in ("committed", "stashed", "stash", "patch")}
        if saved["error"]:
            docs["error"] = saved["error"]
    suggested = parse_suggested_body(result.stdout)
    if docs.get("pr_description_stale") and suggested and docs_config.get("update_pr_description", True):
        docs["suggested_pr_body"] = suggested
        if not dry_run:
            edit = run_gh(
                repo,
                ["pr", "edit", number, "--repo", f"{owner}/{name}", "--body", suggested],
                timeout,
            )
            docs["pr_description_updated"] = edit.ok
            if not edit.ok:
                docs["error"] = "could not update pull-request description"
    return docs, docs_changed


def render_comment(
    status: str,
    checks: list[profile_mod.ReviewCheck],
    reviewers: list[dict[str, Any]],
    docs: dict[str, Any],
    resolved: profile_mod.ResolvedProfile,
    merged: list[dict[str, Any]],
    head_sha: str,
    notes: list[str],
    secret_scan: dict[str, Any] | None = None,
) -> str:
    lines = ["Machinist PR review", f"Status: {status}", ""]
    lines.append(f"Profile: {resolved.name or '(unnamed)'} (source: {resolved.source})")
    if head_sha:
        lines.append(f"Head SHA: {head_sha}")
    if notes:
        lines.append("")
        lines.append("## Notes")
        lines.extend(f"- {note}" for note in notes)
    if secret_scan is not None:
        lines.extend(["", "## Secret scan (TruffleHog)"])
        verified = secret_scan.get("verified") or []
        unverified = secret_scan.get("unverified") or []
        if verified:
            lines.append(f"- {len(verified)} verified credential(s) — blocking")
            for finding in verified[:20]:
                lines.append(f"- {finding['severity']} | {finding['location']} | {finding['message']}")
        elif secret_scan.get("ran"):
            lines.append("- No verified credentials.")
        if unverified:
            lines.append(f"- {len(unverified)} unverified finding(s) — not blocking")
            for finding in unverified[:20]:
                lines.append(f"- {finding['severity']} | {finding['location']} | {finding['message']}")
            if len(unverified) > 20:
                lines.append(f"- ... and {len(unverified) - 20} more")
        if not secret_scan.get("ran"):
            lines.append(f"- not run: {secret_scan.get('error', 'secret scan did not run')}")
    severities = resolved.severities()
    blocking = [finding for finding in merged if finding.get("blocking")]
    if blocking:
        lines.append("")
        lines.append("## CRITICAL")
        for finding in blocking:
            lines.append(f"- {finding['severity']} | {finding.get('location') or '(unparsed)'} | {finding['message']} — {finding.get('reviewer', '')}")
    sections = resolved.report_sections()
    include = set(sections) if sections else {"head_sha", "findings", "checks", "passes", "docs", "blockers"}
    if "checks" in include:
        lines.extend(["", "## Checks"])
        if checks:
            for check in checks:
                state = check.classification
                line_text = f"- {state}: {check.language} {check.name}"
                if check.note:
                    line_text += f" ({check.note})"
                lines.append(line_text)
                if check.result and check.result.output:
                    tail = "\n".join(core.redact(check.result.output).splitlines()[-60:])
                    lines.append(f"  ```\n  {tail.replace(chr(10), chr(10) + '  ')}\n  ```")
        else:
            lines.append("- No configured checks.")
    if "passes" in include:
        lines.extend(["", "## Reviewers"])
        if reviewers:
            for reviewer in reviewers:
                skills = ", ".join(str(skill) for skill in reviewer.get("skills", []))
                lines.append(f"### {reviewer['name']}" + (f" ({skills})" if skills else ""))
                if reviewer.get("skipped"):
                    lines.append(f"- skipped: {reviewer.get('error', 'optional pass skipped')}")
                    continue
                if not reviewer.get("ran"):
                    lines.append(f"- {reviewer.get('error', 'reviewer failed')}")
                    continue
                grouped: dict[str, list[dict[str, Any]]] = {}
                for finding in reviewer.get("findings", []):
                    grouped.setdefault(finding.get("severity", "unparsed"), []).append(finding)
                if not grouped:
                    lines.append("- NO FINDINGS")
                for severity in ("critical", "bug", "warning", "nit", "unparsed"):
                    for finding in grouped.get(severity, []):
                        lines.append(f"- {severity} | {finding.get('location') or '(unparsed)'} | {finding.get('message', '')}")
        else:
            lines.append("- Reviewers not run: no executor")
    if "findings" in include and "passes" not in include:
        lines.extend(["", "## Findings"])
        for finding in merged:
            lines.append(f"- {finding.get('severity')} | {finding.get('location') or '(unparsed)'} | {finding.get('message', '')}")
        if not merged:
            lines.append("- None.")
    if "docs" in include:
        lines.extend(["", "## Docs"])
        if docs.get("ran"):
            lines.append(f"- Files changed: {', '.join(docs.get('changed', [])) or 'none'}")
            lines.append(f"- PR description updated: {'yes' if docs.get('pr_description_updated') else 'no'}")
        else:
            lines.append(f"- {docs.get('error', 'Docs review not run')}")
    if "blockers" in include:
        lines.extend(["", "## Blockers"])
        if blocking:
            for finding in blocking:
                lines.append(f"- {finding.get('severity')} | {finding.get('location') or '(unparsed)'} | {finding.get('message', '')}")
        else:
            lines.append("- None.")
    return "\n".join(core.redact(line) for line in lines).rstrip() + "\n"


def final_status(blocking: bool, incomplete: bool) -> str:
    if blocking:
        return "BLOCKED"
    return "INCOMPLETE" if incomplete else "PASS"


def write_json(path: str, summary: dict[str, Any]) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")


def review(options: ReviewOptions) -> tuple[int, dict[str, Any]]:
    if not options.repo.is_dir():
        raise ReviewError(f"repository path does not exist: {options.repo}")
    owner, name, number = core.parse_pull_request(options.pr)
    number_text = str(number)
    metadata = pull_request_metadata(options.repo, owner, name, number_text, options.timeout)
    starting_branch = ensure_checkout(options.repo, owner, name, metadata, options.timeout)
    summary: dict[str, Any] = {
        "status": "INCOMPLETE",
        "blocking": False,
        "checks": [],
        "reviewers": [],
        "findings": [],
        "docs": {},
        "docs_changed": [],
        "comment_url": None,
        "profile": {},
        "head_sha": metadata.get("headRefOid", ""),
    }
    try:
        base_sha = metadata["baseRefOid"]
        resolved = None
        if options.profile_file:
            # Explicit operator override (e.g. testing a new profile).
            raw = Path(options.profile_file).read_text(encoding="utf-8")
            resolved = profile_mod.ResolvedProfile(body=profile_mod.parse_profile(raw, options.profile_file), raw=raw, source="file")
        else:
            # Repo profile at the base ref wins; then the control plane
            # (project profile or global default); then the bundled default.
            for path in profile_mod.REPO_PROFILE_PATHS:
                result = run_git(options.repo, ["show", f"{base_sha}:{path}"], options.timeout)
                if result.ok and result.stdout.strip():
                    resolved = profile_mod.ResolvedProfile(
                        body=profile_mod.parse_profile(result.stdout, f"{path}@{base_sha[:12]}"),
                        raw=result.stdout,
                        source="repo",
                    )
                    break
            if resolved is None:
                resolved = profile_mod.resolve_profile(options.repo, "", options.timeout, run_git)
        head_sha = metadata["headRefOid"]
        summary["profile"] = {
            "name": resolved.name,
            "source": resolved.source,
            "skills": profile_mod.skill_versions(resolved.body),
        }
        files, files_readable = changed_file_list(
            options.repo, owner, name, number_text, metadata, options.base, options.timeout
        )
        classified = core.classify_changed_files(files)
        repo_files = repo_file_list(options.repo, options.timeout)
        checks_cfg = resolved.body.get("checks") or {}
        blocking_mode = str(checks_cfg.get("blocking", "new_failures"))

        def outstanding_failures(current: list[profile_mod.ReviewCheck]) -> list[profile_mod.ReviewCheck]:
            # In "all" mode failing checks (new or pre-existing) also drive the
            # amend loop; in "new_failures" mode only NEW ones do.
            return [
                check
                for check in current
                if check.classification == "NEW" or (blocking_mode == "all" and check.classification == "failing")
            ]

        authorized = set(options.authorized)
        env_authorized = os.environ.get("MACHINIST_REVIEW_AUTHORIZED", "")
        authorized |= {item.strip() for item in env_authorized.split(",") if item.strip()}
        checks, notes = profile_mod.plan_checks(
            options.repo, resolved.body, files, classified, repo_files, authorized
        )
        lanes = profile_mod.lane_files(resolved.body, files, repo_files)
        lane_matches = {lane for lane, matched in lanes.items() if matched}
        labels = {str(label) for label in metadata.get("labels", [])}
        request_flag = "machinist:adversarial" in labels or "machinist:review-adversarial" in labels
        selected = profile_mod.select_reviewers(resolved.body, files, lane_matches, files_readable, request_flag)
        if not files_readable:
            notes.append("changed-file list unreadable; used the fallback reviewer set")
        diff = pull_request_diff(options.repo, owner, name, number_text, options.timeout)
        context = {
            "repository": f"{owner}/{name}",
            "pr": f"#{number}",
            "head_sha": head_sha,
            "base_sha": base_sha,
            "diff": diff,
            "profile_name": resolved.name,
        }
        planned = [shlex.join(check.args) for check in checks if check.args]
        initial_changes = set(changed_paths(options.repo, options.timeout))
        if options.dry_run and planned:
            print("Planned commands:")
            for command in planned:
                print(f"  {command}")
        secret_scan = run_secret_scan(options.repo, base_sha, metadata["headRefName"], options.timeout)
        if secret_scan.get("error"):
            notes.append(f"secret scan: {secret_scan['error']}")
        checks = execute_checks(options.repo, checks, base_sha, options.timeout, blocking_mode)
        remaining_new = outstanding_failures(checks)

        executor_args = executor_command(options.executor_cmd)
        budget = profile_mod.amend_rounds_budget(resolved, options.amend_rounds)
        budget_limit = budget
        amend_allowed = (resolved.body.get("scope") or {}).get("mode", "amend") == "amend"
        extra_instructions = str(resolved.body.get("extra_instructions", "") or resolved.extra_instructions)
        concurrency = int(resolved.body.get("concurrency", 3) or 3)
        severities = resolved.severities()

        reviewers: list[dict[str, Any]] = []
        docs: dict[str, Any] = {"ran": False, "changed": [], "pr_description_updated": False}

        def merged_findings() -> list[dict[str, Any]]:
            reconcile = resolved.body.get("reconcile") or {}
            return profile_mod.reconcile_findings(
                reviewers,
                severities,
                files,
                dedupe=bool(reconcile.get("dedupe", True)),
                verify_against_code=bool(reconcile.get("verify_against_code", True)),
            )

        if executor_args is None:
            reviewers = [
                {"name": str(reviewer.get("name", "reviewer")), "skills": reviewer.get("skills") or [], "ran": False, "skipped": False, "findings": [], "error": "reviewers not run: no executor"}
                for reviewer in selected
            ]
        elif remaining_new and amend_allowed and budget > 0:
            # First amend round: fix new check failures before reviewers run.
            budget -= 1
            rerun_new_failures(
                options.repo,
                checks,
                executor_args,
                options.timeout,
                metadata["headRefName"],
                options.dry_run,
                initial_changes,
            )
            remaining_new = outstanding_failures(checks)
        if executor_args is not None and remaining_new:
            reviewers = [
                {"name": str(reviewer.get("name", "reviewer")), "skills": reviewer.get("skills") or [], "ran": False, "skipped": False, "findings": [], "error": "reviewers skipped because new check failures remain"}
                for reviewer in selected
            ]
        elif executor_args is not None:
            reviewers = run_reviewers(selected, executor_args, context, extra_instructions, options.timeout, concurrency)
            # Blocking findings send the job back to amend on the same branch.
            # Each send-back consumes one retry-budget round; the checks and
            # reviewers rerun until a pass comes back clean or the budget is
            # spent, at which point the review is BLOCKED.
            findings = profile_mod.blocking_findings(merged_findings())
            while findings and amend_allowed and budget > 0:
                budget -= 1
                run_executor(
                    executor_args,
                    prompt=amend_prompt(remaining_new, findings),
                    timeout=options.timeout,
                )
                if not options.dry_run:
                    paths = [path for path in changed_paths(options.repo, options.timeout) if path not in initial_changes]
                    if paths:
                        if not commit_allowed(options.repo, paths, "Address blocking review findings", options.timeout):
                            raise ReviewError("could not commit review amendments")
                        if not push_branch(options.repo, metadata["headRefName"], options.timeout).ok:
                            raise ReviewError("could not push review amendments")
                checks = execute_checks(
                    options.repo,
                    profile_mod.plan_checks(options.repo, resolved.body, files, classified, repo_files, authorized)[0],
                    base_sha,
                    options.timeout,
                    blocking_mode,
                )
                remaining_new = outstanding_failures(checks)
                if remaining_new and amend_allowed and budget > 0:
                    budget -= 1
                    rerun_new_failures(
                        options.repo,
                        checks,
                        executor_args,
                        options.timeout,
                        metadata["headRefName"],
                        options.dry_run,
                        initial_changes,
                    )
                    remaining_new = outstanding_failures(checks)
                if remaining_new:
                    break
                diff = pull_request_diff(options.repo, owner, name, number_text, options.timeout)
                context["diff"] = diff
                # pin_head_sha (the default): the head may have moved while
                # amending; re-run the affected steps against the fresh diff.
                if not (resolved.body.get("scope") or {}).get("pin_head_sha", True):
                    break
                fresh = pull_request_metadata(options.repo, owner, name, number_text, options.timeout)
                if fresh.get("headRefOid") and fresh["headRefOid"] != head_sha:
                    head_sha = fresh["headRefOid"]
                    context["head_sha"] = head_sha
                    summary["head_sha"] = head_sha
                    notes.append("head moved during review; re-ran the reviewer passes against the new head")
                reviewers = run_reviewers(selected, executor_args, context, extra_instructions, options.timeout, concurrency)
                findings = profile_mod.blocking_findings(merged_findings())

        docs_config = resolved.body.get("docs") or {}
        if executor_args is not None and not remaining_new and docs_config.get("enabled", True):
            docs, docs_changed = run_docs(
                options.repo,
                owner,
                name,
                number_text,
                metadata,
                executor_args,
                diff,
                options.timeout,
                options.dry_run,
                docs_config,
                extra_instructions,
            )
            summary["docs_changed"] = docs_changed

        merged = merged_findings()
        blocking = any(check.classification == "NEW" for check in checks) if blocking_mode == "new_failures" else any(check.classification in {"NEW", "failing"} for check in checks)
        if profile_mod.blocking_findings(merged):
            blocking = True
        if secret_scan.get("verified"):
            blocking = True
        failed_reviewers = [reviewer for reviewer in reviewers if not reviewer.get("ran") and not reviewer.get("skipped") and "no executor" not in str(reviewer.get("error", ""))]
        incomplete = bool(failed_reviewers) or (executor_args is not None and docs_config.get("enabled", True) and not docs.get("ran"))
        if docs.get("error"):
            incomplete = True
        if any(check.classification == "blocked" for check in checks):
            incomplete = True
        if secret_scan.get("error"):
            incomplete = True
        status = final_status(blocking, incomplete)
        summary.update(
            {
                "status": status,
                "blocking": blocking,
                "checks": [
                    {
                        "language": check.language,
                        "name": check.name,
                        "command": list(check.args),
                        "classification": check.classification,
                        "note": check.note,
                        "output": core.redact(check.result.output[-4000:]) if check.result else "",
                    }
                    for check in checks
                ],
                "reviewers": [redact_value({key: value for key, value in reviewer.items() if key != "output"}) for reviewer in reviewers],
                "findings": redact_value(merged),
                "secret_scan": redact_value(secret_scan),
                "docs": redact_value({key: value for key, value in docs.items() if key != "raw"}),
                "amend_rounds": {"used": budget_limit - budget, "limit": budget_limit},
                "notes": notes,
            }
        )
        comment = render_comment(status, checks, reviewers, docs, resolved, merged, summary["head_sha"], notes, secret_scan)
        print(comment)
        state = STATUS_STATES[status]
        commit_status = str((resolved.body.get("output") or {}).get("commit_status", "machinist/review"))
        if options.dry_run:
            print("Planned command: gh pr comment ... --body-file -")
            print(f"Planned command: gh api repos/{owner}/{name}/statuses/{head_sha} ...")
        else:
            comment_result = CommandResult((), 0, "", "")
            if (resolved.body.get("output") or {}).get("pr_comment", True):
                comment_result = run_gh(
                    options.repo,
                    ["pr", "comment", number_text, "--repo", f"{owner}/{name}", "--body-file", "-"],
                    options.timeout,
                    stdin=comment,
                )
                if comment_result.ok:
                    summary["comment_url"] = comment_result.stdout.strip().splitlines()[-1]
            status_result = run_gh(
                options.repo,
                [
                    "api",
                    f"repos/{owner}/{name}/statuses/{head_sha}",
                    "-f",
                    f"state={state}",
                    "-f",
                    f"context={commit_status}",
                    "-f",
                    "description=Machinist review " + status,
                ],
                options.timeout,
            )
            if not status_result.ok or ((resolved.body.get("output") or {}).get("pr_comment", True) and not comment_result.ok):
                raise ReviewError(f"could not publish review: {comment_result.output} {status_result.output}")
        if status == "INCOMPLETE":
            print(
                "post-pr review warning: review incomplete; exiting 0 so the delivery job does not fail",
                file=sys.stderr,
            )
        return EXIT_CODES[status], summary
    finally:
        # MACH-59: never leave the lane dirty before switching back. The docs
        # pass and amend rounds run here; anything they left behind is
        # committed or stashed so the switch (and the next queued job) works.
        if not options.dry_run:
            wip = save_lane_wip(
                options.repo,
                options.timeout,
                message="Save review work in progress",
                prefer_stash=True,
            )
            if wip.get("error"):
                print(f"post-pr review warning: {wip['error']}", file=sys.stderr)
        restore_branch(options.repo, starting_branch, options.timeout)


def main(argv: list[str] | None = None) -> int:
    try:
        args = parse_args(argv)
        timeout = max(1, args.timeout_seconds or timeout_from_env())
        options = ReviewOptions(
            repo=Path(args.repo).resolve(),
            pr=args.pr,
            base=args.base,
            executor_cmd=args.executor_cmd,
            dry_run=args.dry_run,
            json_out=args.json_out,
            timeout=timeout,
            amend_rounds=args.amend_rounds,
            profile_file=args.profile_file,
            authorized={item.strip() for item in (args.authorized or "").split(",") if item.strip()},
        )
    except Exception as error:
        print(f"post-pr review error: {error}", file=sys.stderr)
        return EXIT_CODES["INCOMPLETE"]
    try:
        code, summary = review(options)
    except Exception as error:
        summary = {"status": "INCOMPLETE", "blocking": False, "checks": [], "reviewers": [], "findings": [], "docs": {}, "docs_changed": [], "comment_url": None, "error": str(error)}
        print(f"post-pr review error: {error}", file=sys.stderr)
        code = EXIT_CODES["INCOMPLETE"]
    if options.json_out:
        try:
            write_json(options.json_out, summary)
        except OSError as error:
            print(f"post-pr review error: {error}", file=sys.stderr)
            code = EXIT_CODES["INCOMPLETE"]
    return code


if __name__ == "__main__":
    raise SystemExit(main())
