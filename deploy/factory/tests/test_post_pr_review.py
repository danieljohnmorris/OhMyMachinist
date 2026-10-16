from __future__ import annotations

import contextlib
import io
import json
import os
from pathlib import Path
import tempfile
import textwrap
import unittest
from unittest import mock

import deploy.factory.post_pr_review as post
from deploy.factory.review import core, profile as profile_mod, skill_library
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
            (bin_dir / "trufflehog").write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            os.chmod(bin_dir / "gh", 0o755)
            os.chmod(bin_dir / "git", 0o755)
            os.chmod(bin_dir / "review-executor", 0o755)
            os.chmod(bin_dir / "ruff", 0o755)
            os.chmod(bin_dir / "trufflehog", 0o755)
            gh_log = root / "gh.log"
            git_log = root / "git.log"
            json_out = root / "summary.json"
            env = os.environ.copy()
            env.update({"PATH": f"{bin_dir}:{env['PATH']}", "TEMP_GH_LOG": str(gh_log), "TEMP_GIT_LOG": str(git_log)})
            env.pop("MACHINIST_CONTROL_PLANE_URL", None)
            env.pop("MACHINIST_CONTROL_PLANE_TOKEN", None)
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


class RequiredSkillTests(unittest.TestCase):
    """MACH-50: a required skill that is not installed must publish an error
    status and comment instead of exiting silently behind a stale green
    machinist/review check."""

    def test_required_skill_failure_publishes_error_status(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo = root / "repo"
            repo.mkdir()
            bin_dir = root / "bin"
            bin_dir.mkdir()
            (bin_dir / "git").write_text(
                "#!/bin/sh\n"
                'case "$*" in\n'
                '  *"remote get-url"*) printf \'https://github.com/owner/repo.git\\n\' ;;\n'
                "  *) : ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            (bin_dir / "gh").write_text(
                "#!/bin/sh\n"
                'printf \'%s\\n\' "$*" >> "$TEMP_GH_LOG"\n'
                'case "$*" in\n'
                '  *"pr view"*) printf \'{"headRefName":"feature","headRefOid":"head","baseRefName":"main","baseRefOid":"base","body":"body","url":"url"}\\n\' ;;\n'
                '  *"pr comment"*) printf \'https://github.com/owner/repo/pull/1#issuecomment-1\\n\' ;;\n'
                "  *) : ;;\n"
                "esac\n",
                encoding="utf-8",
            )
            os.chmod(bin_dir / "git", 0o755)
            os.chmod(bin_dir / "gh", 0o755)
            gh_log = root / "gh.log"
            json_out = root / "summary.json"
            env = os.environ.copy()
            env.update({"PATH": f"{bin_dir}:{env['PATH']}", "TEMP_GH_LOG": str(gh_log)})
            env.pop("MACHINIST_CONTROL_PLANE_URL", None)
            env.pop("MACHINIST_CONTROL_PLANE_TOKEN", None)
            error = skill_library.RequiredSkillError('required skill "missing" is not installed')
            with mock.patch.dict(os.environ, env, clear=True), mock.patch.object(
                post.skill_library, "loaded_skills", side_effect=error
            ):
                code = post.main(
                    [
                        "--repo",
                        str(repo),
                        "--pr",
                        "owner/repo#1",
                        "--timeout-seconds",
                        "5",
                        "--json-out",
                        str(json_out),
                    ]
                )
            summary = json.loads(json_out.read_text(encoding="utf-8"))
            gh_calls = gh_log.read_text(encoding="utf-8") if gh_log.exists() else ""
        self.assertEqual(code, 0)
        self.assertEqual(summary["status"], "INCOMPLETE")
        self.assertEqual(summary["error"], str(error))
        self.assertIn("pr comment", gh_calls)
        self.assertIn("statuses/head", gh_calls)
        self.assertIn("state=error", gh_calls)
        self.assertIn("context=machinist/review", gh_calls)


_FIXTURE = Path(__file__).parent / "fixtures" / "trufflehog.jsonl"


def _fake_trufflehog(root: Path) -> Path:
    """A trufflehog stub that emits the verified fixture line for the blocking
    scan and the unverified line for the reporting scan."""
    bin_dir = root / "bin"
    bin_dir.mkdir()
    script = bin_dir / "trufflehog"
    script.write_text(
        "#!/bin/sh\n"
        'case "$*" in\n'
        '  *"--results=verified"*) sed -n \'1p\' "$TH_FIXTURE" ;;\n'
        '  *"--results=unverified,unknown"*) sed -n \'2p\' "$TH_FIXTURE" ;;\n'
        "esac\n"
        "exit 0\n",
        encoding="utf-8",
    )
    os.chmod(script, 0o755)
    return bin_dir


def _payloads() -> list[dict]:
    return core.parse_trufflehog_output(_FIXTURE.read_text(encoding="utf-8"))


class SecretScanTests(unittest.TestCase):
    """MACH-41: the built-in review runs trufflehog over the PR range. Verified
    credentials block; unverified findings are reported but never block, and
    the raw secret is never published."""

    def test_scan_splits_verified_from_unverified(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            bin_dir = _fake_trufflehog(root)
            env = os.environ.copy()
            env.update({"PATH": f"{bin_dir}:{env['PATH']}", "TH_FIXTURE": str(_FIXTURE)})
            with mock.patch.dict(os.environ, env, clear=True):
                scan = post.run_secret_scan(root, "base", "feature", 5)
        self.assertTrue(scan["ran"])
        self.assertIsNone(scan["error"])
        self.assertEqual([finding["location"] for finding in scan["verified"]], ["src/config.py:12"])
        self.assertEqual([finding["location"] for finding in scan["unverified"]], ["docs/example.md:3"])
        self.assertTrue(scan["verified"][0]["blocking"])
        self.assertFalse(scan["unverified"][0]["blocking"])

    def test_failed_reporting_scan_reports_an_error(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            script = bin_dir / "trufflehog"
            script.write_text(
                "#!/bin/sh\n"
                'case "$*" in\n'
                '  *"--results=verified"*) sed -n \'1p\' "$TH_FIXTURE" ;;\n'
                '  *"--results=unverified,unknown"*) exit 3 ;;\n'
                "esac\n"
                "exit 0\n",
                encoding="utf-8",
            )
            os.chmod(script, 0o755)
            env = os.environ.copy()
            env.update({"PATH": f"{bin_dir}:{env['PATH']}", "TH_FIXTURE": str(_FIXTURE)})
            with mock.patch.dict(os.environ, env, clear=True):
                scan = post.run_secret_scan(root, "base", "feature", 5)
        self.assertFalse(scan["ran"])
        self.assertTrue(scan["error"])
        # The verified pass still reported its blocking credential.
        self.assertEqual([finding["location"] for finding in scan["verified"]], ["src/config.py:12"])
        body = post.render_comment(
            "BLOCKED", [], [], {"ran": True, "changed": []}, _resolved_profile(), [], "headsha", [], scan
        )
        self.assertIn("1 verified credential(s) — blocking", body)
        self.assertIn(f"- not run: {scan['error']}", body)

    def test_unparseable_reporting_scan_reports_an_error(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            script = bin_dir / "trufflehog"
            script.write_text(
                "#!/bin/sh\n"
                'case "$*" in\n'
                '  *"--results=verified"*) sed -n \'1p\' "$TH_FIXTURE" ;;\n'
                '  *"--results=unverified,unknown"*) printf \'{"SourceID":1}\\n\' ;;\n'
                "esac\n"
                "exit 0\n",
                encoding="utf-8",
            )
            os.chmod(script, 0o755)
            env = os.environ.copy()
            env.update({"PATH": f"{bin_dir}:{env['PATH']}", "TH_FIXTURE": str(_FIXTURE)})
            with mock.patch.dict(os.environ, env, clear=True):
                scan = post.run_secret_scan(root, "base", "feature", 5)
        self.assertFalse(scan["ran"])
        self.assertIn("result missing detector metadata", scan["error"])
        self.assertEqual([finding["location"] for finding in scan["verified"]], ["src/config.py:12"])

    def test_missing_binary_reports_error(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            env = os.environ.copy()
            env["PATH"] = temp
            with mock.patch.dict(os.environ, env, clear=True):
                scan = post.run_secret_scan(Path(temp), "base", "feature", 5)
        self.assertFalse(scan["ran"])
        self.assertIn("not installed", scan["error"])

    def test_comment_lists_findings_and_never_the_raw_secret(self) -> None:
        verified, unverified = _payloads()
        scan = {
            "ran": True,
            "verified": [core.trufflehog_finding(verified, True)],
            "unverified": [core.trufflehog_finding(unverified, False)],
            "error": None,
        }
        body = post.render_comment(
            "BLOCKED", [], [], {"ran": True, "changed": []}, _resolved_profile(), [], "headsha", [], scan
        )
        self.assertIn("## Secret scan (TruffleHog)", body)
        self.assertIn("AKIA****", body)
        self.assertIn("1 unverified finding(s) — not blocking", body)
        self.assertNotIn("cGxhY2Vob2xkZXI=", body)
        self.assertNotIn("placeholder", body)

    def test_comment_reports_a_skipped_scan(self) -> None:
        scan = {"ran": False, "verified": [], "unverified": [], "error": "trufflehog not installed; secret scan skipped"}
        body = post.render_comment("INCOMPLETE", [], [], {"ran": True, "changed": []}, _resolved_profile(), [], "headsha", [], scan)
        self.assertIn("- not run: trufflehog not installed", body)


class SecretScanEndToEndTests(unittest.TestCase):
    """MACH-41 acceptance: a verified credential in the PR range makes the
    review BLOCKED (status failure, exit 2); unverified findings are reported
    but the review still PASSes."""

    def _repo(self, root: Path) -> Path:
        repo = root / "repo"
        repo.mkdir()
        return repo

    def _bins(self, root: Path, trufflehog_body: str | None) -> Path:
        bin_dir = root / "bin"
        bin_dir.mkdir()
        (bin_dir / "git").write_text(
            "#!/bin/sh\n"
            'case "$*" in\n'
            '  *"remote get-url"*) printf \'https://github.com/owner/repo.git\\n\' ;;\n'
            '  *"rev-parse --abbrev-ref"*) printf \'feature\\n\' ;;\n'
            "  *) : ;;\n"
            "esac\n",
            encoding="utf-8",
        )
        (bin_dir / "gh").write_text(
            "#!/bin/sh\n"
            'case "$*" in\n'
            '  *"pr view"*) printf \'{"headRefName":"feature","headRefOid":"head","baseRefName":"main","baseRefOid":"base","body":"body","url":"url"}\\n\' ;;\n'
            '  *"--name-only"*) printf \'src/app.py\\n\' ;;\n'
            '  *) printf \'diff --git a/src/app.py b/src/app.py\\n\' ;;\n'
            "esac\n",
            encoding="utf-8",
        )
        (bin_dir / "review-executor").write_text("#!/bin/sh\ncat >/dev/null\nprintf 'NO FINDINGS\\n'\n", encoding="utf-8")
        (bin_dir / "ruff").write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
        os.chmod(bin_dir / "git", 0o755)
        os.chmod(bin_dir / "gh", 0o755)
        os.chmod(bin_dir / "review-executor", 0o755)
        os.chmod(bin_dir / "ruff", 0o755)
        if trufflehog_body is not None:
            (bin_dir / "trufflehog").write_text(trufflehog_body, encoding="utf-8")
            os.chmod(bin_dir / "trufflehog", 0o755)
        return bin_dir

    def _run(self, root: Path, bin_dir: Path) -> tuple[int, dict, str]:
        json_out = root / "summary.json"
        env = os.environ.copy()
        env.update({"PATH": f"{bin_dir}:{env['PATH']}", "TH_FIXTURE": str(_FIXTURE)})
        env.pop("MACHINIST_CONTROL_PLANE_URL", None)
        env.pop("MACHINIST_CONTROL_PLANE_TOKEN", None)
        buffer = io.StringIO()
        with mock.patch.dict(os.environ, env, clear=True), contextlib.redirect_stdout(buffer):
            code = post.main(
                [
                    "--repo", str(root / "repo"),
                    "--pr", "owner/repo#1",
                    "--executor-cmd", str(bin_dir / "review-executor"),
                    "--timeout-seconds", "5",
                    "--dry-run",
                    "--json-out", str(json_out),
                ]
            )
        return code, json.loads(json_out.read_text(encoding="utf-8")), buffer.getvalue()

    def test_verified_credential_blocks_the_review(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo = self._repo(root)
            bin_dir = self._bins(
                root,
                "#!/bin/sh\n"
                'case "$*" in\n'
                '  *"--results=verified"*) sed -n \'1p\' "$TH_FIXTURE" ;;\n'
                "esac\n"
                "exit 0\n",
            )
            code, summary, output = self._run(root, bin_dir)
        self.assertEqual(code, 2)
        self.assertEqual(summary["status"], "BLOCKED")
        self.assertTrue(summary["blocking"])
        self.assertEqual(len(summary["secret_scan"]["verified"]), 1)
        self.assertIn("## Secret scan (TruffleHog)", output)
        self.assertNotIn("cGxhY2Vob2xkZXI=", output)

    def test_unverified_finding_does_not_block(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            self._repo(root)
            bin_dir = self._bins(
                root,
                "#!/bin/sh\n"
                'case "$*" in\n'
                '  *"--results=unverified,unknown"*) sed -n \'2p\' "$TH_FIXTURE" ;;\n'
                "esac\n"
                "exit 0\n",
            )
            code, summary, output = self._run(root, bin_dir)
        self.assertEqual(code, 0)
        self.assertEqual(summary["status"], "PASS")
        self.assertFalse(summary["blocking"])
        self.assertEqual(len(summary["secret_scan"]["unverified"]), 1)
        self.assertIn("not blocking", output)

    def test_missing_trufflehog_is_incomplete(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            self._repo(root)
            bin_dir = self._bins(root, None)
            code, summary, _ = self._run(root, bin_dir)
        self.assertEqual(code, 0)
        self.assertEqual(summary["status"], "INCOMPLETE")
        self.assertFalse(summary["blocking"])
        self.assertIn("not installed", summary["secret_scan"]["error"])


if __name__ == "__main__":
    unittest.main()
