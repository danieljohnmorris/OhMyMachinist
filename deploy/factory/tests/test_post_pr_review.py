from __future__ import annotations

import json
import os
from pathlib import Path
import tempfile
import textwrap
import unittest
from unittest import mock

import deploy.factory.post_pr_review as post
from deploy.factory.review import core, profile as profile_mod
from deploy.factory.review.runner import CommandResult


class RoutingTests(unittest.TestCase):
    def test_prose_routing(self) -> None:
        classified = core.classify_changed_files(
            [
                "src/content/blog/a.md",
                "src/content/decisions/b.mdx",
                "src/content/newcollection/c.md",
                "docs/x.md",
                "README.md",
                "src/content/img.png",
            ]
        )
        self.assertEqual(
            classified["prose"],
            {
                "src/content/blog/a.md",
                "src/content/decisions/b.mdx",
                "src/content/newcollection/c.md",
            },
        )
        self.assertEqual(classified["code"], {"docs/x.md", "README.md", "src/content/img.png"})

    def test_mixed_and_empty_routing(self) -> None:
        self.assertEqual(
            core.classify_changed_files(["src/content/blog/a.md", "src/app.ts"]),
            {"prose": {"src/content/blog/a.md"}, "code": {"src/app.ts"}},
        )
        self.assertEqual(core.classify_changed_files([]), {"prose": set(), "code": set()})


class ClassificationTests(unittest.TestCase):
    def test_new_and_pre_existing(self) -> None:
        check = profile_mod.ReviewCheck("python", "ruff", ["ruff", "check", "bad.py"])
        with mock.patch.object(post, "run_git", return_value=CommandResult(("git",), 0, "", "")) as git_mock, mock.patch.object(
            post, "run_command", return_value=CommandResult(("ruff",), 1, "", "error")
        ) as command_mock:
            self.assertEqual(post.classify_failure(Path("."), check, "abc123", 1), "pre-existing, not blocking")
            self.assertEqual(git_mock.call_count, 2)
            self.assertEqual(command_mock.call_count, 1)
        with mock.patch.object(post, "run_git", return_value=CommandResult(("git",), 0, "", "")), mock.patch.object(
            post, "run_command", return_value=CommandResult(("ruff",), 0, "", "")
        ):
            self.assertEqual(post.classify_failure(Path("."), check, "abc123", 1), "NEW")


class ParserTests(unittest.TestCase):
    def test_valid_and_no_findings(self) -> None:
        findings = core.parse_reviewer_output("bug | a.py:2 | bad\nNO FINDINGS".strip().replace("NO FINDINGS", "nit | a.py:3 | style"))
        self.assertEqual([(finding.severity, finding.location, finding.message) for finding in findings], [("bug", "a.py:2", "bad"), ("nit", "a.py:3", "style")])
        self.assertEqual(core.parse_reviewer_output("NO FINDINGS"), [])

    def test_garbage_is_kept(self) -> None:
        findings = core.parse_reviewer_output("I think this is bad")
        self.assertEqual(len(findings), 1)
        self.assertTrue(findings[0].unparsed)


class CommentTests(unittest.TestCase):
    def test_critical_first_and_reviewer_names_exact(self) -> None:
        resolved = _resolved_profile()
        reviewers = [
            {
                "name": "code-reviewer",
                "skills": ["feature-dev:code-reviewer"],
                "ran": True,
                "skipped": False,
                "findings": [{"severity": "warning", "location": "a.py:1", "message": "later", "unparsed": False}],
            },
            {
                "name": "silent-failure-hunter",
                "skills": ["pr-review-toolkit:silent-failure-hunter"],
                "ran": True,
                "skipped": False,
                "findings": [{"severity": "critical", "location": "b.py:1", "message": "critical", "unparsed": False}],
            },
        ]
        merged = profile_mod.reconcile_findings(reviewers, resolved.severities(), ["a.py", "b.py"])
        body = post.render_comment("BLOCKED", [], reviewers, {"ran": True, "changed": [], "pr_description_updated": False}, resolved, merged, "headsha", [])
        self.assertLess(body.index("## CRITICAL"), body.index("## Checks"))
        self.assertLess(body.index("critical"), body.index("later"))
        self.assertIn("code-reviewer", body)
        self.assertIn("silent-failure-hunter", body)
        self.assertIn("Head SHA: headsha", body)


