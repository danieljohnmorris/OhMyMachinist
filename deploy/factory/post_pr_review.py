#!/usr/bin/env python3
from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
import json
import os
from pathlib import Path
import shlex
import shutil
import sys
import tempfile
import re
import urllib.parse
import urllib.request
from typing import Any

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

try:
    from deploy.factory.review import core
    from deploy.factory.review.runner import (
        CommandResult,
        available,
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
    from review import core
    from review.runner import (
        CommandResult,
        available,
        executor_command,
        run_command,
        run_executor,
        timeout_from_env,
    )


STATUS_CONTEXT = "machinist/review"
# MACH-31: INCOMPLETE (crashed review, no executor, timeout) must not fail the
# delivery job, so it exits 0 like PASS; the machinist/review status and the
# JSON summary still record the INCOMPLETE verdict. Only BLOCKED returns 2.
EXIT_CODES = {"PASS": 0, "BLOCKED": 2, "INCOMPLETE": 0}
STATUS_STATES = {"PASS": "success", "BLOCKED": "failure", "INCOMPLETE": "error"}
PROMPTS_DIR = Path(__file__).resolve().parent / "review" / "prompts"


class ReviewError(Exception):
    pass


@dataclass
class ReviewCheck:
    language: str
    name: str
    args: list[str]
    cwd: str = "."
    result: CommandResult | None = None
    classification: str = "not run"

    @property
    def ok(self) -> bool:
        return bool(self.result and self.result.ok)


@dataclass
class ReviewOptions:
    repo: Path
    pr: str
    base: str | None
    executor_cmd: str | None
    dry_run: bool
    json_out: str | None
    timeout: int
    amend_rounds: int = 3
    instructions_file: str | None = None


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
        help="Retry budget for amend rounds after blocking findings; default 3 or MACHINIST_REVIEW_AMEND_ROUNDS",
    )
    parser.add_argument(
        "--instructions-file",
        help="Path to per-project review instructions; default reads MACHINIST_REVIEW_INSTRUCTIONS_FILE",
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
            "headRefName,headRefOid,baseRefName,baseRefOid,body,url",
        ],
        timeout,
    )
    if not result.ok:
        raise ReviewError(f"failed to read pull request: {result.output}")
    try:
        metadata = json.loads(result.stdout)
    except json.JSONDecodeError as error:
        raise ReviewError(f"invalid pull-request JSON: {error}") from error
    required = {"headRefName", "headRefOid", "baseRefName", "baseRefOid", "body", "url"}
    if not required.issubset(metadata):
        raise ReviewError("pull-request JSON is missing required fields")
    return metadata


def ensure_checkout(repo: Path, owner: str, name: str, metadata: dict[str, Any], timeout: int) -> str:
    origin_result = run_git(repo, ["remote", "get-url", "origin"], timeout)
    if not origin_result.ok:
        raise ReviewError(f"cannot read checkout origin: {origin_result.output}")
    try:
        origin = normalize_repo_url(origin_result.stdout)
    except ReviewError:
        raise
    if origin != f"{owner}/{name}".lower():
        raise ReviewError(f"checkout origin {origin} does not match pull request {owner}/{name}")

    branch_result = run_git(repo, ["rev-parse", "--abbrev-ref", "HEAD"], timeout)
    if not branch_result.ok:
        raise ReviewError(f"cannot read checkout branch: {branch_result.output}")
    starting_branch = branch_result.stdout.strip()
    pr_branch = metadata["headRefName"]
    if starting_branch != pr_branch:
        local_branch = run_git(repo, ["show-ref", "--verify", "--quiet", f"refs/heads/{pr_branch}"], timeout)
        if not local_branch.ok:
            raise ReviewError(f"checkout is not on pull-request head branch {pr_branch}")
        switch_result = run_git(repo, ["switch", pr_branch], timeout)
        if not switch_result.ok:
            raise ReviewError(f"cannot switch to pull-request branch: {switch_result.output}")
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
) -> list[str]:
    result = run_gh(repo, ["pr", "diff", number, "--repo", f"{owner}/{name}", "--name-only"], timeout)
    if result.ok:
        files = [line.strip() for line in result.stdout.splitlines() if line.strip()]
        if files:
            return files
    if not base:
        base = metadata.get("baseRefName")
    if not base:
        raise ReviewError("no changed-file list and no base branch for fallback")
    git_result = run_git(repo, ["diff", "--name-only", f"{base}...HEAD"], timeout)
    if not git_result.ok:
        raise ReviewError(f"cannot determine changed files: {git_result.output}")
    return [line.strip() for line in git_result.stdout.splitlines() if line.strip()]


