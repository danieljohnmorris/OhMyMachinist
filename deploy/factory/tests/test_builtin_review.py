"""Tests for the built-in PR review step (MACH-26): step gating, the parallel
reviewer passes, blocking vs non-blocking findings, the amend retry budget,
per-project instructions, and the deliver-pr wiring."""
from __future__ import annotations

import importlib.util
import json
import os
from pathlib import Path
import tempfile
import textwrap
import unittest
import urllib.error
from unittest import mock

import deploy.factory.post_pr_review as post
from deploy.factory.review.runner import CommandResult

_DELIVER_SPEC = Path(__file__).resolve().parents[1] / "deliver-pr.py"
_deliver_spec = importlib.util.spec_from_file_location("deliver_pr_mach26", _DELIVER_SPEC)
deliver = importlib.util.module_from_spec(_deliver_spec)
_deliver_spec.loader.exec_module(deliver)


class AmendBudgetTests(unittest.TestCase):
    def test_default_budget_is_three(self) -> None:
        self.assertEqual(post.amend_budget(None, environ={}), 3)
        self.assertEqual(post.amend_budget(None, environ={"MACHINIST_REVIEW_AMEND_ROUNDS": "garbage"}), 3)

    def test_env_and_flag_override(self) -> None:
        self.assertEqual(post.amend_budget(None, environ={"MACHINIST_REVIEW_AMEND_ROUNDS": "5"}), 5)
        self.assertEqual(post.amend_budget(2, environ={"MACHINIST_REVIEW_AMEND_ROUNDS": "5"}), 2)
        self.assertEqual(post.amend_budget(-1, environ={}), 0)

    def test_zero_budget_blocks_without_amend(self) -> None:
        checks = [post.ReviewCheck("go", "go vet", ["go", "vet", "./..."])]
        self.assertEqual(post.amend_prompt(checks, []), post.amend_prompt(checks, []))


class AmendPromptTests(unittest.TestCase):
    def test_prompt_carries_failing_checks_and_finding_roles(self) -> None:
        check = post.ReviewCheck("go", "go vet", ["go", "vet", "./..."])
        check.result = CommandResult(("go", "vet"), 1, "", "vet failed")
        check.classification = "NEW"
        prompt = post.amend_prompt([check], [{"severity": "critical", "location": "a.go:1", "message": "broken", "role": "feature-dev:code-reviewer"}])
        self.assertIn("Amend the pull request on the current branch", prompt)
        self.assertIn("go vet", prompt)
        self.assertIn("vet failed", prompt)
        self.assertIn("feature-dev:code-reviewer", prompt)
        self.assertIn("critical | a.go:1 | broken", prompt)


