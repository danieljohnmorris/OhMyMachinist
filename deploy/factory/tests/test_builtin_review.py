"""Tests for the built-in PR review step: blocking vs non-blocking findings,
the amend retry budget from the profile, control-plane profile resolution,
reviewer prompts, and the deliver-pr wiring. The profile schema itself is
covered by review.test_profiles (MACH-32)."""
from __future__ import annotations

import importlib.util
import json
import os
from pathlib import Path
import tempfile
import textwrap
import time
import unittest
import urllib.error
from unittest import mock

import deploy.factory.post_pr_review as post
from deploy.factory.review import profile as profile_mod
from deploy.factory.review.runner import CommandResult

_DELIVER_SPEC = Path(__file__).resolve().parents[1] / "deliver-pr.py"
_deliver_spec = importlib.util.spec_from_file_location("deliver_pr_mach26", _DELIVER_SPEC)
deliver = importlib.util.module_from_spec(_deliver_spec)
_deliver_spec.loader.exec_module(deliver)


def _resolved(amend_rounds: int = 3) -> profile_mod.ResolvedProfile:
    body = profile_mod.parse_profile(
        "name: budget-test\nversion: 1\n"
        "reviewers:\n  - name: a\n    prompt: x\n"
        f"blocking:\n  amend_rounds: {amend_rounds}\n",
        "budget-test",
    )
    return profile_mod.ResolvedProfile(body=body, raw="", source="test")


class AmendBudgetTests(unittest.TestCase):
    def test_profile_budget_is_three_by_default(self) -> None:
        self.assertEqual(post.profile_mod.amend_rounds_budget(_resolved(3), None, {}), 3)

    def test_env_and_flag_override(self) -> None:
        self.assertEqual(post.profile_mod.amend_rounds_budget(_resolved(3), None, {"MACHINIST_REVIEW_AMEND_ROUNDS": "5"}), 5)
        self.assertEqual(post.profile_mod.amend_rounds_budget(_resolved(3), 2, {"MACHINIST_REVIEW_AMEND_ROUNDS": "5"}), 2)
        self.assertEqual(post.profile_mod.amend_rounds_budget(_resolved(3), -1, {}), 0)


class ReviewBudgetTests(unittest.TestCase):
    """MACH-61: the round, minute and remaining-job-time gates."""

    def test_rounds_exhausted(self) -> None:
        budget = post.ReviewBudget(1, 30, round_seconds=5, reserve_seconds=300, clock=lambda: 0)
        self.assertTrue(budget.can_start())
        budget.take()
        self.assertFalse(budget.can_start())
        self.assertEqual(budget.exhausted_reason, "review budget exhausted (rounds)")

    def test_minutes_exhausted(self) -> None:
        now = [0.0]
        budget = post.ReviewBudget(3, 1, round_seconds=5, reserve_seconds=0, clock=lambda: now[0])
        self.assertTrue(budget.can_start())
        now[0] = 61.0
        self.assertFalse(budget.can_start())
        self.assertEqual(budget.exhausted_reason, "review budget exhausted (minutes)")

    def test_job_time_too_short(self) -> None:
        budget = post.ReviewBudget(3, 30, round_seconds=60, reserve_seconds=300, job_seconds=200, clock=lambda: 0)
        self.assertFalse(budget.can_start())
        self.assertEqual(budget.exhausted_reason, "review budget exhausted (job time)")

    def test_job_guard_skipped_without_a_job_deadline(self) -> None:
        budget = post.ReviewBudget(3, 30, round_seconds=60, reserve_seconds=300, job_seconds=None, clock=lambda: 0)
        self.assertTrue(budget.can_start())

    def test_minutes_capped_to_fit_the_job_after_the_reserve(self) -> None:
        budget = post.ReviewBudget(3, 30, round_seconds=60, reserve_seconds=300, job_seconds=1200, clock=lambda: 0)
        self.assertEqual(budget.minutes, 15.0)
        self.assertTrue(budget.can_start())

    def test_job_seconds_left_from_env(self) -> None:
        env = {"MACHINIST_JOB_TIMEOUT_SECONDS": "7200", "MACHINIST_JOB_STARTED_EPOCH": "1000"}
        self.assertEqual(post.job_seconds_left(env, now=4000), 4200)
        self.assertIsNone(post.job_seconds_left({}, now=1))

    def test_summary_reports_rounds_and_minutes(self) -> None:
        budget = post.ReviewBudget(3, 30, round_seconds=5, reserve_seconds=300, clock=lambda: 0)
        budget.take()
        summary = budget.summary()
        self.assertEqual(summary["rounds"], {"used": 1, "limit": 3})
        self.assertEqual(summary["minutes"]["limit"], 30.0)


