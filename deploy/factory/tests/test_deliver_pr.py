"""Tests for the deliver-pr secret-file guard (MACH-17)."""
from __future__ import annotations

import importlib.util
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
            artifacts = root / "artifacts"
            with mock.patch.dict(os.environ, {"MACHINIST_OUTPUT_DIR": str(artifacts)}):
                deliver.save_wip(str(repo))
            self.assertEqual(_git(repo, "status", "--porcelain"), "")
            self.assertEqual(len(list(artifacts.glob("*.patch"))), 1)

    def test_save_wip_is_a_noop_on_a_clean_lane(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo = _dirty_repo(root)
            _git(repo, "checkout", "--", "README.md")
            with mock.patch.dict(os.environ, {"MACHINIST_OUTPUT_DIR": str(root / "artifacts")}):
                deliver.save_wip(str(repo))
            self.assertEqual(_git(repo, "stash", "list"), "")


if __name__ == "__main__":
    unittest.main()
