import getpass
import grp
import json
import os
import pathlib
import stat
import subprocess
import tempfile
import unittest

REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
INSTALL = REPO_ROOT / "deploy" / "install.sh"


class InstallTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.tmp.name)
        self.env = dict(
            os.environ,
            MACHINIST_FACTORY_DIR=str(self.root / "factory"),
            MACHINIST_SYSTEMD_DIR=str(self.root / "systemd"),
            MACHINIST_CONFIG_DIR=str(self.root / "etc"),
            MACHINIST_MANIFEST=str(self.root / "etc" / "installed.json"),
            MACHINIST_SKIP_SYSTEMCTL="1",
        )

    def tearDown(self):
        self.tmp.cleanup()

    def install(self, *extra):
        return subprocess.run(["bash", str(INSTALL), *extra], env=self.env,
                              capture_output=True, text=True)

    def head_sha(self):
        return subprocess.run(["git", "-C", str(REPO_ROOT), "rev-parse", "HEAD"],
                              check=True, capture_output=True, text=True).stdout.strip()

    def test_installs_scripts_units_config_and_manifest(self):
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.root / "factory" / "deliver-pr.py").exists())
        self.assertFalse((self.root / "factory" / "fail.py").exists())
        self.assertTrue((self.root / "factory" / "drift-check.sh").exists())
        self.assertTrue((self.root / "systemd" / "machinist-control.service").exists())
        self.assertTrue((self.root / "systemd" / "machinist-worker@.service.d" / "memory.conf").exists())
        self.assertTrue((self.root / "etc" / "config.toml.example").exists())
        self.assertTrue((self.root / "etc" / "config.toml").exists())
        manifest = json.loads((self.root / "etc" / "installed.json").read_text())
        self.assertEqual(manifest["sha"], self.head_sha())
        self.assertEqual(manifest["repo"], str(REPO_ROOT))
        self.assertTrue(any(f["path"].endswith("machinist-worker.service") for f in manifest["files"]))

    def test_rerun_is_idempotent_and_keeps_local_files(self):
        self.assertEqual(self.install().returncode, 0)
        overlay = self.root / "etc" / "config.local.toml"
        overlay.write_text("listen = \"127.0.0.1:9999\"\n")
        secret_env = self.root / "etc" / "plane.env"
        secret_env.write_text("PLANE_API_KEY=live-value\n")
        second = self.install()
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertNotIn("install /", second.stdout)
        self.assertEqual(overlay.read_text(), "listen = \"127.0.0.1:9999\"\n")
        self.assertEqual(secret_env.read_text(), "PLANE_API_KEY=live-value\n")

    def test_dry_run_writes_nothing(self):
        result = self.install("--dry-run")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("dry-run", result.stdout)
        self.assertFalse((self.root / "factory").exists())
        self.assertFalse((self.root / "etc" / "installed.json").exists())

    def test_refuses_when_gitleaks_reports_findings(self):
        fake_bin = self.root / "bin"
        fake_bin.mkdir()
        fake = fake_bin / "gitleaks"
        fake.write_text("#!/bin/sh\nexit 1\n")
        fake.chmod(0o755)
        env = dict(self.env, PATH=f"{fake_bin}:{self.env['PATH']}")
        result = subprocess.run(["bash", str(INSTALL)], env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 1)
        self.assertIn("refusing to install", result.stderr)
        self.assertFalse((self.root / "factory").exists())

    def install_with_alerts_access(self, *extra):
        """Run the installer with the alert-access provisioning enabled
        (MACHINIST_ALERTS_PROVISION) against the temp root. The spool group is
        the current user's own group, so chown/chmod/setfacl run for real;
        groupadd and usermod are shimmed (they need root) and record their
        invocations in $root/shims.log."""
        user = getpass.getuser()
        group = grp.getgrgid(os.getgid()).gr_name
        home = self.root / "home" / user
        pending = home / ".machinist" / "alerts" / "pending"
        self.env["MACHINIST_CONFIG_DIR"] = str(self.root / "etc")
        self.env["MACHINIST_TMPFILES_DIR"] = str(self.root / "tmpfiles")
        config = self.root / "etc" / "config.toml"
        config.parent.mkdir(parents=True, exist_ok=True)
        config.write_text(
            "[server]\nlisten = \"127.0.0.1:7331\"\n\n"
            "[alerts]\n"
            f'directory = "{pending}"\n'
            f'group = "{group}"\n'
            'readers = ["machinist"]\n'
            'file_mode = "0640"\n')
        shims = self.root / "shims"
        shims.mkdir(exist_ok=True)
        for tool in ("groupadd", "usermod"):
            shim = shims / tool
            shim.write_text(f'#!/bin/sh\nprintf \'{tool} %s\\n\' "$*" >> {shims}/shims.log\n')
            shim.chmod(0o755)
        env = dict(self.env, MACHINIST_SERVICE_USER=user,
                   MACHINIST_SERVICE_HOME=str(home),
                   MACHINIST_ALERTS_PROVISION="1",
                   PATH=f"{shims}:{self.env['PATH']}")
        return subprocess.run(["bash", str(INSTALL), *extra], env=env,
                              capture_output=True, text=True), pending

    def test_alert_access_provisioning_and_idempotence(self):
        result, pending = self.install_with_alerts_access()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("provision alert directory", result.stdout)
        group_name = grp.getgrgid(os.getgid()).gr_name
        user = getpass.getuser()
        alerts_root = pending.parent
        for directory in (alerts_root, pending, alerts_root / "done"):
            info = os.stat(directory)
            self.assertEqual(stat.S_IMODE(info.st_mode), 0o2770, str(directory))
            self.assertTrue(info.st_mode & stat.S_ISGID, str(directory))
            self.assertEqual(info.st_uid, os.getuid(), str(directory))
            self.assertEqual(info.st_gid, os.getgid(), str(directory))
            acl = subprocess.run(["getfacl", "-p", "--absolute-names", str(directory)],
                                 capture_output=True, text=True, check=True).stdout
            self.assertIn(f"default:group:{group_name}:rwx", acl, str(directory))
        # The spool group already exists (it is the current user's own group),
        # so groupadd is skipped; the reader is already a member, so usermod is
        # skipped too. Only genuinely missing groups/users invoke the shims.
        shims_log = (self.root / "shims" / "shims.log").read_text() if (self.root / "shims" / "shims.log").exists() else ""
        self.assertNotIn("groupadd", shims_log)
        self.assertNotIn("usermod", shims_log)
        # Readers get only --x by ACL on the parent directories; the modes of
        # the parent directories themselves are never touched (no chmod).
        for directory in (alerts_root.parent, alerts_root.parent.parent):
            self.assertEqual(stat.S_IMODE(os.stat(directory).st_mode), 0o755, str(directory))
            acl = subprocess.run(["getfacl", "-p", "--absolute-names", str(directory)],
                                 capture_output=True, text=True, check=True).stdout
            self.assertIn(f"user:{user}:--x", acl, str(directory))
            self.assertNotIn(f"user:{user}:rwx", acl, str(directory))
        conf = self.root / "tmpfiles" / "machinist-alerts.conf"
        self.assertTrue(conf.exists())
        body = conf.read_text()
        self.assertIn(f"d {pending} 2770 {user} {group_name}", body)
        self.assertIn(f"a+ {pending} - - - - d:g:{group_name}:rwx", body)
        manifest = json.loads((self.root / "etc" / "installed.json").read_text())
        self.assertIn(str(conf), [f["path"] for f in manifest["files"]])
        # A second run changes nothing.
        before = conf.read_text()
        second, _ = self.install_with_alerts_access()
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(conf.read_text(), before)
        for directory in (alerts_root, pending, alerts_root / "done"):
            self.assertEqual(stat.S_IMODE(os.stat(directory).st_mode), 0o2770)

    def test_alert_access_dry_run_writes_nothing(self):
        result, pending = self.install_with_alerts_access("--dry-run")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("dry-run: mkdir -p", result.stdout)
        self.assertFalse(pending.exists())
        self.assertFalse((self.root / "tmpfiles" / "machinist-alerts.conf").exists())

    def test_alert_access_retires_stopgap_watcher(self):
        unit_dir = self.root / "systemd"
        unit_dir.mkdir()
        (unit_dir / "machinist-alerts-perms.service").write_text("[Unit]\n")
        (unit_dir / "machinist-alerts-perms.path").write_text("[Path]\n")
        result, _ = self.install_with_alerts_access()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("retire stopgap unit", result.stdout)
        self.assertFalse((unit_dir / "machinist-alerts-perms.service").exists())
        self.assertFalse((unit_dir / "machinist-alerts-perms.path").exists())

    def test_drift_check_reports_clean_after_install(self):
        result, _ = self.install_with_alerts_access()
        self.assertEqual(result.returncode, 0, result.stderr)
        user = getpass.getuser()
        status = self.root / "drift.json"
        drift = subprocess.run(
            ["bash", str(REPO_ROOT / "deploy" / "drift-check.sh")],
            env=dict(self.env,
                     MACHINIST_ALERTS_DIR=str(self.root / "drift-alerts"),
                     MACHINIST_DRIFT_STATUS=str(status),
                     MACHINIST_CONFIG=str(self.root / "etc" / "config.toml"),
                     MACHINIST_TMPFILES_DIR=str(self.root / "tmpfiles"),
                     MACHINIST_SERVICE_USER=user,
                     MACHINIST_SERVICE_HOME=str(self.root / "home" / user)),
            capture_output=True, text=True)
        parsed = json.loads(status.read_text())
        # The repo-at-SHA comparison legitimately flags files changed in an
        # uncommitted working tree; what must be clean is the installed copy
        # (recorded sha256) and the alert access provisioning.
        sha12 = parsed["sha"][:12]
        unexpected = [entry for entry in parsed["drifted"]
                      if set(entry["reasons"]) - {f"differs from repo@{sha12}"}]
        self.assertEqual(unexpected, [], unexpected)
        self.assertEqual(drift.returncode, 2 if parsed["drifted"] else 0, drift.stderr)
        self.assertTrue(parsed["alerts_access"]["ok"], parsed["alerts_access"])

    def test_refuses_when_gitleaks_reports_findings(self):
        fake_bin = self.root / "bin"
        fake_bin.mkdir()
        fake = fake_bin / "gitleaks"
        fake.write_text("#!/bin/sh\nexit 1\n")
        fake.chmod(0o755)
        env = dict(self.env, PATH=f"{fake_bin}:{self.env['PATH']}")
        result = subprocess.run(["bash", str(INSTALL)], env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 1)
        self.assertIn("refusing to install", result.stderr)
        self.assertFalse((self.root / "factory").exists())


if __name__ == "__main__":
    unittest.main()