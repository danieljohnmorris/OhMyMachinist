"""Tests for the deliver-pr secret-file guard (MACH-17)."""
from __future__ import annotations

import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

_SPEC = Path(__file__).resolve().parents[1] / "deliver-pr.py"
_spec = importlib.util.spec_from_file_location("deliver_pr", _SPEC)
deliver = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(deliver)


def staged_diff(*added_lines: str) -> str:
    """Minimal unified diff with the given added lines (hunk @ -0,0 +1)."""
    body = "\n".join(f"+{line}" for line in added_lines)
    return f"diff --git a/.env.example b/.env.example\n+++ b/.env.example\n{body}\n"


class SecretLikeTests(unittest.TestCase):
    def test_env_example_commented_placeholders_pass(self) -> None:
        files = [".env.example"]
        diffs = {".env.example": staged_diff(
            "# SPARKDASH_CLOCK_AUTO_TOKEN=changeme",
            "# SPARKDASH_CLOCK_AUTO_PROJECT_ID=your-project-id",
            "# SPARKDASH_CLOCK_AUTO_ENABLED=true",
            "",
            "APP_ENV=development",
        )}
        self.assertEqual(deliver.secret_like(files, diffs), [])

    def test_env_sample_passes(self) -> None:
        files = [".env.sample"]
        diffs = {".env.sample": staged_diff(
            "# DATABASE_URL=postgresql://user:password@localhost:5432/db",
            "DEBUG=false",
        )}
        self.assertEqual(deliver.secret_like(files, diffs), [])

    def test_env_example_in_subdir_passes(self) -> None:
        files = ["config/.env.example"]
        diffs = {"config/.env.example": staged_diff("# TOKEN=<your-token-here>")}
        self.assertEqual(deliver.secret_like(files, diffs), [])

    def test_env_template_and_dist_and_dot_suffixed_pass(self) -> None:
        files = [".env.template", ".env.dist", ".env.defaults", "example.env", "sample.env",
                 "build.env.example", "build.env.sample"]
        self.assertEqual(deliver.secret_like(files), [])

    def test_case_insensitive_template_match(self) -> None:
        self.assertEqual(deliver.secret_like(["Config/.ENV.Example"]), [])

    def test_dotenv_refused(self) -> None:
        self.assertEqual(deliver.secret_like([".env"]), [".env"])

    def test_dotenv_variants_refused(self) -> None:
        self.assertEqual(deliver.secret_like([".env.local", ".env.production", ".env.development",
                                              ".env.prod", ".env.test"]), [".env.local", ".env.production",
                                                                           ".env.development", ".env.prod", ".env.test"])

    def test_prod_env_refused(self) -> None:
        self.assertEqual(deliver.secret_like(["prod.env"]), ["prod.env"])

    def test_env_example_with_ghp_token_refused(self) -> None:
        files = [".env.example"]
        diffs = {".env.example": staged_diff("GITHUB_TOKEN=ghp_0123456789abcdefghijklmnopqrstuvwxyz")}
        self.assertEqual(deliver.secret_like(files, diffs), [".env.example"])

    def test_env_example_with_sensitive_key_and_real_value_refused(self) -> None:
        diffs = {".env.example": staged_diff("SPARKDASH_API_KEY=live-9f8e7d6c5b4a")}
        self.assertEqual(deliver.secret_like([".env.example"], diffs), [".env.example"])

    def test_env_example_token_shapes_refused(self) -> None:
        for value in ("gho_abcdefghij", "github_pat_11AAAA0_A1b2C3", "sk-proj-abcdef123456",
                      "xoxb-123-456-abc", "AKIAIOSFODNN7EXAMPLE", "-----BEGIN RSA PRIVATE KEY-----"):
            with self.subTest(value=value):
                diffs = {".env.example": staged_diff(f"WHATEVER={value}")}
                self.assertEqual(deliver.secret_like([".env.example"], diffs), [".env.example"])

    def test_env_example_placeholder_values_pass(self) -> None:
        for value in ("", "changeme", "your-secret-here", "<your-token>", "xxx", "${VAULT_TOKEN}"):
            with self.subTest(value=value):
                diffs = {".env.example": staged_diff(f"MY_TOKEN={value}")}
                self.assertEqual(deliver.secret_like([".env.example"], diffs), [])

    def test_env_example_commented_secret_line_passes(self) -> None:
        diffs = {".env.example": staged_diff("# GITHUB_TOKEN=ghp_0123456789abcdefghijklmnopqrstuvwxyz")}
        self.assertEqual(deliver.secret_like([".env.example"], diffs), [])

    def test_id_rsa_and_pem_still_refused(self) -> None:
        self.assertEqual(deliver.secret_like(["id_rsa", "id_ed25519", "server.pem", "keystore.p12"]),
                         ["id_rsa", "id_ed25519", "server.pem", "keystore.p12"])

    def test_other_secret_globs_unchanged(self) -> None:
        self.assertEqual(deliver.secret_like([".npmrc", ".netrc", "credentials.json", "creds-secret.txt"]),
                         [".npmrc", ".netrc", "credentials.json", "creds-secret.txt"])

    def test_template_diff_has_secret_ignores_context_and_headers(self) -> None:
        diff = ("diff --git a/.env.example b/.env.example\n"
                "+++ b/.env.example\n"
                "@@ -1,2 +1,3 @@\n"
                "-GITHUB_TOKEN=ghp_removednotadded\n"
                "+# GITHUB_TOKEN=ghp_commented\n"
                "+GITHUB_TOKEN=\n"
                " context not added\n")
        self.assertFalse(deliver.template_diff_has_secret(diff))

    def test_error_message_shape_unchanged(self) -> None:
        # Mirrors the refusal string built in main(): "refusing to commit: secret-like files [...]; left on branch X"
        bad = deliver.secret_like([".env", ".env.local"])
        msg = f"refusing to commit: {'secret-like files ' + str(bad) if bad else 'too many'}; left on branch some-branch"
        self.assertEqual(msg, "refusing to commit: secret-like files ['.env', '.env.local']; left on branch some-branch")


