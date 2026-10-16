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
        self.root = pathlib.Path(self.tmp.name)
        root = self.root
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

    def test_unmanaged_dropin_is_reported_as_local_override(self):
        dropin_dir = self.root / "etc" / "systemd" / "machinist-control.service.d"
        dropin_dir.mkdir()
        local = dropin_dir / "90-local.conf"
        local.write_text("[Service]\nEnvironment=PATH=/opt/tools/bin\n")
        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stderr)
        status = json.loads(self.status.read_text())
        self.assertEqual(status["local_overrides"], [str(local)])
        self.assertIn("local override", result.stderr)

    def test_stale_worker_is_flagged(self):
        worker = self.root / "etc" / "systemd" / "machinist-worker.service"
        worker.write_text("[Service]\nExecStart=/usr/local/bin/machinist worker start\n")
        binary = self.root / "bin" / "machinist"
        binary.parent.mkdir()
        binary.write_text("#!/bin/sh\n")
        binary.chmod(0o755)
        # A worker process the fake systemctl reports; it started "before" the
        # binary and the unit file were updated.
        proc = subprocess.Popen(["sleep", "300"])

        def stop_proc():
            proc.terminate()
            proc.wait()

        self.addCleanup(stop_proc)
        started = os.stat("/proc/%d" % proc.pid).st_mtime
        future = started + 120
        os.utime(binary, (future, future))
        os.utime(worker, (future, future))
        fake_bin = self.root / "fakebin"
        fake_bin.mkdir()
        fake = fake_bin / "systemctl"
        fake.write_text("#!/bin/sh\n[ \"$1\" = show ] && echo %d && exit 0\nexit 0\n" % proc.pid)
        fake.chmod(0o755)
        self.manifest.write_text(json.dumps({
            "sha": self.sha,
            "repo": str(self.repo),
            "binary": {"path": str(binary), "sha256": "x", "mtime": future},
            "files": [
                {"path": str(self.installed), "source": "deploy/systemd/machinist-control.service",
                 "sha256": sha256(self.installed)},
                {"path": str(worker), "source": "deploy/systemd/machinist-worker.service",
                 "sha256": sha256(worker)},
            ],
        }))
        env = dict(self.env, PATH=f"{fake_bin}:{self.env['PATH']}")
        result = subprocess.run(["bash", str(DRIFT_CHECK)], env=env,
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)  # reported, not drift
        status = json.loads(self.status.read_text())
        self.assertEqual(status["stale_workers"], [{
            "unit": "machinist-worker.service", "pid": proc.pid,
            "reasons": ["binary updated after start", "unit updated after start"],
        }])
        self.assertIn("stale worker", result.stderr)

    def test_fresh_worker_is_not_flagged(self):
        worker = self.root / "etc" / "systemd" / "machinist-worker.service"
        worker.write_text("[Service]\n")
        self.manifest.write_text(json.dumps({
            "sha": self.sha,
            "repo": str(self.repo),
            "binary": {"path": str(self.root / "nope"), "sha256": "x", "mtime": 1},
            "files": [{"path": str(worker), "source": "deploy/systemd/machinist-worker.service",
                       "sha256": sha256(worker)}],
        }))
        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(self.status.read_text())["stale_workers"], [])


if __name__ == "__main__":
    unittest.main()