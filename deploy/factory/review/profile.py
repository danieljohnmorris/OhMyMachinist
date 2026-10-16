"""Review profiles (MACH-32): declarative, user-editable review pipelines.

A profile is YAML (or markdown with YAML frontmatter) describing the lanes and
checks to run, the reviewer passes, routing by file glob, docs handling,
blocking policy and output shape. Resolution order (first match wins):

1. ``.machinist/review.yaml`` (or ``.machinist/review.md``) in the repository
   at the PR base ref, so a PR cannot weaken its own review.
2. The project's profile in the control plane (a library profile name or a
   full profile body), plus optional per-project extra instructions.
3. The instance-wide default profile (config ``review.default_profile``),
   which falls back to the shipped ``default`` profile.
4. The bundled shipped default profile, for workers with no control plane.

Nothing about checks, reviewer roles or routing is hard-coded in the review
step any more; :mod:`post_pr_review` runs entirely from the resolved profile.
"""

from __future__ import annotations

from dataclasses import dataclass, field
import os
from pathlib import Path
import re
import shlex
import shutil
from typing import Any, Callable
import urllib.error
import urllib.parse
import urllib.request

import yaml

from . import core


DEFAULT_PROFILE_NAME = "default"
REPO_PROFILE_PATHS = (".machinist/review.yaml", ".machinist/review.md")
KNOWN_LANGUAGES = ("py", "shell", "ts", "go", "rust", "ruby", "javascript")
KNOWN_REPORT_SECTIONS = ("head_sha", "findings", "checks", "passes", "docs", "blockers")

# Test seam: when set, used instead of the HTTP control-plane fetch.
_control_plane_fetch: Callable[[dict[str, str], int], dict[str, Any] | None] | None = None


class ProfileError(Exception):
    """Profile parsing/validation failure with line-numbered errors."""

    def __init__(self, errors: list[tuple[int, str]], source: str = ""):
        prefix = f"{source}: " if source else ""
        rendered = "\n".join(f"line {line}: {message}" for line, message in errors) or "invalid profile"
        super().__init__(prefix + rendered)
        self.errors = errors
        self.source = source


@dataclass
class ReviewCheck:
    """One planned check command for a lane."""

    language: str
    name: str
    args: list[str]
    cwd: str = "."
    result: Any = None
    classification: str = "not run"
    note: str = ""

    @property
    def ok(self) -> bool:
        return bool(self.result and self.result.ok)


@dataclass
class ResolvedProfile:
    """A profile body plus where it came from and its extra instructions."""

    body: dict[str, Any]
    raw: str
    source: str
    extra_instructions: str = ""

    @property
    def name(self) -> str:
        return str(self.body.get("name", ""))

    def amend_rounds(self) -> int:
        blocking = self.body.get("blocking") or {}
        try:
            return max(0, int(blocking.get("amend_rounds", 3)))
        except (TypeError, ValueError):
            return 3

    def severities(self) -> set[str]:
        blocking = self.body.get("blocking") or {}
        values = blocking.get("severities") or ["critical", "bug"]
        return {str(value).lower() for value in values}

    def report_sections(self) -> list[str] | None:
        output = self.body.get("output") or {}
        sections = output.get("report")
        if not isinstance(sections, list):
            return None
        return [str(section) for section in sections]


# ---------------------------------------------------------------------------
# Loading and validation


def _extract_frontmatter(text: str) -> str:
    """Accept markdown with YAML frontmatter as well as plain YAML."""
    lines = text.splitlines()
    if not lines or lines[0].strip() != "---":
        return text
    for index in range(1, len(lines)):
        if lines[index].strip() == "---":
            return "\n".join(lines[1:index])
    raise ProfileError([(1, "frontmatter opened with --- but never closed")])


def _glob_errors(pattern: Any, line: int) -> str | None:
    if not isinstance(pattern, str) or not pattern.strip():
        return "glob patterns must be non-empty strings"
    try:
        glob_to_regex(pattern)
    except re.error as error:
        return f"invalid glob {pattern!r}: {error}"
    return None


def glob_to_regex(pattern: str) -> re.Pattern:
    """Compile a file glob supporting ``**`` (any depth), ``*`` and ``?``."""
    parts = pattern.split("/")
    regex = ""
    for index, part in enumerate(parts):
        if part == "**":
            if index == len(parts) - 1:
                regex += ".+"
            else:
                # The group consumes the trailing slash itself.
                regex += "(?:[^/]+/)*"
                continue
        else:
            for char in part:
                if char == "*":
                    regex += "[^/]*"
                elif char == "?":
                    regex += "[^/]"
                else:
                    regex += re.escape(char)
        if index != len(parts) - 1:
            regex += "/"
    return re.compile("^" + regex + "$")


def glob_match(pattern: str, path: str) -> bool:
    normalized = core.normalize_path(path)
    regex = glob_to_regex(pattern)
    if "/" not in pattern:
        # Slashless patterns match the basename at any depth (e.g. Cargo.toml).
        if regex.match(normalized.rsplit("/", 1)[-1]):
            return True
    return regex.match(normalized) is not None


def glob_matches_any(patterns: Any, files: list[str]) -> bool:
    if not isinstance(patterns, list):
        return False
    return any(glob_match(str(pattern), path) for path in files for pattern in patterns if isinstance(pattern, str))