def pull_request_diff(repo: Path, owner: str, name: str, number: str, timeout: int) -> str:
    result = run_gh(repo, ["pr", "diff", number, "--repo", f"{owner}/{name}"], timeout)
    if not result.ok:
        raise ReviewError(f"cannot read pull-request diff: {result.output}")
    return result.stdout


def nearest_package_roots(repo: Path, files: list[str]) -> list[Path]:
    roots: dict[Path, list[str]] = {}
    for changed in files:
        relative = Path(changed)
        current = relative.parent
        selected = None
        while True:
            if (repo / current / "package.json").is_file():
                selected = current
                break
            if current == Path("."):
                break
            current = current.parent
        if selected is not None:
            roots.setdefault(selected, []).append(changed)
    return sorted(roots, key=lambda path: (len(path.parts), str(path)), reverse=True)


def npm_scripts(package_root: Path) -> set[str]:
    try:
        package = json.loads((package_root / "package.json").read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return set()
    return set(package.get("scripts", {}))


def build_checks(repo: Path, classified: dict[str, set[str]], languages: set[str]) -> list[ReviewCheck]:
    checks: list[ReviewCheck] = []
    if "ts" in languages:
        ts_files = [path for path in sorted(classified["code"]) if core.detect_language(path) == "ts"]
        for root in nearest_package_roots(repo, ts_files):
            scripts = npm_scripts(repo / root)
            if (repo / root / "tsconfig.json").is_file():
                checks.append(ReviewCheck("ts", "tsc", ["npx", "tsc", "--noEmit"], root.as_posix()))
            for script in ("lint", "build", "test"):
                if script in scripts:
                    command = "test" if script == "test" else script
                    checks.append(ReviewCheck("ts", f"npm {command}", ["npm", "run", script], root.as_posix()))
    if "python" in languages:
        py_files = [path for path in sorted(classified["code"]) if core.detect_language(path) == "py"]
        if available("ruff"):
            checks.append(ReviewCheck("python", "ruff", ["ruff", "check", *py_files]))
        config = any(
            (repo / name).is_file()
            for name in ("pyproject.toml", "setup.cfg", "mypy.ini")
        )
        if config:
            checks.append(ReviewCheck("python", "mypy", ["mypy"]))
        if any(repo.rglob("test_*.py")) or any(repo.rglob("*_test.py")):
            if available("pytest"):
                checks.append(ReviewCheck("python", "pytest", ["pytest"]))
    if "rust" in languages:
        checks.extend(
            [
                ReviewCheck("rust", "cargo build", ["cargo", "build"]),
                ReviewCheck("rust", "cargo clippy", ["cargo", "clippy", "--", "-D", "warnings"]),
                ReviewCheck("rust", "cargo fmt --check", ["cargo", "fmt", "--check"]),
            ]
        )
    if "go" in languages:
        checks.extend(
            [
                ReviewCheck("go", "go build", ["go", "build", "./..."]),
                ReviewCheck("go", "go vet", ["go", "vet", "./..."]),
                ReviewCheck("go", "go test", ["go", "test", "./..."]),
            ]
        )
    if "ruby" in languages:
        if (repo / "config/ci.rb").is_file():
            checks.append(ruby_check(repo, "bin/ci", ["ci"]))
        else:
            checks.append(ruby_check(repo, "bin/rubocop", ["rubocop"]))
            brakeman_args = ["brakeman", "--no-pager"]
            if (repo / "bin/brakeman").is_file():
                checks.append(ReviewCheck("ruby", "brakeman", ["bin/brakeman", *brakeman_args[1:]]))
            else:
                checks.append(ReviewCheck("ruby", "brakeman", ["bundle", "exec", *brakeman_args]))
            if (repo / "spec").is_dir():
                checks.append(ruby_check(repo, "bin/rspec", ["rspec"]))
            else:
                checks.append(ruby_check(repo, "bin/rails", ["rails", "test"]))
    if "shell" in languages:
        for path in sorted(classified["code"]):
            if core.detect_language(path) == "shell":
                checks.append(ReviewCheck("shell", f"shellcheck {path}", ["shellcheck", path]))
    return checks


def classify_failure(
    repo: Path,
    check: ReviewCheck,
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


def ruby_check(repo: Path, binstub: str, tool_args: list[str]) -> ReviewCheck:
    if (repo / binstub).is_file():
        return ReviewCheck("ruby", binstub, [binstub, *tool_args[1:]])
    return ReviewCheck("ruby", f"bundle exec {tool_args[0]}", ["bundle", "exec", *tool_args])


def execute_checks(
    repo: Path,
    checks: list[ReviewCheck],
    base_sha: str,
    timeout: int,
) -> list[ReviewCheck]:
    for check in checks:
        check.result = run_command(check.args, cwd=repo / check.cwd, timeout=timeout)
        if check.ok:
            check.classification = "passed"
        else:
            check.classification = classify_failure(repo, check, base_sha, timeout)
    return checks


def changed_paths(repo: Path, timeout: int) -> list[str]:
    result = run_git(repo, ["status", "--porcelain"], timeout)
    if not result.ok:
        return []
    paths = []
    for line in result.stdout.splitlines():
        if len(line) > 3:
            paths.append(line[3:].strip().strip('"'))
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


def rerun_new_failures(
    repo: Path,
    checks: list[ReviewCheck],
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


def push_branch(repo: Path, branch: str, timeout: int) -> CommandResult:
    return run_git(repo, ["push", "origin", f"HEAD:refs/heads/{branch}"], timeout)


def amend_prompt(checks: list[ReviewCheck], findings: list[dict[str, Any]]) -> str:
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
            parts.append(f"- {finding['severity']} | {location} | {finding['message']} (found by {finding['role']})")
    return "\n\n".join(parts) + "\n"


INSTRUCTIONS_HEADER = (
    "\n\nPROJECT REVIEW INSTRUCTIONS (from Machinist settings; follow these in "
    "addition to the skill above):\n\n{instructions}\n"
)


def project_instructions(
    owner: str,
    name: str,
    timeout: int,
    instructions_file: str | None = None,
    environ: dict[str, str] | None = None,
) -> str:
    """Load per-project review instructions.

    Order: explicit --instructions-file, then MACHINIST_REVIEW_INSTRUCTIONS_FILE,
    then the Machinist control plane (MACHINIST_CONTROL_PLANE_URL with the
    worker token), which stores the markdown edited per project in Settings.
    Missing sources return "" so the base skill prompts still run.
    """
    env = os.environ if environ is None else environ
    path = instructions_file or env.get("MACHINIST_REVIEW_INSTRUCTIONS_FILE")
    if path:
        try:
            return Path(path).read_text(encoding="utf-8")
        except OSError:
            return ""
    base_url = (env.get("MACHINIST_CONTROL_PLANE_URL") or "").strip().rstrip("/")
    if not base_url:
        return ""
    project = env.get("MACHINIST_PROJECT") or f"{owner}/{name}".lower()
    request = urllib.request.Request(
        f"{base_url}/api/v1/review-instructions?project={urllib.parse.quote(project, safe='')}"
    )
    token = env.get("MACHINIST_CONTROL_PLANE_TOKEN")
    if token:
        request.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(request, timeout=min(timeout, 30)) as response:
            payload = json.loads(response.read().decode("utf-8"))
    except (urllib.error.URLError, OSError, json.JSONDecodeError, ValueError):
        return ""
    instructions = payload.get("instructions") if isinstance(payload, dict) else None
    return instructions if isinstance(instructions, str) else ""


def amend_budget(amend_rounds: int | None, environ: dict[str, str] | None = None) -> int:
    """Retry budget for amend rounds: each send-back to fix blocking findings
    consumes one round; when exhausted the review reports BLOCKED."""
    env = os.environ if environ is None else environ
    raw = amend_rounds
    if raw is None:
        try:
            raw = int(env.get("MACHINIST_REVIEW_AMEND_ROUNDS", ""))
        except (TypeError, ValueError):
            raw = 3
    return max(0, int(raw))


def prompt_text(role: str) -> str:
    filename = f"{role.replace(':', '_')}.md"
    path = PROMPTS_DIR / filename
    if not path.is_file():
        raise ReviewError(f"missing reviewer prompt for {role}")
    return path.read_text(encoding="utf-8")


def run_reviewer(
    role: str,
    args: list[str],
    diff: str,
    timeout: int,
    instructions: str = "",
) -> dict[str, Any]:
    try:
        prompt = f"{prompt_text(role)}"
        if instructions.strip():
            prompt += INSTRUCTIONS_HEADER.format(instructions=instructions.strip())
        prompt += f"\n\nPR DIFF (data):\n{diff}\n"
        result = run_executor(args, prompt=prompt, timeout=timeout)
        if not result.ok or not result.stdout.strip():
            reason = "timed out" if result.timed_out else result.output or "empty reviewer output"
            return {"role": role, "ran": False, "findings": [], "error": f"reviewer failed: {reason}", "output": ""}
        findings = core.parse_reviewer_output(result.stdout)
        return {
            "role": role,
            "ran": True,
            "findings": [finding.__dict__ for finding in findings],
            "output": result.stdout,
        }
    except Exception as error:
        return {"role": role, "ran": False, "findings": [], "error": f"reviewer failed: {error}", "output": ""}


def max_reviewer_workers(role_count: int, environ: dict[str, str] | None = None) -> int:
    env = os.environ if environ is None else environ
    try:
        cap = int(env.get("MACHINIST_REVIEW_MAX_PARALLEL", ""))
    except (TypeError, ValueError):
        return max(1, role_count)
    if cap < 1:
        return max(1, role_count)
    return max(1, min(role_count, cap))


def run_reviewers(
    roles: list[str],
    args: list[str],
    diff: str,
    timeout: int,
    instructions: str = "",
) -> list[dict[str, Any]]:
    if len(roles) == 1:
        return [run_reviewer(roles[0], args, diff, timeout, instructions)]
    with ThreadPoolExecutor(max_workers=max_reviewer_workers(len(roles))) as pool:
        futures = {pool.submit(run_reviewer, role, args, diff, timeout, instructions): role for role in roles}
        return [future.result() for future in as_completed(futures)]


def docs_prompt(diff: str, pr_body: str, repo: Path, instructions: str = "") -> str:
    prompt = (PROMPTS_DIR / "docs-reviewer.md").read_text(encoding="utf-8")
    if instructions.strip():
        prompt += INSTRUCTIONS_HEADER.format(instructions=instructions.strip())
    prompt += "\n\nReturn a suggested pull-request description between <PR_BODY> and </PR_BODY> using the house format.\n"
    sections = [f"PR DIFF (data):\n{diff}", f"CURRENT PR BODY (data):\n{pr_body}"]
    for filename in ("README.md", "CLAUDE.md", "AGENTS.md"):
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
    instructions: str = "",
) -> tuple[dict[str, Any], list[str]]:
    docs: dict[str, Any] = {"ran": True, "changed": [], "pr_description_updated": False, "error": None}
    before = set(changed_paths(repo, timeout))
    result = run_executor(args, prompt=docs_prompt(diff, metadata["body"], repo, instructions), timeout=timeout)
    docs["raw"] = result.stdout
    if not result.ok:
        docs["ran"] = False
        docs["error"] = result.output or "docs reviewer failed"
        return docs, []
    after = set(changed_paths(repo, timeout))
    docs_changed = sorted((after - before) & {"README.md", "CLAUDE.md", "AGENTS.md"})
    docs["changed"] = docs_changed
    stale_line = next((line for line in result.stdout.splitlines() if line.startswith("PR_DESCRIPTION_STALE:")), "")
    docs["pr_description_stale"] = "yes" in stale_line.lower()
    if docs_changed and not dry_run:
        branch = metadata["headRefName"]
        if not commit_allowed(repo, docs_changed, "Update docs from PR review", timeout):
            docs["error"] = "could not commit documentation changes"
        elif not push_branch(repo, branch, timeout).ok:
            docs["error"] = "could not push documentation changes"
    suggested = parse_suggested_body(result.stdout)
    if docs.get("pr_description_stale") and suggested:
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


def render_comment(status: str, checks: list[ReviewCheck], reviewers: list[dict[str, Any]], docs: dict[str, Any]) -> str:
    lines = ["Machinist PR review", f"Status: {status}", ""]
    findings = [(reviewer["role"], core.Finding(**finding)) for reviewer in reviewers for finding in reviewer["findings"]]
    blocking = [item for item in findings if item[1].severity in {"critical", "bug"}]
    if blocking:
        lines.append("## CRITICAL")
        for role, finding in blocking:
            lines.append(f"- {finding.severity} | {finding.location or '(unparsed)'} | {finding.message} — {role}")
        lines.append("")
    lines.append("## Checks")
    if checks:
        for check in checks:
            state = check.classification
            lines.append(f"- {state}: {check.language} {check.name}")
            if check.result and check.result.output:
                tail = "\n".join(check.result.output.splitlines()[-60:])
                lines.append(f"  ```\n  {tail.replace(chr(10), chr(10) + '  ')}\n  ```")
    else:
        lines.append("- No configured checks.")
    lines.extend(["", "## Reviewers"])
    if reviewers:
        for reviewer in reviewers:
            lines.append(f"### {reviewer['role']}")
            if not reviewer["ran"]:
                lines.append(f"- {reviewer.get('error', 'reviewer failed')}")
                continue
            grouped: dict[str, list[core.Finding]] = {}
            for item in reviewer["findings"]:
                finding = core.Finding(**item)
                grouped.setdefault(finding.severity, []).append(finding)
            if not grouped:
                lines.append("- NO FINDINGS")
            for severity in ("critical", "bug", "warning", "nit", "unparsed"):
                for finding in grouped.get(severity, []):
                    lines.append(f"- {severity} | {finding.location or '(unparsed)'} | {finding.message}")
    else:
        lines.append("- Reviewers not run: no executor")
    lines.extend(["", "## Docs"])
    if docs.get("ran"):
        lines.append(f"- Files changed: {', '.join(docs.get('changed', [])) or 'none'}")
        lines.append(f"- PR description updated: {'yes' if docs.get('pr_description_updated') else 'no'}")
    else:
        lines.append(f"- {docs.get('error', 'Docs review not run: no executor')}")
    return "\n".join(lines).rstrip() + "\n"


def planned_commands(checks: list[ReviewCheck]) -> list[str]:
    return [shlex.join(check.args) for check in checks]


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
    summary: dict[str, Any] = {"status": "INCOMPLETE", "blocking": False, "checks": [], "reviewers": [], "docs_changed": [], "comment_url": None}
    try:
        files = changed_file_list(options.repo, owner, name, number_text, metadata, options.base, options.timeout)
        classified = core.classify_changed_files(files)
        languages = core.languages_for_files(classified["code"])
        diff = pull_request_diff(options.repo, owner, name, number_text, options.timeout)
        checks = build_checks(options.repo, classified, languages)
        planned = planned_commands(checks)
        initial_changes = set(changed_paths(options.repo, options.timeout))
        if options.dry_run and planned:
            print("Planned commands:")
            for command in planned:
                print(f"  {command}")
        checks = execute_checks(options.repo, checks, metadata["baseRefOid"], options.timeout)
        remaining_new = [check for check in checks if check.classification == "NEW"]

        executor_args = executor_command(options.executor_cmd)
        budget = amend_budget(options.amend_rounds)
        budget_limit = budget
        instructions = project_instructions(
            owner, name, options.timeout, options.instructions_file
        )

        def blocking_reviewers() -> list[dict[str, Any]]:
            return [
                {**finding, "role": reviewer["role"]}
                for reviewer in reviewers
                for finding in reviewer["findings"]
                if finding["severity"] in {"critical", "bug"}
            ]

        reviewers: list[dict[str, Any]] = []
        docs: dict[str, Any] = {"ran": False, "changed": [], "pr_description_updated": False, "error": "Docs review not run: no executor"}
        if executor_args is None:
            reviewers = [{"role": role, "ran": False, "findings": [], "error": "reviewers not run: no executor"} for role in core.review_roles(classified, small_combined=False)]
        else:
            if remaining_new and budget > 0:
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
                remaining_new = [check for check in checks if check.classification == "NEW"]
        if executor_args is not None and remaining_new:
            roles = core.review_roles(classified, small_combined=False)
            reviewers = [
                {
                    "role": role,
                    "ran": False,
                    "findings": [],
                    "error": "reviewers skipped because new check failures remain",
                }
                for role in roles
            ]
        elif executor_args is not None:
            small = core.size_gate(diff, files)
            roles = core.review_roles(classified, small_combined=small)
            reviewers = run_reviewers(roles, executor_args, diff, options.timeout, instructions)
            # Blocking findings send the job back to amend on the same branch.
            # Each send-back consumes one retry-budget round; the checks and
            # reviewers rerun until a pass comes back clean or the budget is
            # spent, at which point the review is BLOCKED.
            findings = blocking_reviewers()
            while findings and budget > 0:
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
                checks = build_checks(options.repo, classified, languages)
                checks = execute_checks(options.repo, checks, metadata["baseRefOid"], options.timeout)
                remaining_new = [check for check in checks if check.classification == "NEW"]
                if remaining_new and budget > 0:
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
                    remaining_new = [check for check in checks if check.classification == "NEW"]
                if remaining_new:
                    break
                diff = pull_request_diff(options.repo, owner, name, number_text, options.timeout)
                reviewers = run_reviewers(roles, executor_args, diff, options.timeout, instructions)
                findings = blocking_reviewers()

        if executor_args is not None and not remaining_new:
            docs, docs_changed = run_docs(
                options.repo, owner, name, number_text, metadata, executor_args, diff, options.timeout, options.dry_run, instructions
            )
            summary["docs_changed"] = docs_changed

        blocking = any(check.classification == "NEW" for check in checks)
        if blocking_reviewers():
            blocking = True
        failed_reviewers = [reviewer for reviewer in reviewers if not reviewer["ran"] and "no executor" not in reviewer.get("error", "")]
        incomplete = bool(failed_reviewers) or (executor_args is not None and not docs.get("ran"))
        if docs.get("error"):
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
                        "output": (check.result.output[-4000:] if check.result else ""),
                    }
                    for check in checks
                ],
                "reviewers": reviewers,
                "docs": docs,
                "amend_rounds": {"used": budget_limit - budget, "limit": budget_limit},
            }
        )
        comment = render_comment(status, checks, reviewers, docs)
        print(comment)
        state = STATUS_STATES[status]
        if options.dry_run:
            print("Planned command: gh pr comment ... --body-file -")
            print(f"Planned command: gh api repos/{owner}/{name}/statuses/{metadata['headRefOid']} ...")
        else:
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
                    f"repos/{owner}/{name}/statuses/{metadata['headRefOid']}",
                    "-f",
                    f"state={state}",
                    "-f",
                    f"context={STATUS_CONTEXT}",
                    "-f",
                    f"description=Machinist review {status}",
                ],
                options.timeout,
            )
            if not comment_result.ok or not status_result.ok:
                raise ReviewError(f"could not publish review: {comment_result.output} {status_result.output}")
        if status == "INCOMPLETE":
            print(
                "post-pr review warning: review incomplete; exiting 0 so the delivery job does not fail",
                file=sys.stderr,
            )
        return EXIT_CODES[status], summary
    finally:
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
            instructions_file=args.instructions_file,
        )
    except Exception as error:
        print(f"post-pr review error: {error}", file=sys.stderr)
        return EXIT_CODES["INCOMPLETE"]
    try:
        code, summary = review(options)
    except Exception as error:
        summary = {"status": "INCOMPLETE", "blocking": False, "checks": [], "reviewers": [], "docs_changed": [], "comment_url": None, "error": str(error)}
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
