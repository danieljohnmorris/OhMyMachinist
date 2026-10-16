import os
import pathlib
import shutil
import subprocess
import tarfile
import tempfile
import unittest

REPO_ROOT = pathlib.Path(__file__).resolve().parents[3]
WRAPPER = REPO_ROOT / "deploy" / "factory" / "omp-glm-pr-clean"


class OmpGlmPrCleanTest(unittest.TestCase):
    """deploy/factory/omp-glm-pr-clean stashes a dirty lane, then execs omp-glm-pr."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = pathlib.Path(self.tmp.name)
        self.bin_dir = root / "bin"
        self.bin_dir.mkdir()
        shutil.copy2(WRAPPER, self.bin_dir / "omp-glm-pr-clean")
        # Fake executor: records that it ran; the wrapper execs it after cleaning.
        (self.bin_dir / "omp-glm-pr").write_text(
            "#!/bin/sh\n"
            "printf '%s\\n' \"$PWD\" >> \"$RAN_FILE\"\n"
            "git status --porcelain > \"$STATUS_FILE\"\n"
        )
        for name in ("omp-glm-pr-clean", "omp-glm-pr"):
            self.bin_dir.joinpath(name).chmod(0o755)
        self.wip = root / "wip"
        self.repo = root / "lane"
        self.repo.mkdir()
        self.git("init", "-q")
        self.git("config", "user.name", "t")
        self.git("config", "user.email", "t@example.com")
        (self.repo / "tracked.txt").write_text("base\n")
        self.git("add", ".")
        self.git("commit", "-q", "-m", "base")
        self.env = dict(
            os.environ,
            MACHINIST_WIP_DIR=str(self.wip),
            RAN_FILE=str(root / "ran.log"),
            STATUS_FILE=str(root / "status.txt"),
        )

    def tearDown(self):
        self.tmp.cleanup()

    def git(self, *args):
        return subprocess.run(["git", "-C", str(self.repo), *args],
                              capture_output=True, text=True, check=True)

    def run_wrapper(self):
        return subprocess.run([str(self.bin_dir / "omp-glm-pr-clean")],
                              cwd=self.repo, env=self.env,
                              capture_output=True, text=True)

    def test_clean_lane_execs_without_saving(self):
        result = self.run_wrapper()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.wip.exists())

    def test_dirty_lane_is_saved_stashed_and_started(self):
        (self.repo / "tracked.txt").write_text("edited\n")
        (self.repo / "leftover.txt").write_text("untracked\n")
        result = self.run_wrapper()
        self.assertEqual(result.returncode, 0, result.stderr)

        # The executor ran and saw a clean tree.
        self.assertIn(str(self.repo), pathlib.Path(self.env["RAN_FILE"]).read_text())
        self.assertEqual(pathlib.Path(self.env["STATUS_FILE"]).read_text(), "")

        # Patch, file list and tar landed outside the repo.
        patches = list(self.wip.glob("*.patch"))
        lists = list(self.wip.glob("*.files"))
        tars = list(self.wip.glob("*.tar.gz"))
        self.assertEqual(len(patches), 1)
        self.assertEqual(len(lists), 1)
        self.assertEqual(len(tars), 1)
        self.assertIn("edited", patches[0].read_text())
        self.assertEqual(lists[0].read_text().split(), ["leftover.txt"])
        with tarfile.open(tars[0]) as handle:
            self.assertIn("leftover.txt", handle.getnames())

        # The lane is clean again and the change is recoverable from the stash.
        self.assertEqual(self.git("status", "--porcelain").stdout, "")
        self.assertIn("machinist-claim-wip-", self.git("stash", "list").stdout)


if __name__ == "__main__":
    unittest.main()