class AmendPromptTests(unittest.TestCase):
    def test_prompt_carries_failing_checks_and_finding_reviewers(self) -> None:
        check = profile_mod.ReviewCheck("go", "go vet", ["go", "vet", "./..."])
        check.result = CommandResult(("go", "vet"), 1, "", "vet failed")
        check.classification = "NEW"
        prompt = post.amend_prompt([check], [{"severity": "critical", "location": "a.go:1", "message": "broken", "reviewer": "code-reviewer"}])
        self.assertIn("Amend the pull request on the current branch", prompt)
        self.assertIn("go vet", prompt)
        self.assertIn("vet failed", prompt)
        self.assertIn("code-reviewer", prompt)
        self.assertIn("critical | a.go:1 | broken", prompt)


class ControlPlaneProfileTests(unittest.TestCase):
    """The worker fetches the effective profile from the control plane
    (repo base-ref profile and the bundled default are the other layers)."""

    def test_control_plane_fetch_uses_project_and_token(self) -> None:
        captured: dict[str, object] = {}

        class FakeResponse:
            def __enter__(self):
                return self

            def __exit__(self, *args):
                return False

            def read(self):
                return json.dumps({"project": "acme/widgets", "source": "project", "profile": "name: p\nversion: 1\n", "extra_instructions": "rules"}).encode()

        def fake_urlopen(request, timeout):
            captured["url"] = request.full_url
            captured["auth"] = request.headers.get("Authorization")
            captured["timeout"] = timeout
            return FakeResponse()

        env = {
            "MACHINIST_CONTROL_PLANE_URL": "http://cp:7331",
            "MACHINIST_CONTROL_PLANE_TOKEN": "worker-token",
            "MACHINIST_PROJECT": "acme/widgets",
        }
        with mock.patch.object(post.profile_mod.urllib.request, "urlopen", side_effect=fake_urlopen):
            payload = post.profile_mod._control_plane_profile(env, 900)
        self.assertIsNotNone(payload)
        self.assertEqual(captured["url"], "http://cp:7331/api/v1/review-profile?project=acme%2Fwidgets")
        self.assertEqual(captured["auth"], "Bearer worker-token")
        self.assertLessEqual(captured["timeout"], 30)

    def test_control_plane_errors_return_none(self) -> None:
        env = {"MACHINIST_CONTROL_PLANE_URL": "http://cp:7331"}
        with mock.patch.object(post.profile_mod.urllib.request, "urlopen", side_effect=urllib.error.URLError("down")):
            self.assertIsNone(post.profile_mod._control_plane_profile(env, 5))

    def test_no_sources_returns_none(self) -> None:
        self.assertIsNone(post.profile_mod._control_plane_profile({}, 5))

    def test_reviewer_prompt_includes_extra_instructions(self) -> None:
        captured: dict[str, str] = {}

        def fake_executor(args, prompt, timeout):
            captured["prompt"] = prompt
            return CommandResult(args, 0, "NO FINDINGS", "")

        reviewer = {"name": "code-reviewer", "skills": ["feature-dev:code-reviewer"], "read_only": True}
        context = {"profile_name": "p", "repository": "owner/name", "pr": "#1", "head_sha": "h", "base_sha": "b", "diff": "diff"}
        with mock.patch.object(post, "run_executor", side_effect=fake_executor):
            results = post.run_reviewers([reviewer], ["reviewer"], context, "# project rules", 5, 1)
        self.assertEqual(results[0]["ran"], True)
        self.assertEqual(results[0]["findings"], [])
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
            env.pop("MACHINIST_OUTPUT_DIR", None)
            with mock.patch.dict(os.environ, env, clear=False):
                verdict = deliver.run_review(str(root), "https://github.com/owner/repo/pull/1")
            self.assertEqual(verdict["status"], "BLOCKED")
            self.assertEqual(verdict["exit_code"], 2)
            self.assertTrue(verdict["failure"])
            argv = json.loads(argv_out.read_text())
            self.assertEqual(argv[0], "--repo")
            self.assertIn("--pr", argv)

    def test_review_verdict_mapping(self) -> None:
        self.assertEqual(deliver.REVIEW_VERDICTS[0], "PASS")
        self.assertEqual(deliver.REVIEW_VERDICTS[2], "BLOCKED")
        self.assertEqual(deliver.REVIEW_VERDICTS[3], "INCOMPLETE")
        self.assertEqual(deliver.REVIEW_VERDICTS[4], "ERROR")