def validate_profile(root: Any) -> list[tuple[int, str]]:
    """Structural validation over the composed YAML node tree (line numbers)."""
    errors: list[tuple[int, str]] = []

    def line(node: Any) -> int:
        mark = getattr(node, "start_mark", None)
        return mark.line + 1 if mark is not None else 1

    def scalars(node: Any) -> list[tuple[Any, Any]]:
        return list(node.value) if isinstance(node, yaml.MappingNode) else []

    def map_get(node: Any, key: str) -> Any:
        if not isinstance(node, yaml.MappingNode):
            return None
        for key_node, value_node in node.value:
            if isinstance(key_node, yaml.ScalarNode) and key_node.value == key:
                return value_node
        return None

    # With `extends`, reviewers/lanes may be inherited from the base profile;
    # reference existence is checked after the merge instead.
    has_extends = map_get(root, "extends") is not None

    def mapping_keys(node: Any) -> list[str]:
        return [k.value for k, _ in scalars(node) if isinstance(k, yaml.ScalarNode)]

    if not isinstance(root, yaml.MappingNode):
        return [(1, "profile must be a YAML mapping")]

    known_top = {"name", "version", "description", "source", "extends", "extra_instructions", "scope", "checks", "concurrency", "reviewers", "passes", "routing", "reconcile", "docs", "blocking", "output", "skills"}
    for key_node, _ in scalars(root):
        if isinstance(key_node, yaml.ScalarNode) and key_node.value not in known_top:
            errors.append((line(key_node), f"unknown top-level key: {key_node.value}"))
    name_node = map_get(root, "name")
    if name_node is None or not isinstance(name_node, yaml.ScalarNode) or not str(name_node.value).strip():
        errors.append((line(root), "name is required"))
    version_node = map_get(root, "version")
    if version_node is None or not isinstance(version_node, yaml.ScalarNode) or str(version_node.value) != "1":
        errors.append((line(root), "version must be 1"))

    scope = map_get(root, "scope")
    if scope is not None:
        if not isinstance(scope, yaml.MappingNode):
            errors.append((line(scope), "scope must be a mapping"))
        else:
            allowed = {"pin_head_sha", "lanes_from", "mode"}
            for key_node, _ in scalars(scope):
                if isinstance(key_node, yaml.ScalarNode) and key_node.value not in allowed:
                    errors.append((line(key_node), f"unknown scope key: {key_node.value}"))
            lanes_from = map_get(scope, "lanes_from")
            if lanes_from is not None and str(lanes_from.value) not in {"diff", "repo"}:
                errors.append((line(lanes_from), "scope.lanes_from must be diff or repo"))
            mode = map_get(scope, "mode")
            if mode is not None and str(mode.value) not in {"review_only", "amend"}:
                errors.append((line(mode), "scope.mode must be review_only or amend"))

    checks = map_get(root, "checks")
    lane_names: set[str] = set()
    if checks is not None:
        lanes: list[Any] = []
        if isinstance(checks, yaml.MappingNode):
            for key_node, _ in scalars(checks):
                if isinstance(key_node, yaml.ScalarNode) and key_node.value not in {"blocking", "missing_tool", "lanes"}:
                    errors.append((line(key_node), f"unknown checks key: {key_node.value}"))
            blocking = map_get(checks, "blocking")
            if blocking is not None and str(blocking.value) not in {"new_failures", "all"}:
                errors.append((line(blocking), "checks.blocking must be new_failures or all"))
            missing = map_get(checks, "missing_tool")
            if missing is not None and str(missing.value) not in {"blocked", "skip", "pass"}:
                errors.append((line(missing), "checks.missing_tool must be blocked, skip or pass"))
            lanes_node = map_get(checks, "lanes")
            if lanes_node is not None:
                lanes = list(lanes_node.value) if isinstance(lanes_node, yaml.SequenceNode) else []
                if not isinstance(lanes_node, yaml.SequenceNode):
                    errors.append((line(lanes_node), "checks.lanes must be a list"))
        elif isinstance(checks, yaml.SequenceNode):
            # Lenient shape: a bare list of lanes, optionally with
            # blocking/missing_tool entries mixed in.
            for item in checks.value:
                if isinstance(item, yaml.MappingNode) and map_get(item, "lane") is None:
                    first_key = mapping_keys(item)
                    if first_key and all(key in {"blocking", "missing_tool"} for key in first_key):
                        for policy_key, allowed in (("blocking", {"new_failures", "all"}), ("missing_tool", {"blocked", "skip", "pass"})):
                            policy_node = map_get(item, policy_key)
                            if policy_node is not None and str(policy_node.value) not in allowed:
                                errors.append((line(policy_node), f"checks.{policy_key} must be {', '.join(sorted(allowed))}"))
                        continue
                lanes.append(item)
        else:
            errors.append((line(checks), "checks must be a mapping or a list of lanes"))
        for lane_node in lanes:
            if not isinstance(lane_node, yaml.MappingNode):
                errors.append((line(lane_node), "each lane must be a mapping"))
                continue
            lane_name_node = map_get(lane_node, "lane")
            if lane_name_node is None:
                errors.append((line(lane_node), "lane requires a lane name"))
            else:
                lane_name = str(lane_name_node.value)
                if lane_name in lane_names:
                    errors.append((line(lane_name_node), f"duplicate lane: {lane_name}"))
                lane_names.add(lane_name)
            allowed_lane_keys = {"lane", "when", "commands", "post_deploy"}
            for key_node, _ in scalars(lane_node):
                if isinstance(key_node, yaml.ScalarNode) and key_node.value not in allowed_lane_keys:
                    errors.append((line(key_node), f"unknown lane key: {key_node.value}"))
            when = map_get(lane_node, "when")
            if when is not None:
                if not isinstance(when, yaml.SequenceNode):
                    errors.append((line(when), "lane.when must be a list of globs"))
                else:
                    for pattern_node in when.value:
                        pattern = str(pattern_node.value) if isinstance(pattern_node, yaml.ScalarNode) else ""
                        problem = _glob_errors(pattern, line(pattern_node))
                        if problem:
                            errors.append((line(pattern_node), problem))
            commands = map_get(lane_node, "commands")
            if commands is None or not isinstance(commands, yaml.SequenceNode):
                errors.append((line(lane_node), "lane.commands must be a list"))
            else:
                for command_node in commands.value:
                    if isinstance(command_node, yaml.ScalarNode):
                        if not str(command_node.value).strip():
                            errors.append((line(command_node), "command must not be empty"))
                        continue
                    if not isinstance(command_node, yaml.MappingNode):
                        errors.append((line(command_node), "commands must be strings or mappings"))
                        continue
                    allowed_command_keys = {"run", "if_exists", "if_script", "if_configured", "optional", "else", "requires_authorization"}
                    for key_node, _ in scalars(command_node):
                        if isinstance(key_node, yaml.ScalarNode) and key_node.value not in allowed_command_keys:
                            errors.append((line(key_node), f"unknown command key: {key_node.value}"))
                    run_node = map_get(command_node, "run")
                    if run_node is None or not isinstance(run_node, yaml.ScalarNode) or not str(run_node.value).strip():
                        errors.append((line(command_node), "command requires a run string"))

    reviewers_node = map_get(root, "reviewers") or map_get(root, "passes")
    reviewer_names: set[str] = set()
    if reviewers_node is not None:
        if not isinstance(reviewers_node, yaml.SequenceNode):
            errors.append((line(reviewers_node), "reviewers must be a list"))
        else:
            for reviewer_node in reviewers_node.value:
                if not isinstance(reviewer_node, yaml.MappingNode):
                    errors.append((line(reviewer_node), "each reviewer must be a mapping"))
                    continue
                allowed_reviewer_keys = {"name", "skills", "skill", "prompt", "prompt_from", "model", "effort", "parallel", "when", "lanes", "read_only", "optional", "trigger", "routing_only"}
                for key_node, _ in scalars(reviewer_node):
                    if isinstance(key_node, yaml.ScalarNode) and key_node.value not in allowed_reviewer_keys:
                        errors.append((line(key_node), f"unknown reviewer key: {key_node.value}"))
                name_node = map_get(reviewer_node, "name")
                if name_node is None or not isinstance(name_node, yaml.ScalarNode) or not str(name_node.value).strip():
                    errors.append((line(reviewer_node), "reviewer requires a name"))
                else:
                    reviewer_name = str(name_node.value)
                    if reviewer_name in reviewer_names:
                        errors.append((line(name_node), f"duplicate reviewer: {reviewer_name}"))
                    reviewer_names.add(reviewer_name)
                skills = map_get(reviewer_node, "skills") or map_get(reviewer_node, "skill")
                prompt = map_get(reviewer_node, "prompt") or map_get(reviewer_node, "prompt_from")
                if skills is None and prompt is None:
                    errors.append((line(reviewer_node), "reviewer requires skills or a prompt"))
                effort = map_get(reviewer_node, "effort")
                if effort is not None and str(effort.value) not in {"low", "medium", "high"}:
                    errors.append((line(effort), "effort must be low, medium or high"))
                trigger = map_get(reviewer_node, "trigger")
                if trigger is not None and str(trigger.value) != "on_request":
                    errors.append((line(trigger), "trigger must be on_request"))
                lanes = map_get(reviewer_node, "lanes")
                if lanes is not None:
                    if not isinstance(lanes, yaml.SequenceNode):
                        errors.append((line(lanes), "reviewer.lanes must be a list"))
                    else:
                        for lane_ref in lanes.value:
                            lane_name = str(lane_ref.value) if isinstance(lane_ref, yaml.ScalarNode) else ""
                            # With `extends`, lanes may be defined on the base
                            # profile; cross-profile refs are checked at merge.
                            if lane_name and lane_name not in lane_names and not has_extends:
                                errors.append((line(lane_ref), f"reviewer references unknown lane: {lane_name}"))

    routing = map_get(root, "routing")
    if routing is not None:
        rules: list[Any] = []
        if isinstance(routing, yaml.SequenceNode):
            for item in routing.value:
                if isinstance(item, yaml.MappingNode) and map_get(item, "when") is None:
                    continue
                rules.append(item)
        elif isinstance(routing, yaml.MappingNode):
            for key_node, _ in scalars(routing):
                if isinstance(key_node, yaml.ScalarNode) and key_node.value not in {"fallback", "rules"}:
                    errors.append((line(key_node), f"unknown routing key: {key_node.value}"))
            rules_node = map_get(routing, "rules")
            if rules_node is not None and isinstance(rules_node, yaml.SequenceNode):
                rules = list(rules_node.value)
        else:
            errors.append((line(routing), "routing must be a mapping or a list of rules"))
        for rule_node in rules:
            if not isinstance(rule_node, yaml.MappingNode):
                errors.append((line(rule_node), "each routing rule must be a mapping"))
                continue
            for key_node, _ in scalars(rule_node):
                if isinstance(key_node, yaml.ScalarNode) and key_node.value not in {"when", "passes"}:
                    errors.append((line(key_node), f"unknown routing rule key: {key_node.value}"))
            passes = map_get(rule_node, "passes")
            if passes is None or not isinstance(passes, yaml.SequenceNode):
                errors.append((line(rule_node), "routing rule requires passes"))
            else:
                for pass_ref in passes.value:
                    pass_name = str(pass_ref.value) if isinstance(pass_ref, yaml.ScalarNode) else ""
                    if pass_name and pass_name not in reviewer_names and not has_extends:
                        errors.append((line(pass_ref), f"routing references unknown pass: {pass_name}"))
            when = map_get(rule_node, "when")
            if when is not None and not isinstance(when, yaml.SequenceNode):
                errors.append((line(when), "routing rule when must be a list of globs"))
        fallback = map_get(routing, "fallback") if isinstance(routing, yaml.MappingNode) else None
        if fallback is None and isinstance(routing, yaml.SequenceNode):
            for item in routing.value:
                if isinstance(item, yaml.MappingNode) and map_get(item, "fallback") is not None:
                    fallback = map_get(item, "fallback")
        if fallback is not None:
            if not isinstance(fallback, yaml.SequenceNode):
                errors.append((line(fallback), "routing.fallback must be a list"))
            else:
                for pass_ref in fallback.value:
                    pass_name = str(pass_ref.value) if isinstance(pass_ref, yaml.ScalarNode) else ""
                    if pass_name and pass_name not in reviewer_names and not has_extends:
                        errors.append((line(pass_ref), f"routing.fallback references unknown pass: {pass_name}"))

    for key, expected in (("reconcile", {"dedupe", "verify_against_code"}), ("docs", {"enabled", "files", "update_pr_description"}), ("output", {"pr_comment", "commit_status", "report"})):
        section = map_get(root, key)
        if section is None:
            continue
        if not isinstance(section, yaml.MappingNode):
            errors.append((line(section), f"{key} must be a mapping"))
            continue
        for sub_key_node, _ in scalars(section):
            if isinstance(sub_key_node, yaml.ScalarNode) and sub_key_node.value not in expected:
                errors.append((line(sub_key_node), f"unknown {key} key: {sub_key_node.value}"))
        if key == "output":
            report = map_get(section, "report")
            if report is not None and isinstance(report, yaml.SequenceNode):
                for entry in report.value:
                    token = str(entry.value) if isinstance(entry, yaml.ScalarNode) else ""
                    if token and token not in KNOWN_REPORT_SECTIONS:
                        errors.append((line(entry), f"unknown report section: {token}"))

    blocking = map_get(root, "blocking")
    if blocking is not None:
        if not isinstance(blocking, yaml.MappingNode):
            errors.append((line(blocking), "blocking must be a mapping"))
        else:
            for sub_key_node, _ in scalars(blocking):
                if isinstance(sub_key_node, yaml.ScalarNode) and sub_key_node.value not in {"severities", "amend_rounds"}:
                    errors.append((line(sub_key_node), f"unknown blocking key: {sub_key_node.value}"))
            rounds = map_get(blocking, "amend_rounds")
            if rounds is not None and (not isinstance(rounds, yaml.ScalarNode) or not str(rounds.value).isdigit()):
                errors.append((line(rounds), "blocking.amend_rounds must be a non-negative integer"))

    concurrency = map_get(root, "concurrency")
    if concurrency is not None and (not isinstance(concurrency, yaml.ScalarNode) or not str(concurrency.value).isdigit() or int(str(concurrency.value)) < 1):
        errors.append((line(concurrency), "concurrency must be a positive integer"))

    return errors


