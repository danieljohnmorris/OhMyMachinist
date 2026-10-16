"""Tests for the single dirty-lane definition (MACH-62).

deploy/factory/lane-dirty.sh is the one place that answers "is this lane dirty?",
shared by the runner and the omp-glm-pr preflight: `git status --porcelain`, which
counts tracked modifications AND untracked files and never counts ignored ones.
"""
from __future__ import annotations

import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "lane-dirty.sh"
WRAPPER = SCRIPT.parent / "omp-glm-pr"


def git(repo: Path, *args: str) -> str:
    result = subprocess.run(
        ["git", "-C", str(repo), "-c", "user.email=test@example.invalid", "-c", "user.name=Test", *args],
        capture_output=True, text=True)
    if result.returncode != 0:
        raise AssertionError(f"git {' '.join(args)} failed: {result.stderr}")
    return result.stdout


class LaneDirtyTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.repo = Path(self.tmp.name) / "lane"
        self.repo.mkdir()
        git(self.repo, "init", "-q")
        (self.repo / "tracked.txt").write_text("one\n")
        (self.repo / ".gitignore").write_text("ignored.txt\n")
        git(self.repo, "add", "tracked.txt", ".gitignore")
        git(self.repo, "commit", "-q", "-m", "init")

    def tearDown(self) -> None:
        self.tmp.cleanup()

    def dirty(self) -> str:
        result = subprocess.run(["bash", str(SCRIPT), str(self.repo)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        return result.stdout

    def test_script_is_executable(self) -> None:
        self.assertTrue(os.access(SCRIPT, os.X_OK), "lane-dirty.sh must be mode 0755")

    def test_clean_lane_prints_nothing(self) -> None:
        self.assertEqual(self.dirty(), "")

    def test_modified_tracked_file_is_dirty(self) -> None:
        (self.repo / "tracked.txt").write_text("two\n")
        self.assertEqual(self.dirty().splitlines(), [" M tracked.txt"])

    def test_untracked_file_is_dirty(self) -> None:
        (self.repo / "untracked.txt").write_text("hello\n")
        self.assertEqual(self.dirty().splitlines(), ["?? untracked.txt"])

    def test_ignored_file_is_not_dirty(self) -> None:
        (self.repo / "ignored.txt").write_text("scratch\n")
        self.assertEqual(self.dirty(), "")

    def test_untracked_file_is_dirty_even_next_to_committed_work(self) -> None:
        (self.repo / "tracked.txt").write_text("two\n")
        (self.repo / "untracked.txt").write_text("hello\n")
        self.assertEqual(self.dirty().splitlines(), [" M tracked.txt", "?? untracked.txt"])

    def test_missing_repo_argument_prints_nothing_and_exits_zero(self) -> None:
        result = subprocess.run(["bash", str(SCRIPT)], capture_output=True, text=True)
        self.assertEqual(result.returncode, 0)

    def test_omp_glm_pr_preflight_uses_this_script(self) -> None:
        wrapper = WRAPPER.read_text()
        self.assertIn('"$HERE/lane-dirty.sh" "$repo"', wrapper)
        self.assertNotIn('git -C "$repo" status --porcelain', wrapper)


if __name__ == "__main__":
    unittest.main()