class EndToEndAmendLoopTests(unittest.TestCase):
    """The review sends blocking findings back to amend on the same branch,
    within the profile retry budget, and re-runs the reviewer passes until
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
                  *"pr view"*) printf '{"headRefName":"feature","headRefOid":"head","baseRefName":"main","baseRefOid":"base","body":"body","url":"url","labels":[]}' ;;
                  *"--name-only"*) printf 'src/app.py\\n' ;;
                  *) printf 'diff --git a/src/app.py b/src/app.py\\n+++ b/src/app.py\\n@@ -1 +1,2 @@\\n+print(1)\\n' ;;
                esac
                """
            ),
            encoding="utf-8",
        )
        # Three reviewer passes see blocking findings on the first wave; the
        # executor is also used for the amend and the docs pass.
        (bin_dir / "review-executor").write_text(
            textwrap.dedent(
                """#!/bin/sh
                echo call >> "$EXEC_LOG"
                n=$(wc -l < "$EXEC_LOG")
                if [ "$n" -le 3 ]; then
                  printf 'CRITICAL | src/app.py:1 | blocking finding\\n'
                else
                  printf 'NO FINDINGS\\n'
                fi
                """
            ),
            encoding="utf-8",
        )
        # ruff would be "blocked" with the tool missing; provide a passing
        # stub so the check runs and passes.
        (bin_dir / "ruff").write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        # The built-in secret scan needs trufflehog on PATH; the stub reports
        # no credentials so it neither blocks nor marks the review incomplete.
        (bin_dir / "trufflehog").write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        for name in ("git", "gh", "review-executor", "ruff", "trufflehog"):
            os.chmod(bin_dir / name, 0o755)
        return bin_dir, repo

    def _run(self, root: Path, bin_dir: Path, extra_args: list[str], extra_env: dict[str, str] | None = None) -> tuple[int, dict]:
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
                "EXEC_LOG": str(exec_count),
            }
        )
        if extra_env:
            env.update(extra_env)
        # Resolve the shipped default profile, not a control plane that may be
        # running on the host (which would serve its own embedded copy).
        env.pop("MACHINIST_CONTROL_PLANE_URL", None)
        env.pop("MACHINIST_CONTROL_PLANE_TOKEN", None)
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
        summary["executor_calls"] = len(exec_count.read_text().splitlines()) if exec_count.exists() else 0
        summary["git_log"] = git_log.read_text(encoding="utf-8") if git_log.exists() else ""
        return summary

    def test_blocking_findings_amend_and_rereview(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            bin_dir, _ = self._environment(root)
            summary = self._run(root, bin_dir, ["--amend-rounds", "3"])
            self.assertEqual(summary["exit_code"], 0)
            self.assertEqual(summary["status"], "PASS")
            # 4 reviewer passes + 1 amend + 4 re-review passes + 1 docs pass.
            self.assertEqual(summary["executor_calls"], 10)
            self.assertEqual(summary["amend_rounds"], {"used": 1, "limit": 3})
            self.assertNotIn("push", summary["git_log"])

    def test_exhausted_rounds_report_incomplete(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            bin_dir, _ = self._environment(root)
            summary = self._run(root, bin_dir, ["--amend-rounds", "0"])
            # MACH-61/MACH-63: an exhausted budget is INCOMPLETE with the
            # reason, never PASS and never BLOCKED (it must not fail the
            # delivery job).
            self.assertEqual(summary["exit_code"], 3)
            self.assertEqual(summary["status"], "INCOMPLETE")
            self.assertTrue(summary["blocking"])
            # 4 reviewer passes; the budget stops the review before docs.
            self.assertEqual(summary["executor_calls"], 4)
            self.assertEqual(summary["amend_rounds"], {"used": 0, "limit": 0})
            self.assertEqual(summary["budget"]["exhausted"], "review budget exhausted (rounds)")
            self.assertTrue(any("review budget exhausted (rounds)" in note for note in summary["notes"]))


class ReviewBudgetEndToEndTests(unittest.TestCase):
    """MACH-61 acceptance: a review that keeps finding issues stops at the
    configured rounds, and a slow one stops at the configured minutes, both
    well before the job timeout; the verdict is INCOMPLETE with the reason and
    never PASS."""

    def _always_blocking(self, root: Path) -> tuple[Path, Path]:
        bin_dir, repo = EndToEndAmendLoopTests._environment(self, root)
        executor = bin_dir / "review-executor"
        executor.write_text(
            "#!/bin/sh\n"
            'echo call >> "$EXEC_LOG"\n'
            "printf 'CRITICAL | src/app.py:1 | blocking finding\\n'\n",
            encoding="utf-8",
        )
        os.chmod(executor, 0o755)
        return bin_dir, repo

    def _run(self, root: Path, bin_dir: Path, args: list[str], extra_env: dict[str, str] | None = None) -> dict:
        return EndToEndAmendLoopTests._run(self, root, bin_dir, args, extra_env)

    def test_rounds_exhausted_reports_incomplete(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            bin_dir, _ = self._always_blocking(root)
            summary = self._run(root, bin_dir, ["--amend-rounds", "1"])
        self.assertEqual(summary["exit_code"], 3)
        self.assertEqual(summary["status"], "INCOMPLETE")
        self.assertNotEqual(summary["status"], "PASS")
        self.assertEqual(summary["amend_rounds"], {"used": 1, "limit": 1})
        self.assertEqual(summary["budget"]["exhausted"], "review budget exhausted (rounds)")
        self.assertTrue(any("review budget exhausted (rounds)" in note for note in summary["notes"]))
        # First wave (4) + 1 fix round + re-review (4); no further round, no docs.
        self.assertEqual(summary["executor_calls"], 9)

    def test_minutes_exhausted_reports_incomplete(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            bin_dir, _ = self._always_blocking(root)
            summary = self._run(root, bin_dir, ["--max-minutes", "0"])
        self.assertEqual(summary["exit_code"], 3)
        self.assertEqual(summary["status"], "INCOMPLETE")
        self.assertEqual(summary["amend_rounds"], {"used": 0, "limit": 3})
        self.assertEqual(summary["budget"]["exhausted"], "review budget exhausted (minutes)")
        # Only the reviewer wave ran; no fix round and no docs.
        self.assertEqual(summary["executor_calls"], 4)

    def test_job_time_too_short_does_not_start_a_round(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            bin_dir, _ = self._always_blocking(root)
            env = {
                "MACHINIST_JOB_TIMEOUT_SECONDS": "1",
                "MACHINIST_JOB_STARTED_EPOCH": str(int(time.time())),
            }
            summary = self._run(root, bin_dir, [], extra_env=env)
        self.assertEqual(summary["status"], "INCOMPLETE")
        self.assertEqual(summary["amend_rounds"], {"used": 0, "limit": 3})
        self.assertEqual(summary["budget"]["exhausted"], "review budget exhausted (job time)")
        # The reviewer wave ran once; no fix round and no docs.
        self.assertEqual(summary["executor_calls"], 4)


if __name__ == "__main__":
    unittest.main()