def parse_profile(text: str, source: str = "") -> dict[str, Any]:
    """Parse and validate a profile document; raises ProfileError."""
    try:
        root = yaml.compose(_extract_frontmatter(text))
    except yaml.YAMLError as error:
        mark = getattr(error, "problem_mark", None)
        raise ProfileError([(mark.line + 1 if mark else 1, f"invalid YAML: {error}")], source) from error
    errors = validate_profile(root)
    if errors:
        raise ProfileError(errors, source)
    try:
        body = yaml.safe_load(_extract_frontmatter(text))
    except yaml.YAMLError as error:  # pragma: no cover - compose already validated
        raise ProfileError([(1, f"invalid YAML: {error}")], source) from error
    if not isinstance(body, dict):
        raise ProfileError([(1, "profile must be a YAML mapping")], source)
    if isinstance(body.get("passes"), list) and "reviewers" not in body:
        body["reviewers"] = body.pop("passes")
    routing = body.get("routing")
    if isinstance(routing, list):
        rules = [rule for rule in routing if isinstance(rule, dict) and "when" in rule]
        fallback: list[str] = []
        for item in routing:
            if isinstance(item, dict) and isinstance(item.get("fallback"), list):
                fallback = [str(name) for name in item["fallback"]]
        body["routing"] = {"fallback": fallback, "rules": rules}
    checks = body.get("checks")
    if isinstance(checks, list):
        lanes: list[dict[str, Any]] = []
        blocking = "new_failures"
        missing_tool = "blocked"
        for item in checks:
            if isinstance(item, dict) and "lane" not in item and set(item) <= {"blocking", "missing_tool"}:
                blocking = str(item.get("blocking", blocking))
                missing_tool = str(item.get("missing_tool", missing_tool))
            else:
                lanes.append(item)
        body["checks"] = {"blocking": blocking, "missing_tool": missing_tool, "lanes": lanes}
    if isinstance(body.get("extends"), str) and body["extends"].strip():
        body = _apply_extends(body, _load_library_profile)
    return body