def _git(repo: Path, *args: str) -> str:
    result = subprocess.run(["git", "-C", str(repo), *args], capture_output=True, text=True)
    if result.returncode != 0:
        raise AssertionError(f"git {args}: {result.stderr or result.stdout}")
    return result.stdout.strip()


class FinalizeAfterReviewTests(unittest.TestCase):
    """MACH-61: whatever the review leaves behind, delivery finishes with a
    clean lane and the leftover fix edits either committed+pushed or stashed
    with a labelled stash and a patch in the run artifacts."""

    def _repo(self, root: Path) -> Path:
        repo = root / "repo"
        repo.mkdir()
        _git(repo, "init", "-q", "-b", "main")
        _git(repo, "config", "user.name", "Test")
        _git(repo, "config", "user.email", "test@example.com")
        (repo / "app.py").write_text("print(1)\n", encoding="utf-8")
        _git(repo, "add", "-A")
        _git(repo, "commit", "-q", "-m", "base")
        return repo

    def test_commits_and_pushes_leftover_edits(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            origin = root / "origin.git"
            subprocess.run(["git", "init", "-q", "--bare", "-b", "main", str(origin)], check=True)
            repo = self._repo(root)
            _git(repo, "remote", "add", "origin", str(origin))
            (repo / "app.py").write_text("print(2)\n", encoding="utf-8")
            deliver.finalize_after_review(str(repo), "main", "", "run1")
            self.assertEqual(_git(repo, "status", "--porcelain"), "")
            self.assertIn("Review fixes", _git(repo, "log", "-1", "--format=%s"))
            self.assertIn("print(2)", _git(origin, "show", "main:app.py"))

    def test_stashes_with_patch_when_push_impossible(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo = self._repo(root)
            out = root / "artifacts"
            (repo / "app.py").write_text("print(3)\n", encoding="utf-8")
            with mock.patch.dict(os.environ, {"MACHINIST_OUTPUT_DIR": str(out)}):
                deliver.finalize_after_review(str(repo), "main", "", "run1")
            self.assertEqual(_git(repo, "status", "--porcelain"), "")
            self.assertIn("machinist-review-budget-run1", _git(repo, "stash", "list"))
            patch = out / "machinist-review-budget-run1.patch"
            self.assertTrue(patch.is_file())
            self.assertIn("print(3)", patch.read_text(encoding="utf-8"))


def _dirty_repo(root: Path) -> Path:
    repo = root / "repo"
    repo.mkdir()
    _git(repo, "init", "--quiet")
    _git(repo, "config", "user.name", "test")
    _git(repo, "config", "user.email", "test@example.com")
    (repo / "README.md").write_text("seed\n", encoding="utf-8")
    _git(repo, "add", "README.md")
    _git(repo, "commit", "-m", "seed", "--quiet")
    (repo / "README.md").write_text("edited by the review\n", encoding="utf-8")
    return repo


class LaneWipTests(unittest.TestCase):
    """MACH-59: deliver-pr switches back to the start branch at the end; a
    review edit left in the lane must be stashed (with a patch) first."""

    def test_save_wip_stashes_a_dirty_lane_and_writes_a_patch(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo = _dirty_repo(root)
            scratch = root / "scratch"
            output = root / "outputs"
            output.mkdir()
            with mock.patch.dict(
                os.environ,
                {"MACHINIST_SCRATCH_DIR": str(scratch), "MACHINIST_OUTPUT_DIR": str(output)},
            ):
                deliver.save_wip(str(repo))
            self.assertEqual(_git(repo, "status", "--porcelain"), "")
            self.assertEqual(len(list(scratch.glob("*.patch"))), 1)
            # The published task folder must stay free of the WIP patch.
            self.assertEqual(list(output.glob("*.patch")), [])

    def test_save_wip_is_a_noop_on_a_clean_lane(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo = _dirty_repo(root)
            _git(repo, "checkout", "--", "README.md")
            with mock.patch.dict(os.environ, {"MACHINIST_SCRATCH_DIR": str(root / "scratch")}):
                deliver.save_wip(str(repo))
            self.assertEqual(_git(repo, "stash", "list"), "")

    def test_save_wip_reports_a_failed_status(self) -> None:
        """A failed `git status` is not a clean lane: warn, stash nothing."""
        with tempfile.TemporaryDirectory() as temp:
            repo = _dirty_repo(Path(temp))
            failure = subprocess.CompletedProcess(["git"], 128, "", "fatal: not a git repository")
            with mock.patch.dict(os.environ, {"MACHINIST_SCRATCH_DIR": str(Path(temp) / "scratch")}), \
                 mock.patch.object(deliver.subprocess, "run", return_value=failure) as run_mock:
                deliver.save_wip(str(repo))
            self.assertEqual(run_mock.call_count, 1)
            self.assertEqual(_git(repo, "stash", "list"), "")

    def test_save_wip_does_not_read_a_stale_stash_after_a_failed_push(self) -> None:
        """A failed stash push must not label and publish a pre-existing stash."""
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo = _dirty_repo(root)
            _git(repo, "checkout", "--", "README.md")
            (repo / "seed-2.txt").write_text("seed\n", encoding="utf-8")
            _git(repo, "add", "seed-2.txt")
            _git(repo, "commit", "-m", "seed 2", "--quiet")
            (repo / "seed-2.txt").write_text("earlier stash\n", encoding="utf-8")
            _git(repo, "stash", "push", "-m", "machinist-earlier")
            (repo / "README.md").write_text("dirty again\n", encoding="utf-8")
            scratch = root / "scratch"
            real_run = subprocess.run

            def fake_run(args, **kwargs):
                if "stash" in args and "push" in args:
                    return subprocess.CompletedProcess(args, 1, "", "fatal: stash failed")
                return real_run(args, **kwargs)

            with mock.patch.dict(os.environ, {"MACHINIST_SCRATCH_DIR": str(scratch)}), \
                 mock.patch.object(deliver.subprocess, "run", side_effect=fake_run):
                deliver.save_wip(str(repo))
            self.assertEqual(list(scratch.glob("*.patch")) if scratch.exists() else [], [])
            self.assertIn("machinist-earlier", _git(repo, "stash", "list"))


class ReviewVerdictEndToEndTests(unittest.TestCase):
    """MACH-63: every verdict flows through deliver-pr.py end to end (with a
    stubbed review script). Only an explicit PASS is a pass; BLOCKED fails the
    delivery; INCOMPLETE and ERROR do not, but they are never recorded as PASS.
    Each verdict is written to pr.json and posted as a matching machinist/review
    status with the reason in the description."""

    _GIT = """#!/bin/sh
case "$*" in
  *"remote get-url"*) printf 'https://github.com/owner/repo.git\\n' ;;
  *"symbolic-ref"*) printf 'origin/main\\n' ;;
  *"diff --cached --name-only"*) printf 'src/app.py\\n' ;;
  *"diff --cached -U0"*) printf '' ;;
  *"rev-parse --abbrev-ref"*) printf 'feature\\n' ;;
  *"rev-parse --short"*) printf 'abc1234\\n' ;;
  *"rev-parse HEAD"*) printf 'deadbeef\\n' ;;
  *"status --porcelain"*) printf ' M src/app.py\\n' ;;
  *"prBase"*) printf '' ;;
  *) : ;;
esac
exit 0
"""

    _GH = """#!/bin/sh
case "$*" in
  *"pr list"*) exit 0 ;;
  *"pr view"*)
    case "$*" in
      *"headRefOid"*) printf 'headsha123\\n' ;;
      *) printf 'OPEN\\n' ;;
    esac
    exit 0 ;;
  *"pr create"*) printf 'https://github.com/owner/repo/pull/7\\n'; exit 0 ;;
  *"api"*) printf '%s\\n' "$*" >> "$STATUS_LOG"; exit 0 ;;
esac
exit 0
"""

    def _environment(self, root: Path) -> tuple[Path, Path, Path]:
        repo = root / "repo"
        (repo / "src").mkdir(parents=True)
        (repo / "src" / "app.py").write_text("print(1)\n", encoding="utf-8")
        (repo / ".machinist-pr.md").write_text("# Add a thing\n\nLead sentence.\n", encoding="utf-8")
        brief = root / "brief.md"
        brief.write_text("--- SPEC: MACH-63 A title ---\nbody\n", encoding="utf-8")
        bin_dir = root / "bin"
        bin_dir.mkdir()
        (bin_dir / "git").write_text(self._GIT, encoding="utf-8")
        (bin_dir / "gh").write_text(self._GH, encoding="utf-8")
        os.chmod(bin_dir / "git", 0o755)
        os.chmod(bin_dir / "gh", 0o755)
        return repo, bin_dir, brief

    def _run_verdict(self, root: Path, status: str, reason: str, exit_code: int, write_json: bool = True) -> dict:
        repo, bin_dir, brief = self._environment(root)
        out = root / "out"
        out.mkdir()
        status_log = root / "status.log"
        review_json = out / "post-pr-review.json"
        script = root / "review.py"
        if write_json:
            script.write_text(
                "import json, os, sys\n"
                f"json.dump({{'status': {status!r}, 'reason': {reason!r}}}, open(os.environ['REVIEW_JSON'], 'w'))\n"
                f"sys.exit({exit_code})\n",
                encoding="utf-8",
            )
        else:
            script.write_text(f"import sys\nsys.exit({exit_code})\n", encoding="utf-8")
        env = os.environ.copy()
        env.update(
            {
                "PATH": f"{bin_dir}:{env['PATH']}",
                "MACHINIST_ALLOWED_OWNERS": "owner",
                "MACHINIST_REVIEW_SCRIPT": str(script),
                "MACHINIST_OUTPUT_DIR": str(out),
                "MACHINIST_SCRATCH_DIR": str(root / "scratch"),
                "MACHINIST_RUN_ID": "run123",
                "REVIEW_JSON": str(review_json),
                "STATUS_LOG": str(status_log),
            }
        )
        env.pop("MACHINIST_REVIEW_EXECUTOR", None)
        env.pop("DELIVER_PR_DRY", None)
        argv = ["deliver-pr.py", "--repo", str(repo), "--brief", str(brief)]
        with mock.patch.dict(os.environ, env, clear=True), mock.patch.object(sys, "argv", argv), \
             mock.patch.object(deliver, "ALLOWED_OWNERS", {"owner"}):
            code = deliver.main()
        pr_json = json.loads((out / "pr.json").read_text(encoding="utf-8"))
        pr_json["exit_code"] = code
        pr_json["status_calls"] = status_log.read_text(encoding="utf-8") if status_log.exists() else ""
        return pr_json

    def test_pass_is_the_only_pass(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            result = self._run_verdict(Path(temp), "PASS", "", 0)
        self.assertEqual(result["exit_code"], 0)
        self.assertEqual(result["review_status"], "PASS")
        self.assertIn("state=success", result["status_calls"])
        self.assertIn("context=machinist/review", result["status_calls"])

    def test_blocked_fails_the_delivery(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            result = self._run_verdict(Path(temp), "BLOCKED", "blocking findings", 2)
        self.assertEqual(result["exit_code"], 2)
        self.assertEqual(result["review_status"], "BLOCKED")
        self.assertEqual(result["review_reason"], "blocking findings")
        self.assertIn("state=failure", result["status_calls"])
        self.assertIn("blocking findings", result["status_calls"])

    def test_incomplete_does_not_fail_the_delivery(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            result = self._run_verdict(Path(temp), "INCOMPLETE", "review budget exhausted (rounds)", 3)
        self.assertEqual(result["exit_code"], 0)
        self.assertEqual(result["review_status"], "INCOMPLETE")
        self.assertNotEqual(result["review_status"], "PASS")
        self.assertIn("state=error", result["status_calls"])
        self.assertIn("review budget exhausted", result["status_calls"])

    def test_error_does_not_fail_the_delivery(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            result = self._run_verdict(Path(temp), "ERROR", "cannot resolve the review base commit", 4)
        self.assertEqual(result["exit_code"], 0)
        self.assertEqual(result["review_status"], "ERROR")
        self.assertIn("state=error", result["status_calls"])

    def test_missing_json_file_is_never_pass(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            # The review exited 0 but wrote no verdict: not a PASS.
            result = self._run_verdict(Path(temp), "", "", 0, write_json=False)
        self.assertEqual(result["exit_code"], 0)
        self.assertEqual(result["review_status"], "ERROR")
        self.assertIn("no verdict", result["review_reason"])
        self.assertIn("state=error", result["status_calls"])

    def test_unknown_exit_code_is_never_pass(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            result = self._run_verdict(Path(temp), "", "", 7, write_json=False)
        self.assertEqual(result["exit_code"], 0)
        self.assertEqual(result["review_status"], "ERROR")
        self.assertIn("state=error", result["status_calls"])

    def test_unreadable_json_is_never_pass(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            repo, bin_dir, brief = self._environment(Path(temp))
            root = Path(temp)
            out = root / "out"
            out.mkdir()
            (out / "post-pr-review.json").write_text("{not json", encoding="utf-8")
            script = root / "review.py"
            script.write_text("import sys\nsys.exit(0)\n", encoding="utf-8")
            env = os.environ.copy()
            env.update({
                "PATH": f"{bin_dir}:{env['PATH']}", "MACHINIST_ALLOWED_OWNERS": "owner",
                "MACHINIST_REVIEW_SCRIPT": str(script), "MACHINIST_OUTPUT_DIR": str(out),
                "MACHINIST_SCRATCH_DIR": str(root / "scratch"), "MACHINIST_RUN_ID": "run123",
                "STATUS_LOG": str(root / "status.log"),
            })
            env.pop("MACHINIST_REVIEW_EXECUTOR", None)
            with mock.patch.dict(os.environ, env, clear=True), \
                 mock.patch.object(sys, "argv", ["deliver-pr.py", "--repo", str(repo), "--brief", str(brief)]), \
                 mock.patch.object(deliver, "ALLOWED_OWNERS", {"owner"}):
                code = deliver.main()
            result = json.loads((out / "pr.json").read_text(encoding="utf-8"))
        self.assertEqual(code, 0)
        self.assertEqual(result["review_status"], "ERROR")
        self.assertNotEqual(result["review_status"], "PASS")


class ReviewVerdictResolutionTests(unittest.TestCase):
    """MACH-63: the verdict is read from the JSON first, the exit code second."""

    def test_json_verdict_wins_over_exit_code(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "post-pr-review.json"
            path.write_text(json.dumps({"status": "BLOCKED", "reason": "a bug"}), encoding="utf-8")
            verdict = deliver.review_verdict(str(path), 0)
        self.assertEqual(verdict["status"], "BLOCKED")
        self.assertEqual(verdict["reason"], "a bug")
        self.assertFalse(verdict["passed"])
        self.assertTrue(verdict["failure"])

    def test_exit_code_used_when_no_json_requested(self) -> None:
        self.assertEqual(deliver.review_verdict(None, 0)["status"], "PASS")
        self.assertEqual(deliver.review_verdict(None, 3)["status"], "INCOMPLETE")
        self.assertEqual(deliver.review_verdict(None, 4)["status"], "ERROR")
        self.assertEqual(deliver.review_verdict(None, 9)["status"], "ERROR")

    def test_requested_json_that_is_missing_is_not_pass(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            missing = str(Path(temp) / "post-pr-review.json")
            verdict = deliver.review_verdict(missing, 0)
        self.assertEqual(verdict["status"], "ERROR")
        self.assertFalse(verdict["passed"])
        self.assertIn("no verdict", verdict["reason"])


if __name__ == "__main__":
    unittest.main()
