"""MACH-67: the per-claim lane clean must save a dirty checkout (tracked and
untracked leftovers), stash it, and hand over to the real executor."""
import os
import pathlib
import subprocess
import tarfile
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).resolve().parents[1] / "omp-glm-pr-clean"


class OmpGlmPrCleanTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.tmp.name)
        self.repo = self.root / "checkout"
        self.repo.mkdir()
        subprocess.run(["git", "-C", str(self.repo), "init", "-q"], check=True)
        subprocess.run(["git", "-C", str(self.repo), "-c", "user.name=t",
                        "-c", "user.email=t@e", "add", "."], check=True)

        self.clean_dir = self.root / "backups"
        self.executor_calls = self.root / "executor.calls"
        self.executor = self.root / "fake-executor"
        self.executor.write_text(
            '#!/bin/sh\nprintf "%%s\\n" "$*" >> "%s"\n' % self.executor_calls)
        self.executor.chmod(0o755)

    def tearDown(self):
        self.tmp.cleanup()

    def commit(self, *args):
        subprocess.run(["git", "-C", str(self.repo), "-c", "user.name=t",
                        "-c", "user.email=t@e", "commit", "-q", *args],
                       check=True, cwd=self.repo)

    def git(self, *args):
        return subprocess.run(["git", "-C", str(self.repo), *args],
                              check=True, capture_output=True, text=True, cwd=self.repo)

    def run_clean(self, *args):
        env = dict(os.environ,
                   MACHINIST_LANE_CLEAN_DIR=str(self.clean_dir),
                   MACHINIST_WRAPPED_EXECUTOR=str(self.executor),
                   MACHINIST_RUN_ID="testrun")
        return subprocess.run([str(SCRIPT), *args], env=env, cwd=self.repo,
                              capture_output=True, text=True)

    def commit_readme(self):
        (self.repo / "README.md").write_text("hello\n")
        self.git("add", "README.md")
        self.commit("-m", "init")

    def test_dirty_checkout_is_saved_stashed_and_executor_runs(self):
        self.commit_readme()
        (self.repo / "README.md").write_text("dirty\n")          # tracked edit
        (self.repo / "stray.txt").write_text("leftover\n")        # untracked

        result = self.run_clean("--some-arg")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("--some-arg", self.executor_calls.read_text())

        # The checkout is clean again; the leftovers live in a stash and the
        # backups directory.
        self.assertEqual(self.git("status", "--porcelain").stdout, "")
        self.assertIn("lane-clean per-claim", self.git("stash", "list", "--format=%s").stdout)

        backups = sorted(self.clean_dir.iterdir())
        kinds = [p.name.rsplit("-", 1)[-1] for p in backups]
        self.assertTrue(any(p.name.endswith(".patch") for p in backups), kinds)
        self.assertTrue(any(p.name.endswith(".files.txt") for p in backups), kinds)
        self.assertTrue(any(p.name.endswith("-untracked.tar.gz") for p in backups), kinds)

        files_txt = next(p for p in backups if p.name.endswith(".files.txt")).read_text()
        self.assertIn("run: testrun", files_txt)
        self.assertIn("stray.txt", files_txt)
        tarball = next(p for p in backups if p.name.endswith("-untracked.tar.gz"))
        with tarfile.open(tarball) as tar:
            self.assertIn("stray.txt", tar.getnames())
        patch = next(p for p in backups if p.name.endswith(".patch")).read_text()
        self.assertIn("dirty", patch)

    def test_clean_checkout_runs_executor_without_stashing(self):
        self.commit_readme()
        result = self.run_clean()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.executor_calls.exists())
        self.assertEqual(self.git("stash", "list").stdout, "")
        self.assertFalse(self.clean_dir.exists())


if __name__ == "__main__":
    unittest.main()