def bundled_default_profile_text() -> str:
    """The shipped default profile. The repository copy under profiles/review
    is canonical; flat installs carry a copy next to this package."""
    here = Path(__file__).resolve().parent
    for candidate in (here.parents[2] / "profiles" / "review" / "default.yaml", here / "profiles" / "default.yaml"):
        try:
            return candidate.read_text(encoding="utf-8")
        except OSError:
            continue
    raise ProfileError([(0, "shipped default profile not found next to the review package")])


def _apply_extends(body: dict[str, Any], loader: Callable[[str], dict[str, Any] | None]) -> dict[str, Any]:
    """Merge a base profile into ``body``: mappings merge recursively, lists
    and scalars from the child win."""
    base_name = body.get("extends")
    if not base_name or not isinstance(base_name, str):
        return body
    base = loader(base_name)
    if base is None:
        raise ProfileError([(1, f"extends references unknown profile: {base_name}")])

    def merge(child: dict[str, Any], parent: dict[str, Any]) -> dict[str, Any]:
        merged = dict(parent)
        for key, value in child.items():
            if isinstance(value, dict) and isinstance(merged.get(key), dict):
                merged[key] = merge(value, merged[key])
            else:
                merged[key] = value
        return merged

    return merge(body, base)


def _load_library_profile(name: str) -> dict[str, Any] | None:
    """Load a named profile from the profile library: the shipped
    profiles/review/*.yaml in the repository layout or the copy shipped next
    to this package (flat installs). User profiles from the control plane are
    resolved before parse time, so `extends` only needs the shipped library."""
    here = Path(__file__).resolve().parent
    directories = [here.parents[2] / "profiles" / "review", here / "profiles"]
    for directory in directories:
        for candidate in sorted(directory.glob("*.yaml")):
            try:
                body = parse_profile(candidate.read_text(encoding="utf-8"), str(candidate))
            except (OSError, ProfileError):
                continue
            if str(body.get("name", "")) == name:
                return body
    return None