def _resolved_profile() -> profile_mod.ResolvedProfile:
    body = profile_mod.parse_profile(
        "name: comment-test\nversion: 1\n"
        "reviewers:\n  - name: code-reviewer\n    skills: [\"feature-dev:code-reviewer\"]\n"
        "    read_only: true\n"
        "  - name: silent-failure-hunter\n    skills: [\"pr-review-toolkit:silent-failure-hunter\"]\n"
        "    read_only: true\n"
        "blocking:\n  severities: [critical, bug]\n  amend_rounds: 3\n",
        "comment-test",
    )
    return profile_mod.ResolvedProfile(body=body, raw="", source="test")


class ExitCodeTests(unittest.TestCase):
    def test_statuses_and_exit_codes(self) -> None:
        self.assertEqual(post.final_status(True, False), "BLOCKED")
        self.assertEqual(post.EXIT_CODES[post.final_status(True, False)], 2)
        self.assertEqual(post.STATUS_STATES[post.final_status(True, False)], "failure")
        self.assertEqual(post.final_status(False, False), "PASS")
        self.assertEqual(post.EXIT_CODES[post.final_status(False, False)], 0)
        self.assertEqual(post.STATUS_STATES[post.final_status(False, False)], "success")
        self.assertEqual(post.final_status(False, True), "INCOMPLETE")
        # MACH-31: INCOMPLETE exits 0 (never fails the delivery); the status
        # state still records "error" on machinist/review.
        self.assertEqual(post.EXIT_CODES[post.final_status(False, True)], 0)
        self.assertEqual(post.STATUS_STATES[post.final_status(False, True)], "error")


class EndToEndDryRunTests(unittest.TestCase):
    def test_dry_run_uses_stubs_and_writes_json(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
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
            (bin_dir / "review-executor").write_text("#!/bin/sh\ncat >/dev/null\nprintf 'NO FINDINGS\\n'\n", encoding="utf-8")
            (bin_dir / "ruff").write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            os.chmod(bin_dir / "gh", 0o755)
            os.chmod(bin_dir / "git", 0o755)
            os.chmod(bin_dir / "review-executor", 0o755)
            os.chmod(bin_dir / "ruff", 0o755)
            gh_log = root / "gh.log"
            git_log = root / "git.log"
            json_out = root / "summary.json"
            env = os.environ.copy()
            env.update({"PATH": f"{bin_dir}:{env['PATH']}", "TEMP_GH_LOG": str(gh_log), "TEMP_GIT_LOG": str(git_log)})
            with mock.patch.dict(os.environ, env, clear=True):
                code = post.main(
                    [
                        "--repo",
                        str(repo),
                        "--pr",
                        "owner/repo#1",
                        "--executor-cmd",
                        str(bin_dir / "review-executor"),
                        "--timeout-seconds",
                        "1",
                        "--dry-run",
                        "--json-out",
                        str(json_out),
                    ]
                )
            self.assertEqual(code, 0)
            summary = json.loads(json_out.read_text(encoding="utf-8"))
            self.assertEqual(summary["status"], "PASS")
            self.assertFalse(summary["blocking"])
            self.assertFalse(gh_log.exists() and "pr comment" in gh_log.read_text(encoding="utf-8"))
            git_calls = git_log.read_text(encoding="utf-8") if git_log.exists() else ""
            self.assertNotIn("push", git_calls)
            self.assertNotIn("commit", git_calls)


if __name__ == "__main__":
    unittest.main()
