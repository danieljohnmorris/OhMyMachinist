"""Tests for the deliver-pr secret-file guard (MACH-17)."""
from __future__ import annotations

import importlib.util
import json
import os
from pathlib import Path
import subprocess
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


_JOB_BRANCH = "factory/MACH-71-test-thing-123456"
_PR_URL = "https://github.com/test/repo/pull/85"
_REAL_RUN = deliver.run
_REAL_SUBPROCESS = subprocess.run


def _stub_gh(open_pulls: str, head_lookup: str):
    """Stub gh in deliver-pr: the MACH-71 job lookup (`pr list --json
    headRefName,url`) returns open_pulls; other `pr list` calls return
    head_lookup; `pr create` is recorded, never run. Git runs for real."""
    created: list[list[str]] = []

    def canned(cmd):
        """Canned gh responses, or None to run for real."""
        cmd = list(cmd)
        if cmd[:1] != ["gh"]:
            return None
        if "create" in cmd:
            created.append(cmd)
            return subprocess.CompletedProcess(cmd, 0, stdout=_PR_URL + "\n", stderr="")
        if any("headRefName" in str(argument) for argument in cmd):
            return subprocess.CompletedProcess(cmd, 0, stdout=open_pulls, stderr="")
        return subprocess.CompletedProcess(cmd, 0, stdout=head_lookup, stderr="")

    def fake_run(*cmd, **kwargs):
        cmd = list(cmd)
        response = canned(cmd)
        if response is not None:
            return response.stdout.strip()
        if cmd[:2] == ["git", "ls-remote"] and "--heads" in cmd:
            # Simulate the branch appearing between the lookup and the push.
            return ""
        return _REAL_RUN(*cmd, **kwargs)

    def fake_subprocess(cmd, *args, **kwargs):
        response = canned(cmd)
        if response is not None:
            return response
        return _REAL_SUBPROCESS(cmd, *args, **kwargs)

    patches = [
        mock.patch.object(deliver, "run", fake_run),
        mock.patch.object(deliver.subprocess, "run", fake_subprocess),
        mock.patch.object(deliver, "gh_repo", lambda url: ("test", "repo")),
        mock.patch.object(deliver, "ALLOWED_OWNERS", {"test"}),
    ]
    return patches, created