# ---------------------------------------------------------------------------
# Resolution


def _control_plane_profile(environ: dict[str, str], timeout: int) -> dict[str, Any] | None:
    """Fetch the effective project profile from the control plane."""
    if _control_plane_fetch is not None:  # test seam
        return _control_plane_fetch(environ, timeout)
    base_url = (environ.get("MACHINIST_CONTROL_PLANE_URL") or "").strip().rstrip("/")
    if not base_url:
        return None
    request = urllib.request.Request(f"{base_url}/api/v1/review-profile")
    project = environ.get("MACHINIST_PROJECT")
    if project:
        request = urllib.request.Request(
            f"{base_url}/api/v1/review-profile?project={urllib.parse.quote(project, safe='')}"
        )
    token = environ.get("MACHINIST_CONTROL_PLANE_TOKEN")
    if token:
        request.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(request, timeout=min(timeout, 30)) as response:
            payload = json_loads(response.read().decode("utf-8"))
    except (urllib.error.URLError, OSError, ValueError):
        return None
    return payload if isinstance(payload, dict) else None


def json_loads(payload: str) -> Any:
    import json

    return json.loads(payload)


def resolve_profile(
    repo: Path,
    base_sha: str,
    timeout: int,
    run_git_fn: Callable[..., Any],
    environ: dict[str, str] | None = None,
) -> ResolvedProfile:
    """Resolve the profile for a pull request: repo (base ref) > project >
    global default > bundled shipped default."""
    env = os.environ if environ is None else environ
    # 1. Repository profile at the base ref (a PR cannot weaken its own review).
    if base_sha:
        for path in REPO_PROFILE_PATHS:
            result = run_git_fn(repo, ["show", f"{base_sha}:{path}"], timeout)
            if result.ok and result.stdout.strip():
                body = parse_profile(result.stdout, f"{path}@{base_sha[:12]}")
                return ResolvedProfile(body=body, raw=result.stdout, source="repo")
    # 2./3. Control plane: project profile (name or body) or global default.
    remote = _control_plane_profile(env, timeout)
    if remote:
        body_text = remote.get("profile") or ""
        if isinstance(body_text, str) and body_text.strip():
            body = parse_profile(body_text, f"control-plane ({remote.get('source', 'project')})")
            extra = remote.get("extra_instructions") or ""
            if not str(body.get("extra_instructions", "")).strip() and isinstance(extra, str):
                body["extra_instructions"] = extra
            return ResolvedProfile(body=body, raw=body_text, source=str(remote.get("source") or "project"), extra_instructions=str(extra) if not str(body.get("extra_instructions", "")).strip() else str(body.get("extra_instructions", "")))
    # 4. Bundled shipped default.
    raw = bundled_default_profile_text()
    return ResolvedProfile(body=parse_profile(raw, "bundled default"), raw=raw, source="default")


# ---------------------------------------------------------------------------
# Planning


def _files_for_patterns(patterns: list[str], files: list[str]) -> list[str]:
    return [path for path in files if any(glob_match(pattern, path) for pattern in patterns)]


