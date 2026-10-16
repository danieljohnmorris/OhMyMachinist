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


if __name__ == "__main__":
    unittest.main()