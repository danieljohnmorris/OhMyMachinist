import json
import os
import pathlib
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
        self.assertTrue((self.root / "factory" / "fail.py").exists())
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


if __name__ == "__main__":
    unittest.main()