def lane_files(profile: dict[str, Any], changed: list[str], repo_files: list[str]) -> dict[str, list[str]]:
    """Map lane name -> files the lane applies to. ``scope.lanes_from`` picks
    the input list: the changed-file list (diff, default) or the whole repo."""
    scope = profile.get("scope") or {}
    source_files = repo_files if scope.get("lanes_from") == "repo" else changed
    mapping: dict[str, list[str]] = {}
    checks = profile.get("checks") or {}
    for lane in checks.get("lanes") or []:
        name = str(lane.get("lane", ""))
        mapping[name] = _files_for_patterns([str(pattern) for pattern in lane.get("when") or []], source_files)
    return mapping


def _language_placeholder_files(language: str, classified: dict[str, set[str]]) -> list[str]:
    key = {"py": "python", "ts": "ts"}.get(language, language)
    return sorted(path for path in classified.get("code", set()) if core.detect_language(path) == key)


def _command_available(command: str) -> bool:
    try:
        tokens = shlex.split(command)
    except ValueError:
        return False
    if not tokens:
        return False
    first = tokens[0]
    if "/" in first:
        # Relative or absolute path: the file must exist on disk.
        return Path(first).exists() or shutil.which(first) is not None
    return shutil.which(first) is not None


def _package_json_defines_script(repo: Path, lane_cwd_files: list[str], script: str) -> bool:
    roots = [Path(".")]
    for path in lane_cwd_files:
        candidate = Path(path).parent
        while True:
            if candidate not in roots:
                roots.append(candidate)
            if candidate == Path("."):
                break
            candidate = candidate.parent
    for root in sorted(roots, key=lambda item: len(item.parts), reverse=True):
        try:
            package = json_loads((repo / root / "package.json").read_text(encoding="utf-8"))
        except (OSError, ValueError):
            continue
        if isinstance(package, dict) and script in (package.get("scripts") or {}):
            return True
    return False


def _tool_configured(repo: Path, tool: str) -> bool:
    if tool == "mypy":
        return any((repo / name).is_file() for name in ("pyproject.toml", "setup.cfg", "mypy.ini", ".mypy.ini"))
    if tool == "pytest":
        return any((repo / name).is_file() for name in ("pytest.ini", "pyproject.toml", "setup.cfg", "tox.ini")) or any(
            repo.rglob("test_*.py")
        ) or any(repo.rglob("*_test.py"))
    if tool == "ruff":
        return any((repo / name).is_file() for name in ("pyproject.toml", "ruff.toml", ".ruff.toml"))
    return any((repo / name).is_file() for name in (f"{tool}.toml", f".{tool}.ini", f"{tool}.ini", f"{tool}.cfg"))


def _resolve_commands(
    repo: Path,
    lane: dict[str, Any],
    lane_changed: list[str],
    classified: dict[str, set[str]],
    missing_tool_policy: str,
    authorized: set[str],
) -> tuple[list[ReviewCheck], list[str]]:
    """Expand a lane's command entries into ReviewChecks. Returns checks and
    disclosure notes for skipped entries."""
    checks: list[ReviewCheck] = []
    notes: list[str] = []

    def expand(entry: Any, depth: int = 0) -> None:
        if depth > 4:
            return
        if isinstance(entry, str):
            text = entry.strip()
            if text:
                _append(text, {}, depth)
            return
        if not isinstance(entry, dict):
            return
        run = str(entry.get("run", "")).strip()
        if not run:
            return
        if_exists = entry.get("if_exists")
        if isinstance(if_exists, str) and if_exists:
            if_exists = [if_exists]
        if isinstance(if_exists, list) and if_exists and not all((repo / str(path)).exists() for path in if_exists):
            alternatives = entry.get("else") or []
            if isinstance(alternatives, list) and alternatives:
                for alternative in alternatives:
                    expand(alternative, depth + 1)
            else:
                notes.append(f"skipped `{run}` (missing {', '.join(str(path) for path in if_exists)})")
            return
        if_script = entry.get("if_script")
        if isinstance(if_script, str) and if_script and not _package_json_defines_script(repo, lane_changed, if_script):
            notes.append(f"skipped `{run}` (package.json has no `{if_script}` script)")
            return
        if_configured = entry.get("if_configured")
        if if_configured and not _tool_configured(repo, str(if_configured if if_configured != "true" else run.split()[0])):
            notes.append(f"skipped `{run}` ({if_configured} is not configured)")
            return
        _append(run, entry, depth)

    def _append(run: str, entry: dict[str, Any], depth: int) -> None:
        requires = entry.get("requires_authorization")
        if isinstance(requires, str) and requires and requires not in authorized:
            checks.append(
                ReviewCheck(
                    language=str(lane.get("lane", "")),
                    name=run,
                    args=[],
                    note=f"pending authorization: {requires}",
                    classification="pending authorization",
                )
            )
            return
        missing_policy = str(entry.get("missing_tool", missing_tool_policy))
        if not _command_available(run) and not entry.get("optional"):
            if missing_policy == "skip":
                notes.append(f"skipped `{run}` (tool not installed)")
                return
            if missing_policy == "pass":
                checks.append(ReviewCheck(language=str(lane.get("lane", "")), name=run, args=shlex.split(run) if run else [], note="tool missing; assumed pass", classification="assumed pass"))
                return
            checks.append(ReviewCheck(language=str(lane.get("lane", "")), name=run, args=shlex.split(run) if run else [], note="tool missing", classification="blocked"))
            return
        if entry.get("optional"):
            if not _command_available(run):
                notes.append(f"skipped optional `{run}` (tool not installed)")
                return
        _append_command(run, str(lane.get("lane", "")), lane_changed, classified, checks)

    def _append_command(run: str, lane_name: str, lane_changed: list[str], classified: dict[str, set[str]], checks: list[ReviewCheck]) -> None:
        def replace(match: re.Match) -> str:
            placeholder = match.group(1)
            if placeholder == "changed":
                files = lane_changed
            elif placeholder.startswith("changed_"):
                language = placeholder[len("changed_"):]
                files = _language_placeholder_files(language, classified)
            else:
                return match.group(0)
            if not files:
                # Nothing to scope to; run the bare command (e.g. whole repo).
                return ""
            return " ".join(shlex.quote(path) for path in files)

        expanded = re.sub(r"\{([a-z_]+)\}", replace, run).strip()
        if not expanded:
            return
        try:
            args = shlex.split(expanded)
        except ValueError:
            args = shlex.split(run)
        checks.append(ReviewCheck(language=lane_name, name=run, args=args))

    for entry in lane.get("commands") or []:
        expand(entry)
    return checks, notes


