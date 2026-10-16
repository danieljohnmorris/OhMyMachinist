import hashlib
import json
import os
import pathlib
import subprocess
import tempfile
import unittest

REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
DRIFT_CHECK = REPO_ROOT / "deploy" / "drift-check.sh"


def sha256(path):
    return hashlib.sha256(pathlib.Path(path).read_bytes()).hexdigest()


class DriftCheckTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = pathlib.Path(self.tmp.name)
        self.repo = root / "repo"
        self.repo.mkdir()
        (self.repo / "deploy" / "systemd").mkdir(parents=True)
        self.source = self.repo / "deploy" / "systemd" / "machinist-control.service"
        self.source.write_text("[Unit]\nDescription=test\n")
        subprocess.run(["git", "-C", str(self.repo), "init", "-q"], check=True)
        subprocess.run(["git", "-C", str(self.repo), "add", "."], check=True)
        subprocess.run(
            ["git", "-C", str(self.repo), "-c", "user.name=t", "-c", "user.email=t@e",
             "commit", "-q", "-m", "init"],
            check=True,
        )
        self.sha = subprocess.run(
            ["git", "-C", str(self.repo), "rev-parse", "HEAD"],
            check=True, capture_output=True, text=True,
        ).stdout.strip()

        self.installed = root / "etc" / "systemd" / "machinist-control.service"
        self.installed.parent.mkdir(parents=True)
        self.installed.write_bytes(self.source.read_bytes())
        self.manifest = root / "installed.json"
        self.alerts = root / "alerts"
        self.status = root / "drift.json"
        self.manifest.write_text(json.dumps({
            "sha": self.sha,
            "repo": str(self.repo),
            "files": [{
                "path": str(self.installed),
                "source": "deploy/systemd/machinist-control.service",
                "sha256": sha256(self.installed),
            }],
        }))
        self.env = dict(os.environ, MACHINIST_MANIFEST=str(self.manifest),
                        MACHINIST_ALERTS_DIR=str(self.alerts),
                        MACHINIST_DRIFT_STATUS=str(self.status))

        # Hermetic stale-worker check: a fake systemctl that reports no units.
        self.bin_dir = root / "bin"
        self.bin_dir.mkdir()
        fake = self.bin_dir / "systemctl"
        fake.write_text("#!/bin/sh\nexit 0\n")
        fake.chmod(0o755)
        self.env["PATH"] = f"{self.bin_dir}:{self.env['PATH']}"

        # The installed binary the manifest records (see stale-worker tests).
        self.binary = self.bin_dir / "machinist"
        self.binary.write_text("#!/bin/sh\nexit 0\n")

    def tearDown(self):
        self.tmp.cleanup()

    def run_check(self):
        return subprocess.run(["bash", str(DRIFT_CHECK)], env=self.env,
                              capture_output=True, text=True)

    def test_clean_tree_reports_no_drift(self):
        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(self.status.read_text())["ok"])
        self.assertFalse(self.alerts.exists())

    def test_edited_installed_file_alerts(self):
        self.installed.write_text("[Unit]\nDescription=hand-edited\n")
        result = self.run_check()
        self.assertEqual(result.returncode, 2, result.stderr)
        status = json.loads(self.status.read_text())
        self.assertFalse(status["ok"])
        self.assertEqual(status["drifted"][0]["path"], str(self.installed))
        alerts = list(self.alerts.glob("drift-*.json"))
        self.assertEqual(len(alerts), 1)
        alert = json.loads(alerts[0].read_text())
        self.assertEqual(alert["type"], "machinist.drift")
        self.assertEqual(alert["drifted"][0]["path"], str(self.installed))

    def test_missing_manifest_is_a_setup_error(self):
        self.manifest.unlink()
        result = self.run_check()
        self.assertEqual(result.returncode, 1)
        self.assertIn("missing", result.stderr)

    def fake_systemctl(self, state):
        """systemctl fake: `list-units` prints $state/units, `show` prints the pid."""
        fake = self.bin_dir / "systemctl"
        fake.write_text(
            '#!/bin/sh\n'
            'state="%s"\n'
            'case "$1" in\n'
            '  list-units) cat "$state/units" 2>/dev/null ;;\n'
            '  show) cat "$state/pid" 2>/dev/null ;;\n'
            'esac\n' % state)
        fake.chmod(0o755)
        state.mkdir(parents=True, exist_ok=True)

    def add_worker_unit(self):
        """Adds machinist-worker@.service (plus a drop-in) to the manifest."""
        worker_src = self.repo / "deploy" / "systemd" / "machinist-worker@.service"
        worker_src.write_text("[Unit]\nDescription=worker\n")
        dropin_src = self.repo / "deploy" / "systemd" / "machinist-worker@.service.d" / "memory.conf"
        dropin_src.parent.mkdir()
        dropin_src.write_text("[Service]\nMemoryHigh=1G\n")
        subprocess.run(["git", "-C", str(self.repo), "add", "."], check=True)
        subprocess.run(
            ["git", "-C", str(self.repo), "-c", "user.name=t", "-c", "user.email=t@e",
             "commit", "-q", "-m", "worker"],
            check=True,
        )
        worker_installed = self.installed.parent / "machinist-worker@.service"
        worker_installed.write_bytes(worker_src.read_bytes())
        dropin_installed = self.installed.parent / "machinist-worker@.service.d" / "memory.conf"
        dropin_installed.parent.mkdir()
        dropin_installed.write_bytes(dropin_src.read_bytes())
        self.manifest.write_text(json.dumps({
            "sha": self.sha,
            "repo": str(self.repo),
            "binary": str(self.binary),
            "files": [
                {"path": str(self.installed),
                 "source": "deploy/systemd/machinist-control.service",
                 "sha256": sha256(self.installed)},
                {"path": str(worker_installed),
                 "source": "deploy/systemd/machinist-worker@.service",
                 "sha256": sha256(worker_installed)},
                {"path": str(dropin_installed),
                 "source": "deploy/systemd/machinist-worker@.service.d/memory.conf",
                 "sha256": sha256(dropin_installed)},
            ],
        }))

    def test_unmanaged_dropin_is_reported_as_local_override(self):
        self.add_worker_unit()
        extra = self.installed.parent / "machinist-worker@.service.d" / "90-local.conf"
        extra.write_text("[Service]\nEnvironment=PATH=/opt/bin\n")
        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stderr)
        status = json.loads(self.status.read_text())
        self.assertEqual(status["local_overrides"], [str(extra)])
        self.assertIn("local override", result.stderr)
        self.assertFalse(self.alerts.exists())

    def test_stale_worker_is_flagged(self):
        self.add_worker_unit()
        # The fake systemctl reports a running lane whose MainPID is a real
        # (sleeping) process; the installed binary is stamped NEWER than the
        # process start, so the lane is running old code.
        state = self.installed.parent
        self.fake_systemctl(state)
        (state / "units").write_text(
            "machinist-worker@general-1.service loaded active running Machinist worker\n")
        proc = subprocess.Popen(["sleep", "300"])
        # Cleanup runs LIFO: terminate first, then wait, in one callable.
        self.addCleanup(lambda: (proc.terminate(), proc.wait()))
        (state / "pid").write_text("%d\n" % proc.pid)
        started = os.stat("/proc/%d" % proc.pid).st_mtime
        future = started + 120
        os.utime(self.binary, (future, future))

        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stderr)  # stale is not drift
        status = json.loads(self.status.read_text())
        self.assertEqual(status["stale_workers"][0]["unit"],
                         "machinist-worker@general-1.service")
        self.assertIn("stale worker", result.stderr)
        self.assertTrue(status["ok"])

    def test_fresh_worker_is_not_flagged(self):
        self.add_worker_unit()
        state = self.installed.parent
        self.fake_systemctl(state)
        (state / "units").write_text(
            "machinist-worker@general-1.service loaded active running Machinist worker\n")
        proc = subprocess.Popen(["sleep", "300"])
        self.addCleanup(lambda: (proc.terminate(), proc.wait()))
        (state / "pid").write_text("%d\n" % proc.pid)
        proc_start = os.stat("/proc/%d" % proc.pid).st_mtime
        os.utime(self.binary, (proc_start - 120, proc_start - 120))

        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.status.read_text())["stale_workers"], [])


if __name__ == "__main__":
    unittest.main()