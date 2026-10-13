"""Tests for MACH-31: the built-in review must run as a plain script from the
repo and from a flat install directory, INCOMPLETE must not fail the delivery
job (only BLOCKED returns 2), and the review executor defaults to
MACHINIST_REVIEW_EXECUTOR, then omp-glm next to deliver-pr.py, then none."""
from __future__ import annotations

import importlib.util
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest
from unittest import mock

import deploy.factory.post_pr_review as post

_FACTORY = Path(__file__).resolve().parents[1]
_REPO_ROOT = _FACTORY.parents[1]


def _load_deliver_from(path: Path):
    """Load a deliver-pr.py copy so __file__-relative logic (omp-glm lookup)
    resolves against that copy's directory."""
    name = f"deliver_pr_mach31_{abs(hash(str(path)))}"
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class ScriptRunnableTests(unittest.TestCase):
    def test_help_from_flat_install_directory(self) -> None:
        """deliver-pr.py, post_pr_review.py and review/ side by side, as
        installed in /usr/local/lib/machinist-factory/."""
        with tempfile.TemporaryDirectory() as temp:
            tmp = Path(temp)
            shutil.copy(_FACTORY / "deliver-pr.py", tmp / "deliver-pr.py")
            shutil.copy(_FACTORY / "post_pr_review.py", tmp / "post_pr_review.py")
            shutil.copytree(_FACTORY / "review", tmp / "review", ignore=shutil.ignore_patterns("__pycache__"))
            result = subprocess.run(
                [sys.executable, str(tmp / "post_pr_review.py"), "--help"],
                capture_output=True,
                text=True,
                timeout=120,
            )
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_help_from_repo(self) -> None:
        result = subprocess.run(
            [sys.executable, "deploy/factory/post_pr_review.py", "--help"],
            cwd=_REPO_ROOT,
            capture_output=True,
            text=True,
            timeout=120,
        )
        self.assertEqual(result.returncode, 0, result.stderr)


class ReviewExecutorDefaultTests(unittest.TestCase):
    def test_env_variable_wins(self) -> None:
        deliver = _load_deliver_from(_FACTORY / "deliver-pr.py")
        env = os.environ.copy()
        env["MACHINIST_REVIEW_EXECUTOR"] = "my-executor fast"
        with mock.patch.dict(os.environ, env, clear=True):
            self.assertEqual(deliver.review_executor_cmd(), "my-executor fast")

    def test_omp_glm_next_to_script(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            tmp = Path(temp)
            shutil.copy(_FACTORY / "deliver-pr.py", tmp / "deliver-pr.py")
            omp = tmp / "omp-glm"
            omp.write_text("#!/bin/sh\n", encoding="utf-8")
            omp.chmod(0o755)
            deliver = _load_deliver_from(tmp / "deliver-pr.py")
            env = {k: v for k, v in os.environ.items() if k != "MACHINIST_REVIEW_EXECUTOR"}
            with mock.patch.dict(os.environ, env, clear=True):
                self.assertEqual(deliver.review_executor_cmd(), f"{omp} medium")

    def test_none_when_no_executor_available(self) -> None:
        deliver = _load_deliver_from(_FACTORY / "deliver-pr.py")
        env = {k: v for k, v in os.environ.items() if k != "MACHINIST_REVIEW_EXECUTOR"}
        with mock.patch.dict(os.environ, env, clear=True):
            self.assertIsNone(deliver.review_executor_cmd())


class DeliverExitPolicyTests(unittest.TestCase):
    """INCOMPLETE must not fail the delivery job (exit 0); only BLOCKED (2)
    propagates as a failure."""

    def _run_review_with_verdict(self, code: int) -> int:
        deliver = _load_deliver_from(_FACTORY / "deliver-pr.py")
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            script = root / "fake_review.py"
            script.write_text(
                textwrap.dedent(
                    f"""
                    import sys
                    sys.exit({code})
                    """
                ),
                encoding="utf-8",
            )
            env = os.environ.copy()
            env["MACHINIST_REVIEW_SCRIPT"] = str(script)
            env.pop("MACHINIST_REVIEW_EXECUTOR", None)
            with mock.patch.dict(os.environ, env, clear=False):
                return deliver.run_review(str(root), "https://github.com/owner/repo/pull/1")

    def test_incomplete_verdict_does_not_fail_delivery(self) -> None:
        self.assertEqual(self._run_review_with_verdict(3), 0)

    def test_crashed_review_does_not_fail_delivery(self) -> None:
        self.assertEqual(self._run_review_with_verdict(1), 0)

    def test_blocked_verdict_fails_delivery(self) -> None:
        self.assertEqual(self._run_review_with_verdict(2), 2)

    def test_exit_code_mapping(self) -> None:
        self.assertEqual(post.EXIT_CODES["PASS"], 0)
        self.assertEqual(post.EXIT_CODES["INCOMPLETE"], 0)
        self.assertEqual(post.EXIT_CODES["BLOCKED"], 2)


if __name__ == "__main__":
    unittest.main()