def plan_checks(
    repo: Path,
    profile: dict[str, Any],
    changed: list[str],
    classified: dict[str, set[str]],
    repo_files: list[str],
    authorized: set[str] | None = None,
) -> tuple[list[ReviewCheck], list[str]]:
    """Build the check plan for the lanes whose ``when`` globs match."""
    checks_cfg = profile.get("checks") or {}
    missing_tool_policy = str(checks_cfg.get("missing_tool", "blocked"))
    blocking_mode = str(checks_cfg.get("blocking", "new_failures"))
    if blocking_mode not in {"new_failures", "all"}:
        blocking_mode = "new_failures"
    lanes = lane_files(profile, changed, repo_files)
    checks: list[ReviewCheck] = []
    notes: list[str] = []
    for lane in checks_cfg.get("lanes") or []:
        lane_name = str(lane.get("lane", ""))
        matched = lanes.get(lane_name, [])
        if not matched:
            continue
        lane_checks, lane_notes = _resolve_commands(repo, lane, matched, classified, missing_tool_policy, authorized or set())
        checks.extend(lane_checks)
        notes.extend(lane_notes)
    return checks, notes


def select_reviewers(
    profile: dict[str, Any],
    changed: list[str],
    lane_matches: set[str],
    files_readable: bool,
    request_flag: bool,
) -> list[dict[str, Any]]:
    """Pick the reviewer passes for this PR: routing rules add their passes,
    lanes add lane-filtered reviewers, everything else falls back to the
    fallback list (or all ordinary reviewers when no fallback is configured).
    Unreadable file lists fall back; mixed PRs get every matching pass."""
    reviewers = profile.get("reviewers") or []
    routing = profile.get("routing") or {}
    fallback_names = [str(name) for name in routing.get("fallback") or []]
    by_name = {str(reviewer.get("name", "")): reviewer for reviewer in reviewers if isinstance(reviewer, dict)}

    selected: dict[str, dict[str, Any]] = {}

    def add(name: str) -> None:
        reviewer = by_name.get(name)
        if reviewer is not None and str(reviewer.get("trigger", "")) != "on_request":
            selected[name] = reviewer

    def matches_lanes(reviewer: dict[str, Any]) -> bool:
        lanes = reviewer.get("lanes") or []
        return any(str(lane) in lane_matches for lane in lanes)

    if not files_readable:
        if fallback_names:
            for name in fallback_names:
                add(name)
        else:
            # No fallback configured and no file list: run every ordinary
            # reviewer rather than silently reviewing nothing.
            for name, reviewer in by_name.items():
                if reviewer.get("routing_only") or str(reviewer.get("trigger", "")) == "on_request":
                    continue
                if not reviewer.get("lanes"):
                    selected[name] = reviewer
        return list(selected.values())

    for rule in routing.get("rules") or []:
        if not isinstance(rule, dict):
            continue
        if glob_matches_any(rule.get("when") or [], changed):
            for name in rule.get("passes") or []:
                selected[str(name)] = by_name.get(str(name), {"name": str(name)})

    for name, reviewer in by_name.items():
        if name in selected:
            continue
        if reviewer.get("routing_only") or str(reviewer.get("trigger", "")) == "on_request":
            continue
        if reviewer.get("when") and not glob_matches_any(reviewer.get("when") or [], changed):
            continue
        if reviewer.get("lanes"):
            if matches_lanes(reviewer):
                selected[name] = reviewer
            continue
        if not fallback_names or name in fallback_names:
            selected[name] = reviewer

    if request_flag:
        for name, reviewer in by_name.items():
            if str(reviewer.get("trigger", "")) == "on_request":
                selected[name] = reviewer

    ordered: list[dict[str, Any]] = []
    for reviewer in reviewers:
        if isinstance(reviewer, dict) and str(reviewer.get("name", "")) in selected:
            ordered.append(reviewer)
    for name, reviewer in selected.items():
        if all(str(item.get("name", "")) != name for item in ordered):
            ordered.append(reviewer)
    return ordered


# ---------------------------------------------------------------------------
# Reviewer passes


def skill_versions(profile: dict[str, Any]) -> dict[str, str]:
    """Resolve every referenced skill through the library: name -> version.

    Covers both the profile's top-level ``skills`` list (resolved through the
    MACH-50 configuration resolver) and each reviewer's ``skills``/``skill``.
    """
    from . import skill_library

    versions: dict[str, str] = {}
    names: list[str] = []
    for entry in profile.get("skills") or []:
        if isinstance(entry, dict) and entry.get("name"):
            names.append(str(entry["name"]))
        elif isinstance(entry, str):
            names.append(entry)
    for reviewer in profile.get("reviewers") or []:
        if not isinstance(reviewer, dict):
            continue
        skills = reviewer.get("skills") or reviewer.get("skill")
        if isinstance(skills, str):
            skills = [skills]
        names.extend(name for name in skills or [] if isinstance(name, str))
    for name in names:
        resolved = skill_library.resolve(name)
        versions[name] = resolved[1] if resolved else "unavailable"
    return versions


