from __future__ import annotations

import json
import os
from pathlib import Path
import tempfile
import textwrap
import unittest
from unittest import mock

import deploy.factory.post_pr_review as post
from deploy.factory.review import core
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


class LanguageTests(unittest.TestCase):
    def test_language_extras(self) -> None:
        ts_roles = core.review_roles({"prose": set(), "code": {"a.tsx"}}, small_combined=False)
        self.assertIn("typescript-code-review", ts_roles)
        self.assertIn("typescript-jsdoc", ts_roles)
        rust_roles = core.review_roles({"prose": set(), "code": {"lib.rs"}}, small_combined=False)
        self.assertEqual(["rust-review" in rust_roles, any(role.startswith("typescript") for role in rust_roles)], [True, False])

    def test_python_no_extras_and_shell_shebang(self) -> None:
        self.assertEqual(core.review_roles({"prose": set(), "code": {"x.py"}}, small_combined=False), list(core.CODE_REVIEW_ROLES))
        self.assertEqual(core.detect_language("script", "#!/usr/bin/env bash\n"), "shell")


class SizeGateTests(unittest.TestCase):
    def test_small_combined(self) -> None:
        diff = "+line\n-line\n" * 25
        self.assertTrue(core.size_gate(diff, ["a.py", "b.py", "c.py"]))

    def test_sensitive_path_fans_out(self) -> None:
        diff = "+line\n" * 10
        self.assertFalse(core.size_gate(diff, ["src/auth/login.py"]))

    def test_many_files_fan_out(self) -> None:
        diff = "+line\n"
        self.assertFalse(core.size_gate(diff, [f"file{i}.py" for i in range(10)]))


class ClassificationTests(unittest.TestCase):
    def test_new_and_pre_existing(self) -> None:
        check = post.ReviewCheck("python", "ruff", ["ruff", "check", "bad.py"])
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
    def test_critical_first_and_roles_exact(self) -> None:
        reviewers = [
            {
                "role": "feature-dev:code-reviewer",
                "ran": True,
                "findings": [{"severity": "warning", "location": "a.py:1", "message": "later", "unparsed": False}],
            },
            {
                "role": "pr-review-toolkit:silent-failure-hunter",
                "ran": True,
                "findings": [{"severity": "critical", "location": "b.py:1", "message": "critical", "unparsed": False}],
            },
        ]
        body = post.render_comment("BLOCKED", [], reviewers, {"ran": True, "changed": [], "pr_description_updated": False})
        self.assertLess(body.index("## CRITICAL"), body.index("## Checks"))
        self.assertLess(body.index("critical"), body.index("later"))
        self.assertIn("feature-dev:code-reviewer", body)
        self.assertIn("pr-review-toolkit:silent-failure-hunter", body)


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
            os.chmod(bin_dir / "gh", 0o755)
            os.chmod(bin_dir / "git", 0o755)
            os.chmod(bin_dir / "review-executor", 0o755)
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