class DeliveryReuseTests(unittest.TestCase):
    """MACH-71: redelivery and retries reuse the job's existing branch or PR
    and never fail on a non-fast-forward push to the job's own branch."""

    def _setup(self, root: Path):
        origin = root / "origin.git"
        subprocess.run(["git", "init", "-q", "--bare", "-b", "main", str(origin)], check=True)
        repo = root / "repo"
        repo.mkdir()
        _git(repo, "init", "-q", "-b", "main")
        _git(repo, "config", "user.name", "Test")
        _git(repo, "config", "user.email", "test@example.com")
        (repo / "app.py").write_text("base\n", encoding="utf-8")
        _git(repo, "add", "-A")
        _git(repo, "commit", "-q", "-m", "base")
        _git(repo, "remote", "add", "origin", str(origin))
        _git(repo, "push", "-q", "origin", "main")
        brief = root / "brief.txt"
        brief.write_text("--- SPEC: MACH-71 Test thing ---\n--- END SPEC ---\n", encoding="utf-8")
        return repo, str(brief)

    def _first_attempt(self, root: Path, origin: Path):
        """Another lane already cut the job's branch (no fast-forward base)."""
        lane = root / "lane"
        subprocess.run(["git", "clone", "-q", str(origin), str(lane)], check=True)
        _git(lane, "config", "user.name", "Test")
        _git(lane, "config", "user.email", "test@example.com")
        _git(lane, "switch", "-q", "-c", _JOB_BRANCH)
        (lane / "other.py").write_text("first attempt\n", encoding="utf-8")
        _git(lane, "add", "-A")
        _git(lane, "commit", "-q", "-m", "first attempt")
        _git(lane, "push", "-q", "origin", _JOB_BRANCH)

    def _argv(self, repo: Path, brief: str, extra=()):
        return mock.patch("sys.argv", ["deliver-pr.py", "--repo", str(repo), "--brief", brief,
                                       "--start-branch", "main", *extra])

    def _env(self, root: Path):
        return mock.patch.dict(os.environ, {
            "MACHINIST_RUN_ID": "run_000000123456",
            "MACHINIST_SCRATCH_DIR": str(root / "scratch"),
            "MACHINIST_REVIEW_SCRIPT": str(root / "no-review-here.py"),
        })

    def test_retry_reuses_existing_branch_and_pr(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo, brief = self._setup(root)
            self._first_attempt(root, repo / ".." / "origin.git")
            out = root / "outputs"
            out.mkdir()
            patches, created = _stub_gh(json.dumps([{"headRefName": _JOB_BRANCH, "url": _PR_URL}]), _PR_URL)
            with self._env(root), mock.patch.dict(os.environ, {"MACHINIST_OUTPUT_DIR": str(out)}), self._argv(repo, brief):
                with patches[0], patches[1], patches[2], patches[3]:
                    (repo / "app.py").write_text("second attempt\n", encoding="utf-8")
                    self.assertEqual(deliver.main(), 0)
            self.assertEqual(created, [], "a second PR must not be opened")
            _git(repo, "fetch", "-q", "origin", _JOB_BRANCH)
            self.assertEqual(_git(repo, "show", f"origin/{_JOB_BRANCH}:app.py"), "second attempt")
            self.assertEqual(_git(repo, "show", f"origin/{_JOB_BRANCH}:other.py"), "first attempt")
            self.assertEqual(_git(repo, "rev-parse", "--abbrev-ref", "HEAD"), "main")
            result = json.loads((out / "pr.json").read_text(encoding="utf-8"))
            self.assertEqual(result["pr"], _PR_URL)
            self.assertEqual(result["branch"], _JOB_BRANCH)

    def test_push_rejection_rebases_onto_job_branch(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo, brief = self._setup(root)
            self._first_attempt(root, root / "origin.git")
            patches, created = _stub_gh("[]", "")
            with self._env(root), self._argv(repo, brief):
                with patches[0], patches[1], patches[2], patches[3]:
                    (repo / "app.py").write_text("second attempt\n", encoding="utf-8")
                    self.assertEqual(deliver.main(), 0)
            self.assertEqual(len(created), 1, "exactly one PR after the rebase")
            _git(repo, "fetch", "-q", "origin", _JOB_BRANCH)
            self.assertEqual(_git(repo, "show", f"origin/{_JOB_BRANCH}:app.py"), "second attempt")
            self.assertEqual(_git(repo, "show", f"origin/{_JOB_BRANCH}:other.py"), "first attempt")

    def test_no_changes_with_open_pr_finishes_as_already_delivered(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo, brief = self._setup(root)
            out = root / "outputs"
            out.mkdir()
            patches, created = _stub_gh(json.dumps([{"headRefName": _JOB_BRANCH, "url": _PR_URL}]), _PR_URL)
            with self._env(root), mock.patch.dict(os.environ, {"MACHINIST_OUTPUT_DIR": str(out)}), self._argv(repo, brief):
                with patches[0], patches[1], patches[2], patches[3]:
                    self.assertEqual(deliver.main(), 0)
            self.assertEqual(created, [])
            self.assertEqual(_git(repo, "rev-parse", "--abbrev-ref", "HEAD"), "main")
            self.assertEqual(json.loads((out / "pr.json").read_text(encoding="utf-8"))["pr"], _PR_URL)
            # Nothing was pushed: the remote tip is still the bare seed.
            self.assertEqual(_git(repo, "ls-remote", "origin", "refs/heads/main").split("\t")[0],
                             _git(repo, "rev-parse", "main"))


class PushRejectionHelpersTests(unittest.TestCase):
    def test_is_push_rejection_matches_rejections_only(self) -> None:
        self.assertTrue(deliver.is_push_rejection("! [rejected] main -> main (non-fast-forward)"))
        self.assertTrue(deliver.is_push_rejection("error: failed to push some refs to 'origin'"))
        self.assertFalse(deliver.is_push_rejection("remote: protected branch hook declined"))
        self.assertFalse(deliver.is_push_rejection(""))

    def test_find_job_pr_matches_head_branch_prefix(self) -> None:
        with mock.patch.object(deliver, "run", return_value='[{"headRefName":"factory/MACH-71-x-aaaaaa","url":"u1"}]'):
            self.assertEqual(deliver.find_job_pr("test/repo", "factory/MACH-71-")["url"], "u1")
        with mock.patch.object(deliver, "run", return_value=""):
            self.assertIsNone(deliver.find_job_pr("test/repo", "factory/MACH-71-"))

    def test_find_job_branch_matches_remote_prefix(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            origin = root / "origin.git"
            subprocess.run(["git", "init", "-q", "--bare", "-b", "main", str(origin)], check=True)
            repo = root / "repo"
            repo.mkdir()
            _git(repo, "init", "-q", "-b", "main")
            _git(repo, "config", "user.name", "Test")
            _git(repo, "config", "user.email", "test@example.com")
            (repo / "app.py").write_text("base\n", encoding="utf-8")
            _git(repo, "add", "-A")
            _git(repo, "commit", "-q", "-m", "base")
            _git(repo, "remote", "add", "origin", str(origin))
            _git(repo, "push", "-q", "origin", "main")
            _git(repo, "push", "-q", "origin", "main:refs/heads/" + _JOB_BRANCH)
            _git(repo, "push", "-q", "origin", "main:refs/heads/factory/OTHER-job-999999")
            self.assertEqual(deliver.find_job_branch(str(repo), "factory/MACH-71-"), _JOB_BRANCH)
            self.assertIsNone(deliver.find_job_branch(str(repo), "factory/MACH-72-"))


if __name__ == "__main__":
    unittest.main()