def reviewer_prompt(
    reviewer: dict[str, Any],
    context: dict[str, str],
    extra_instructions: str,
) -> tuple[str, str]:
    """Build the prompt for one reviewer pass. Returns (prompt, error).

    The brief is fixed: read-only, the finding format, PR coordinates, then
    the skill text (or inline prompt), project instructions, and the diff.
    ``error`` is non-empty when a required skill is unavailable."""
    from . import skill_library

    skills = reviewer.get("skills") or reviewer.get("skill")
    if isinstance(skills, str):
        skills = [skills]
    prompt_from = reviewer.get("prompt_from")
    if prompt_from:
        section = str(prompt_from).split("#", 1)
        resolved = skill_library.resolve(section[0]) if section[0] else None
        if resolved and len(section) == 2:
            # Extract the referenced section from the skill text.
            match = re.search(
                rf"^#+\s*.*{re.escape(section[1].replace('-', ' '))}.*$(.*?)(?=^#+\s|\Z)",
                resolved[0],
                re.IGNORECASE | re.MULTILINE | re.DOTALL,
            )
            text = match.group(1).strip() if match else ""
        else:
            text = resolved[0] if resolved else ""
        skills = None
        if not text:
            return "", f"unavailable skill section: {prompt_from}"
    else:
        text = ""
        for name in skills or []:
            resolved = skill_library.resolve(name)
            if resolved is None:
                if reviewer.get("optional"):
                    return "", f"optional skill unavailable: {name}"
                return "", f"unavailable skill: {name}"
            text = (text + "\n\n" + resolved[0]).strip() if text else resolved[0]
    if not text and isinstance(reviewer.get("prompt"), str):
        text = str(reviewer["prompt"])
    header = (
        f"You are the `{reviewer.get('name', 'reviewer')}` reviewer pass of a Machinist PR review "
        f"(profile: {context.get('profile_name', 'unknown')}).\n"
        "Read-only: do not modify files, do not run write commands.\n"
        f"Repository: {context.get('repository', 'unknown')}; "
        f"PR {context.get('pr', '')}; head {context.get('head_sha', 'unknown')}; "
        f"base {context.get('base_sha', 'unknown')}.\n"
    )
    effort = reviewer.get("effort")
    model = reviewer.get("model")
    if effort or model:
        header += f"Reviewing at effort {effort or 'default'} with model {model or 'default'}.\n"
    header += (
        "Report findings one per line as `SEVERITY | path:line | message` with SEVERITY in "
        "critical, bug, warning, nit; write `NO FINDINGS` on its own line if there are none.\n"
    )
    parts = [header, text.strip()]
    if extra_instructions.strip():
        parts.append(
            "PROJECT REVIEW INSTRUCTIONS (from Machinist settings; follow these in addition to the skill above):\n\n"
            + extra_instructions.strip()
        )
    parts.append(f"PR DIFF (data):\n{context.get('diff', '')}")
    return "\n\n".join(parts) + "\n", ""


def reconcile_findings(
    reviewers: list[dict[str, Any]],
    severities: set[str],
    changed_files: list[str],
    dedupe: bool = True,
    verify_against_code: bool = True,
) -> list[dict[str, Any]]:
    """Merge reviewer findings: drop exact duplicates, optionally drop
    findings whose location is outside the changed-file list, and mark the
    blocking set from the profile severities."""
    file_set = {core.normalize_path(path) for path in changed_files}
    seen: set[tuple[str, str, str]] = set()
    merged: list[dict[str, Any]] = []
    for reviewer in reviewers:
        for finding in reviewer.get("findings") or []:
            item = dict(finding)
            item["reviewer"] = reviewer.get("name", reviewer.get("role", ""))
            key = (str(item.get("severity", "")), str(item.get("location", "")), str(item.get("message", "")))
            if dedupe and key in seen:
                continue
            seen.add(key)
            location = str(item.get("location", "") or "")
            if verify_against_code and location and location != "(unparsed)":
                path = location.split(":", 1)[0]
                if file_set and core.normalize_path(path) not in file_set:
                    item["verified"] = False
                else:
                    item["verified"] = True
            item["blocking"] = str(item.get("severity", "")).lower() in severities
            merged.append(item)
    return merged


def blocking_findings(merged: list[dict[str, Any]]) -> list[dict[str, Any]]:
    return [finding for finding in merged if finding.get("blocking")]


def amend_rounds_budget(
    profile: ResolvedProfile,
    override: int | None = None,
    environ: dict[str, str] | None = None,
) -> int:
    """Retry budget for amend rounds: CLI --amend-rounds, then env
    MACHINIST_REVIEW_AMEND_ROUNDS, then the profile's blocking.amend_rounds."""
    env = os.environ if environ is None else environ
    if override is not None:
        return max(0, int(override))
    raw = env.get("MACHINIST_REVIEW_AMEND_ROUNDS", "")
    if raw.strip():
        try:
            return max(0, int(raw))
        except (TypeError, ValueError):
            pass
    return profile.amend_rounds()
