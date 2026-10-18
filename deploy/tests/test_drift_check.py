import hashlib
import getpass
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
        self.root = root
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

    def run_check(self, env_extra=None):
        env = dict(self.env, **(env_extra or {}))
        return subprocess.run(["bash", str(DRIFT_CHECK)], env=env,
                              capture_output=True, text=True)

    def test_clean_tree_reports_no_drift(self):
        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(json.loads(self.status.read_text())["ok"])
        self.assertFalse(self.alerts.exists())

    def test_missing_alerts_section_warns(self):
        config = pathlib.Path(self.tmp.name) / "config.toml"
        config.write_text("[server]\nlisten = \"127.0.0.1:7331\"\n")
        result = self.run_check({"MACHINIST_CONFIG": str(config)})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("WARNING", result.stderr)
        self.assertIn("[alerts]", result.stderr)
        status = json.loads(self.status.read_text())
        self.assertFalse(status["alerts_configured"])

    def _group_exists(self, group):
        import grp
        try:
            grp.getgrnam(group)
            return True
        except KeyError:
            return False

    def provision_alerts(self, group="machinist", user=None, mode=0o2770):
        """Create a spool layout matching a provisioned MACH-72 install and
        return the env pointing the drift check at it. Uses the current user's
        own group so no root is needed."""
        user = user or getpass.getuser()
        home = self.root / "home" / user
        pending = home / ".machinist" / "alerts" / "pending"
        dirs = [home / ".machinist" / "alerts", pending,
                home / ".machinist" / "alerts" / "done"]
        for directory in dirs:
            directory.mkdir(parents=True, exist_ok=True)
            directory.chmod(mode)
        if mode == 0o2770 and group and self._group_exists(group):
            subprocess.run(["setfacl", "-m",
                            f"g:{group}:rwx,d:g:{group}:rwx,d:u:{user}:rwx",
                            *[str(d) for d in dirs]], check=True,
                           capture_output=True)
        tmpfiles = self.root / "tmpfiles"
        tmpfiles.mkdir(exist_ok=True)
        conf = tmpfiles / "machinist-alerts.conf"
        lines = []
        for directory in dirs:
            lines.append(f"d {directory} 2770 {user} {group} -")
        for directory in dirs:
            lines.append(f"a+ {directory} - - - - d:g:{group}:rwx")
            lines.append(f"a+ {directory} - - - - d:u:{user}:rwx")
        conf.write_text("\n".join(lines) + "\n")
        config = pathlib.Path(self.tmp.name) / "config.toml"
        config.write_text(
            "[alerts]\n"
            f'directory = "{pending}"\n'
            f'group = "{group}"\n'
            'file_mode = "0640"\n')
        return {"MACHINIST_CONFIG": str(config),
                "MACHINIST_SERVICE_USER": user,
                "MACHINIST_TMPFILES_DIR": str(tmpfiles)}

    def test_present_alerts_section_does_not_warn(self):
        env = self.provision_alerts()
        result = self.run_check(env)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("WARNING", result.stderr)
        self.assertNotIn("alerts access drift", result.stderr)
        self.assertTrue(json.loads(self.status.read_text())["alerts_configured"])
        self.assertTrue(json.loads(self.status.read_text())["alerts_access"]["ok"])

    def test_alerts_access_drifts_when_group_is_missing(self):
        result = self.run_check(self.provision_alerts(group="no-such-group"))
        self.assertEqual(result.returncode, 2, result.stderr)
        status = json.loads(self.status.read_text())
        self.assertFalse(status["alerts_access"]["ok"])
        self.assertIn("does not exist", status["alerts_access"]["reasons"][0])
        alerts = list(self.alerts.glob("drift-*.json"))
        self.assertEqual(len(alerts), 1)

    def test_alerts_access_drifts_when_dir_mode_changes(self):
        env = self.provision_alerts()
        (self.root / "home" / getpass.getuser() / ".machinist" / "alerts" / "pending").chmod(0o770)
        result = self.run_check(env)
        self.assertEqual(result.returncode, 2, result.stderr)
        reasons = json.loads(self.status.read_text())["alerts_access"]["reasons"]
        self.assertIn("want setgid 2770", reasons[0])

    def test_alerts_access_drifts_when_default_acl_is_removed(self):
        env = self.provision_alerts()
        subprocess.run(["setfacl", "-k",
                        str(self.root / "home" / getpass.getuser() / ".machinist" / "alerts" / "done")],
                       check=True, capture_output=True)
        result = self.run_check(env)
        self.assertEqual(result.returncode, 2, result.stderr)
        reasons = json.loads(self.status.read_text())["alerts_access"]["reasons"]
        self.assertIn("lacks the default ACL", reasons[0])

    def test_alerts_access_drifts_when_tmpfiles_rule_is_missing(self):
        env = self.provision_alerts()
        env["MACHINIST_TMPFILES_DIR"] = str(self.root / "empty-tmpfiles")
        (self.root / "empty-tmpfiles").mkdir()
        result = self.run_check(env)
        self.assertEqual(result.returncode, 2, result.stderr)
        reasons = json.loads(self.status.read_text())["alerts_access"]["reasons"]
        self.assertIn("tmpfiles rule", reasons[0])

    def test_alerts_access_flags_stopgap_units(self):
        env = self.provision_alerts()
        unit_dir = self.root / "systemd"
        unit_dir.mkdir()
        (unit_dir / "machinist-alerts-perms.service").write_text("[Unit]\n")
        env["MACHINIST_SYSTEMD_DIR"] = str(unit_dir)
        result = self.run_check(env)
        self.assertEqual(result.returncode, 2, result.stderr)
        reasons = json.loads(self.status.read_text())["alerts_access"]["reasons"]
        self.assertIn("stopgap unit", reasons[0])

    def test_alerts_access_flags_unreadable_alert_files(self):
        env = self.provision_alerts()
        pending = self.root / "home" / getpass.getuser() / ".machinist" / "alerts" / "pending"
        (pending / "fail-run_1.json").write_text("{}\n")
        (pending / "fail-run_1.json").chmod(0o600)
        result = self.run_check(env)
        self.assertEqual(result.returncode, 2, result.stderr)
        reasons = json.loads(self.status.read_text())["alerts_access"]["reasons"]
        self.assertIn("not group-readable", reasons[0])

    def test_alerts_access_accepts_group_readable_alert_files(self):
        env = self.provision_alerts()
        pending = self.root / "home" / getpass.getuser() / ".machinist" / "alerts" / "pending"
        alert = pending / "fail-run_1.json"
        alert.write_text("{}\n")
        alert.chmod(0o640)
        result = self.run_check(env)
        self.assertEqual(result.returncode, 0, result.stderr)

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