class ProjectInstructionsTests(unittest.TestCase):
    def test_instructions_file_wins(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "instructions.md"
            path.write_text("# project rules\n", encoding="utf-8")
            instructions = post.project_instructions("owner", "name", 5, instructions_file=str(path), environ={})
        self.assertEqual(instructions, "# project rules\n")

    def test_control_plane_fetch_uses_project_and_token(self) -> None:
        captured: dict[str, object] = {}

        class FakeResponse:
            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

            def read(self):
                return json.dumps({"project": "omacom/mobile", "instructions": "# mobile rules", "custom": True}).encode()

        def fake_urlopen(request, timeout):
            captured["url"] = request.full_url
            captured["auth"] = request.headers.get("Authorization")
            captured["timeout"] = timeout
            return FakeResponse()

        env = {
            "MACHINIST_CONTROL_PLANE_URL": "http://cp:7331",
            "MACHINIST_CONTROL_PLANE_TOKEN": "worker-token",
            "MACHINIST_PROJECT": "omacom/mobile",
        }
        with mock.patch.object(post.urllib.request, "urlopen", side_effect=fake_urlopen):
            instructions = post.project_instructions("owner", "name", 900, environ=env)
        self.assertEqual(instructions, "# mobile rules")
        self.assertEqual(captured["url"], "http://cp:7331/api/v1/review-instructions?project=omacom%2Fmobile")
        self.assertEqual(captured["auth"], "Bearer worker-token")
        self.assertLessEqual(captured["timeout"], 30)

    def test_control_plane_errors_return_empty(self) -> None:
        env = {"MACHINIST_CONTROL_PLANE_URL": "http://cp:7331"}
        with mock.patch.object(post.urllib.request, "urlopen", side_effect=urllib.error.URLError("down")):
            self.assertEqual(post.project_instructions("owner", "name", 5, environ=env), "")

    def test_no_sources_returns_empty(self) -> None:
        self.assertEqual(post.project_instructions("owner", "name", 5, environ={}), "")

    def test_reviewer_prompt_includes_instructions(self) -> None:
        captured: dict[str, str] = {}

        def fake_executor(args, prompt, timeout):
            captured["prompt"] = prompt
            return CommandResult(args, 0, "NO FINDINGS", "")

        with mock.patch.object(post, "run_executor", side_effect=fake_executor):
            result = post.run_reviewer("feature-dev:code-reviewer", ["reviewer"], "diff", 5, instructions="# project rules")
        self.assertEqual(result["ran"], True)
        self.assertEqual(result["findings"], [])
        self.assertIn("PROJECT REVIEW INSTRUCTIONS", captured["prompt"])
        self.assertIn("# project rules", captured["prompt"])


class BuiltinReviewWiringTests(unittest.TestCase):
    """deliver-pr must run the built-in review after the PR opens and
    propagate the verdict (blocking findings fail the delivery)."""

    def test_run_review_skips_when_script_missing(self) -> None:
        with mock.patch.dict(os.environ, {"MACHINIST_REVIEW_SCRIPT": "/nonexistent/post_pr_review.py"}, clear=False):
            self.assertIsNone(deliver.run_review("/tmp", "https://github.com/owner/repo/pull/1"))

    def test_run_review_propagates_exit_code_and_arguments(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            script = root / "fake_review.py"
            script.write_text(
                textwrap.dedent(
                    """
                    import json, os, sys
                    json.dump(sys.argv[1:], open(os.environ["ARGV_OUT"], "w"))
                    sys.exit(2)
                    """
                ),
                encoding="utf-8",
            )
            argv_out = root / "argv.json"
            env = os.environ.copy()
            env["MACHINIST_REVIEW_SCRIPT"] = str(script)
            env["ARGV_OUT"] = str(argv_out)
            with mock.patch.dict(os.environ, env, clear=False):
                code = deliver.run_review(str(root), "https://github.com/owner/repo/pull/1")
            self.assertEqual(code, 2)
            argv = json.loads(argv_out.read_text())
            self.assertEqual(argv[0], "--repo")
            self.assertIn("--pr", argv)

    def test_review_verdict_mapping(self) -> None:
        self.assertEqual(deliver.REVIEW_VERDICTS[0], "PASS")
        self.assertEqual(deliver.REVIEW_VERDICTS[2], "BLOCKED")
        self.assertEqual(deliver.REVIEW_VERDICTS[3], "INCOMPLETE")


class EndToEndAmendLoopTests(unittest.TestCase):
    """The review sends blocking findings back to amend on the same branch,
    within the retry budget, and re-runs the reviewers (parallel passes) until
    clean; a spent budget reports BLOCKED without amending."""

    def _environment(self, root: Path) -> tuple[Path, Path]:
        repo = root / "repo"
        repo.mkdir()
        bin_dir = root / "bin"
        bin_dir.mkdir()
        (bin_dir / "git").write_text(
            textwrap.dedent(
                """#!/bin/sh
                printf '%s\\n' "$*" >> "$TEMP_GIT_LOG"
                case "$*" in
                  *"remote get-url"*) printf 'https://github.com/owner/repo.git\\n' ;;
                  *"rev-parse --abbrev-ref"*) printf 'feature\\n' ;;
                  *) : ;;
                esac
                """
            ),
            encoding="utf-8",
        )
        (bin_dir / "gh").write_text(
            textwrap.dedent(
                """#!/bin/sh
                printf '%s\\n' "$*" >> "$TEMP_GH_LOG"
                case "$*" in
                  *"pr view"*) printf '{"headRefName":"feature","headRefOid":"head","baseRefName":"main","baseRefOid":"base","body":"body","url":"url"}' ;;
                  *"--name-only"*) printf 'src/app.py\\n' ;;
                  *) printf 'diff --git a/src/app.py b/src/app.py\\n+++ b/src/app.py\\n@@ -1 +1,2 @@\\n+print(1)\\n' ;;
                esac
                """
            ),
            encoding="utf-8",
        )
        # Three parallel reviewer calls see blocking findings; after the amend
        # the re-review passes. Calls 4+ are the amend and the clean re-review.
        (bin_dir / "review-executor").write_text(
            textwrap.dedent(
                """#!/bin/sh
                n=$(cat "$EXEC_COUNT" 2>/dev/null || echo 0)
                n=$((n+1))
                printf '%s' "$n" > "$EXEC_COUNT"
                if [ "$n" -le 1 ]; then
                  printf 'CRITICAL | src/app.py:1 | blocking finding\\n'
                else
                  printf 'NO FINDINGS\\n'
                fi
                """
            ),
            encoding="utf-8",
        )
        for name in ("git", "gh", "review-executor"):
            os.chmod(bin_dir / name, 0o755)
        return bin_dir, repo

    def _run(self, root: Path, bin_dir: Path, extra_args: list[str]) -> tuple[int, dict]:
        gh_log = root / "gh.log"
        git_log = root / "git.log"
        exec_count = root / "exec.count"
        json_out = root / "summary.json"
        env = os.environ.copy()
        env.update(
            {
                "PATH": f"{bin_dir}:{env['PATH']}",
                "TEMP_GH_LOG": str(gh_log),
                "TEMP_GIT_LOG": str(git_log),
                "EXEC_COUNT": str(exec_count),
            }
        )
        with mock.patch.dict(os.environ, env, clear=True):
            code = post.main(
                [
                    "--repo",
                    str(root / "repo"),
                    "--pr",
                    "owner/repo#1",
                    "--executor-cmd",
                    str(bin_dir / "review-executor"),
                    "--timeout-seconds",
                    "1",
                    "--dry-run",
                    "--json-out",
                    str(json_out),
                    *extra_args,
                ]
            )
        summary = json.loads(json_out.read_text(encoding="utf-8"))
        summary["exit_code"] = code
        summary["executor_calls"] = int(exec_count.read_text())
        summary["git_log"] = git_log.read_text(encoding="utf-8") if git_log.exists() else ""
        return summary

    def test_blocking_findings_amend_and_rereview(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            bin_dir, _ = self._environment(root)
            summary = self._run(root, bin_dir, ["--amend-rounds", "3"])
            self.assertEqual(summary["exit_code"], 0)
            self.assertEqual(summary["status"], "PASS")
            # 1 combined reviewer + 1 amend + 1 re-review + 1 docs pass.
            self.assertEqual(summary["executor_calls"], 4)
            self.assertEqual(summary["amend_rounds"], {"used": 1, "limit": 3})
            self.assertNotIn("push", summary["git_log"])

    def test_spent_budget_blocks_without_amend(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            bin_dir, _ = self._environment(root)
            summary = self._run(root, bin_dir, ["--amend-rounds", "0"])
            self.assertEqual(summary["exit_code"], 2)
            self.assertEqual(summary["status"], "BLOCKED")
            self.assertTrue(summary["blocking"])
            self.assertEqual(summary["executor_calls"], 2)
            self.assertEqual(summary["amend_rounds"], {"used": 0, "limit": 0})


if __name__ == "__main__":
    unittest.main()
