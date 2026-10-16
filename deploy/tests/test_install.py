import json
import os
import pathlib
import shutil
import subprocess
import tempfile
import unittest

REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
INSTALL = REPO_ROOT / "deploy" / "install.sh"


class InstallTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.tmp.name)
        # Instance values a real host has before the installer runs: the
        # machinist binary and the control-plane worker token (~/ paths in the
        # configs expand against MACHINIST_HOME).
        fake_bin = self.root / "fakebin"
        fake_bin.mkdir()
        binary = fake_bin / "machinist"
        binary.write_text(
            "#!/bin/sh\n"
            "for a in \"$@\"; do\n"
            "  case \"$a\" in\n"
            "    version) echo \"machinist version 0.0.0-test\"; exit 0 ;;\n"
            "    pause|resume) [ -n \"$FAKE_MACHINIST_LOG\" ] && echo \"$a\" >> \"$FAKE_MACHINIST_LOG\"; exit 0 ;;\n"
            "  esac\n"
            "done\n"
            "exit 0\n"
        )
        binary.chmod(0o755)
        token = self.root / "home" / ".machinist" / "server" / "worker.token"
        token.parent.mkdir(parents=True)
        token.write_text("test-token\n")
        self.env = dict(
            os.environ,
            MACHINIST_FACTORY_DIR=str(self.root / "factory"),
            MACHINIST_SYSTEMD_DIR=str(self.root / "systemd"),
            MACHINIST_CONFIG_DIR=str(self.root / "etc"),
            MACHINIST_MANIFEST=str(self.root / "etc" / "installed.json"),
            MACHINIST_SKIP_SYSTEMCTL="1",
            MACHINIST_BIN=str(binary),
            MACHINIST_HOME=str(self.root / "home"),
            MACHINIST_ASSUME_PRIVILEGED="1",
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
        self.assertEqual(manifest["binary"]["path"], self.env["MACHINIST_BIN"])

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
        cwd_file = self.root / "gitleaks.cwd"
        args_file = self.root / "gitleaks.args"
        fake = fake_bin / "gitleaks"
        fake.write_text(
            "#!/bin/sh\n"
            f"pwd > {cwd_file}\n"
            f"printf '%s\\n' \"$@\" > {args_file}\n"
            "exit 1\n"
        )
        fake.chmod(0o755)
        env = dict(self.env, PATH=f"{fake_bin}:{self.env['PATH']}")
        result = subprocess.run(["bash", str(INSTALL)], env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 1)
        self.assertIn("refusing to install", result.stderr)
        self.assertFalse((self.root / "factory").exists())
        # The scan must run from inside the repo with a relative source, so the
        # fingerprints match the relative entries in .gitleaksignore.
        self.assertEqual(cwd_file.read_text().strip(), str(REPO_ROOT))
        args = args_file.read_text().split()
        self.assertIn(".", args)
        self.assertIn("--no-git", args)

    def test_gitleaks_real_scan_passes_on_clean_checkout(self):
        if shutil.which("gitleaks") is None:
            self.skipTest("gitleaks is not installed")
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_refuses_when_binary_missing(self):
        env = dict(self.env, MACHINIST_BIN=str(self.root / "absent-machinist"))
        result = subprocess.run(["bash", str(INSTALL)], env=env, capture_output=True, text=True)
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("refusing to touch any unit", result.stderr)
        self.assertFalse((self.root / "factory").exists())
        self.assertFalse((self.root / "systemd" / "machinist-control.service").exists())

    def test_uses_binary_from_configured_path(self):
        # setUp installs the fake binary outside /usr/local/bin; the installer
        # must check it there before touching any unit.
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("binary ok: " + self.env["MACHINIST_BIN"], result.stdout)

    def test_preflight_fails_on_missing_token(self):
        etc = self.root / "etc"
        etc.mkdir(exist_ok=True)
        (etc / "worker.toml").write_text(
            '[control_plane]\nurl = "http://127.0.0.1:7331"\ntoken_file = "server/worker.token"\n'
        )
        result = self.install()
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("pre-flight failed", result.stderr)
        self.assertIn(str(etc / "server" / "worker.token"), result.stderr)
        # Nothing is installed: no factory scripts, no units, no manifest.
        self.assertFalse((self.root / "factory").exists())
        self.assertFalse((self.root / "systemd" / "machinist-control.service").exists())
        self.assertFalse((self.root / "etc" / "installed.json").exists())

    def test_preflight_fails_on_missing_prompt_and_repository(self):
        etc = self.root / "etc"
        etc.mkdir(exist_ok=True)
        (etc / "config.toml").write_text(
            '[commands.task-to-pr]\nprompt_file = "prompts/task.md"\n'
            "\n"
            "[repositories.my-project]\npath = " + json.dumps(str(self.root / "no-such-repo")) + "\n"
        )
        result = self.install()
        self.assertEqual(result.returncode, 1)
        self.assertIn("prompts/task.md", result.stderr)
        self.assertIn("no-such-repo", result.stderr)

    def test_preflight_passes_with_absolute_paths(self):
        etc = self.root / "etc"
        etc.mkdir(exist_ok=True)
        token = self.root / "home" / ".machinist" / "server" / "worker.token"
        (etc / "worker.toml").write_text(
            "[control_plane]\ntoken_file = " + json.dumps(str(token)) + "\n"
        )
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_local_overrides_are_reported(self):
        dropin = self.root / "systemd" / "machinist-control.service.d"
        dropin.mkdir(parents=True)
        (dropin / "90-local.conf").write_text("[Service]\nEnvironment=PATH=/opt/tools/bin\n")
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("local override", result.stdout)
        self.assertIn(str(dropin / "90-local.conf"), result.stdout)

    # --- restart policy -------------------------------------------------------

    def fake_systemctl(self, state):
        """Write a fake systemctl driven by `state` (lines of '<state> <unit>')."""
        bin_dir = self.root / "sbin"
        bin_dir.mkdir(exist_ok=True)
        state_file = bin_dir / "state"
        state_file.write_text("".join(line + "\n" for line in state))
        log = bin_dir / "restarts.log"
        script = f"""#!/bin/sh
STATE={state_file}
LOG={log}
cmd="$1"; shift
case "$cmd" in
  is-active)
    [ "$1" = "--quiet" ] && shift
    grep -qxs "active $1" "$STATE"
    ;;
  is-enabled)
    [ "$1" = "--quiet" ] && shift
    grep -qxs "enabled $1" "$STATE"
    ;;
  restart)
    echo "restarted $1" >> "$LOG"
    ;;
  daemon-reload)
    ;;
  list-units)
    shift 3
    grep "^active machinist-worker@" "$STATE" | awk '{{print $2}}'
    ;;
  *)
    ;;
esac
"""
        fake = bin_dir / "systemctl"
        fake.write_text(script)
        fake.chmod(0o755)
        self.restarts_log = log
        return dict(self.env, MACHINIST_SYSTEMCTL=str(fake), MACHINIST_SKIP_SYSTEMCTL="0")

    def fake_machinist_log(self):
        log = self.root / "machinist-cli.log"
        self.env["FAKE_MACHINIST_LOG"] = str(log)
        return log

    def restart_test_env(self, state):
        self.fake_machinist_log()
        return self.fake_systemctl(state)

    def assertRestarted(self, env, unit):
        self.assertIn(f"restarted {unit}", self.restarts_log.read_text().splitlines())

    def assertNotRestarted(self, env, unit):
        if not self.restarts_log.exists():
            return
        self.assertNotIn(f"restarted {unit}", self.restarts_log.read_text().splitlines())

    def test_no_restart_touches_nothing(self):
        env = self.restart_test_env([
            "active machinist-control.service",
            "active machinist-worker@general-3",
        ])
        result = subprocess.run(["bash", str(INSTALL), "--no-restart"], env=env,
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("skipping restarts", result.stdout)
        self.assertFalse(self.restarts_log.exists() and self.restarts_log.read_text())

    def test_busy_lane_drains_and_is_reported_pending(self):
        lock = self.root / "home" / ".machinist" / "worker" / "lane.lock"
        lock.parent.mkdir(parents=True)
        lock.write_text("run-123\n")
        env = self.restart_test_env([
            "active machinist-control.service",
            "active machinist-worker@general-3",
        ])
        result = subprocess.run(
            ["bash", str(INSTALL), "--drain-timeout", "1"], env=env,
            capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        # Control plane restarts straight away; the busy lane does not.
        self.assertRestarted(env, "machinist-control.service")
        self.assertNotRestarted(env, "machinist-worker@general-3")
        self.assertIn("still running a job", result.stdout)
        self.assertIn("machinist-worker@general-3", result.stdout)
        self.assertIn("pending", result.stdout)
        # Claims were paused and resumed through the factory control.
        cli_log = pathlib.Path(self.env["FAKE_MACHINIST_LOG"]).read_text().splitlines()
        self.assertIn("pause", cli_log)
        self.assertIn("resume", cli_log)

    def test_idle_worker_restarts_without_drain(self):
        env = self.restart_test_env([
            "active machinist-worker@general-3",
        ])
        result = subprocess.run(["bash", str(INSTALL)], env=env,
                                capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertRestarted(env, "machinist-worker@general-3")
        self.assertNotIn("pending", result.stdout)
        self.assertFalse(pathlib.Path(self.env["FAKE_MACHINIST_LOG"]).exists())

    def test_restart_lanes_when_idle_skips_busy_lanes(self):
        lock = self.root / "home" / ".machinist" / "worker" / "lane.lock"
        lock.parent.mkdir(parents=True)
        lock.write_text("run-123\n")
        env = self.restart_test_env([
            "active machinist-worker@general-3",
        ])
        result = subprocess.run(
            ["bash", str(INSTALL), "--restart-lanes=when-idle"], env=env,
            capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotRestarted(env, "machinist-worker@general-3")
        self.assertIn("still running a job", result.stdout)
        cli_log = pathlib.Path(self.env["FAKE_MACHINIST_LOG"])
        self.assertFalse(cli_log.exists() and "pause" in cli_log.read_text())



if __name__ == "__main__":
    unittest.main()