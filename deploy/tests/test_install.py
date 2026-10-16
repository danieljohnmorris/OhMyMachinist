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
        self.bin_dir = self.root / "bin"
        self.bin_dir.mkdir()
        self.fake_machinist = self.bin_dir / "machinist"
        self.fake_machinist.write_text(
            '#!/bin/sh\n[ "$1" = version ] && echo machinist-test v0 && exit 0\nexit 1\n')
        self.fake_machinist.chmod(0o755)
        self.machinist_home = self.root / "home"
        token = self.machinist_home / ".machinist" / "server" / "worker.token"
        token.parent.mkdir(parents=True)
        token.write_text("0" * 64)
        self.env = dict(
            os.environ,
            MACHINIST_FACTORY_DIR=str(self.root / "factory"),
            MACHINIST_SYSTEMD_DIR=str(self.root / "systemd"),
            MACHINIST_CONFIG_DIR=str(self.root / "etc"),
            MACHINIST_MANIFEST=str(self.root / "etc" / "installed.json"),
            MACHINIST_BIN=str(self.fake_machinist),
            MACHINIST_HOME=str(self.machinist_home),
            MACHINIST_SKIP_SYSTEMCTL="1",
        )

    def tearDown(self):
        self.tmp.cleanup()

    def install(self, *extra, env=None):
        return subprocess.run(["bash", str(INSTALL), *extra], env=env or self.env,
                              capture_output=True, text=True)

    def head_sha(self):
        return subprocess.run(["git", "-C", str(REPO_ROOT), "rev-parse", "HEAD"],
                              check=True, capture_output=True, text=True).stdout.strip()

    # --- gitleaks scan path -------------------------------------------------

    def test_gitleaks_scan_uses_relative_source_matching_gitleaksignore(self):
        # The fake scan refuses (exit 1) when invoked with the relative source
        # "." — the fixed behaviour — and succeeds otherwise, so the install
        # only succeeds if install.sh scans from inside the repo.
        fake = self.bin_dir / "gitleaks"
        fake.write_text('#!/bin/sh\ncase " $* " in *" --source . "*) exit 1;; esac\nexit 0\n')
        fake.chmod(0o755)
        env = dict(self.env, PATH=f"{self.bin_dir}:{self.env['PATH']}")
        result = self.install(env=env)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("refusing to install", result.stderr)

    def test_gitleaks_findings_are_printed_on_refusal(self):
        fake = self.bin_dir / "gitleaks"
        fake.write_text('#!/bin/sh\necho "fake.ts:generic-api-key:1"\nexit 1\n')
        fake.chmod(0o755)
        env = dict(self.env, PATH=f"{self.bin_dir}:{self.env['PATH']}")
        result = self.install(env=env)
        self.assertEqual(result.returncode, 1)
        self.assertIn("fake.ts:generic-api-key:1", result.stderr)
        self.assertFalse((self.root / "factory").exists())

    def test_refuses_when_gitleaks_reports_findings(self):
        fake = self.bin_dir / "gitleaks"
        fake.write_text("#!/bin/sh\nexit 1\n")
        fake.chmod(0o755)
        env = dict(self.env, PATH=f"{self.bin_dir}:{self.env['PATH']}")
        result = self.install(env=env)
        self.assertEqual(result.returncode, 1)
        self.assertIn("refusing to install", result.stderr)
        self.assertFalse((self.root / "factory").exists())

    @unittest.skipUnless(shutil.which("gitleaks"), "gitleaks not on PATH")
    def test_real_gitleaks_scan_passes_on_clean_checkout(self):
        # Acceptance: install.sh passes its own scan with the real gitleaks and
        # the real .gitleaksignore (relative fingerprints must match).
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("refusing to install", result.stderr)

    # --- binary path --------------------------------------------------------

    def test_missing_binary_refuses_before_changing_anything(self):
        env = dict(self.env, MACHINIST_BIN=str(self.root / "nope" / "machinist"))
        result = self.install(env=env)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("does not exist or is not executable", result.stderr)
        self.assertFalse((self.root / "factory").exists())
        self.assertFalse((self.root / "etc" / "installed.json").exists())

    def test_binary_that_fails_version_refuses(self):
        broken = self.bin_dir / "broken-machinist"
        broken.write_text("#!/bin/sh\necho boom >&2\nexit 1\n")
        broken.chmod(0o755)
        env = dict(self.env, MACHINIST_BIN=str(broken))
        result = self.install(env=env)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("does not run", result.stderr)
        self.assertFalse((self.root / "factory").exists())

    def test_binary_at_configured_path_is_used_and_checked(self):
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("binary %s: machinist-test v0" % self.fake_machinist,
                      result.stdout)

    def test_install_binary_copies_and_records(self):
        src = self.root / "build" / "machinist"
        src.parent.mkdir()
        src.write_text("#!/bin/sh\n[ \"$1\" = version ] && echo built && exit 0\nexit 1\n")
        src.chmod(0o755)
        dest = self.root / "opt" / "machinist" / "bin" / "machinist"
        result = self.install("--install-binary", str(src),
                              env=dict(self.env, MACHINIST_BIN=str(dest)))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(dest.exists())
        manifest = json.loads((self.root / "etc" / "installed.json").read_text())
        self.assertEqual(manifest["binary"], str(dest))
        self.assertTrue(any(f["path"] == str(dest) for f in manifest["files"]))

    # --- config pre-flight --------------------------------------------------

    def write_config(self, name, text):
        path = self.root / "etc" / name
        path.parent.mkdir(exist_ok=True)
        path.write_text(text)
        return path

    def test_preflight_fails_on_missing_token_file_before_any_change(self):
        self.write_config("config.toml",
                          '[control_plane]\ntoken_file = "/nonexistent/token"\n')
        result = self.install()
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("token_file", result.stderr)
        self.assertIn("does not exist", result.stderr)
        self.assertIn("nothing was changed", result.stderr)
        self.assertFalse((self.root / "factory").exists())

    def test_preflight_fails_on_missing_prompt_file(self):
        self.write_config("config.toml",
                          '[commands.task-to-pr]\nprompt_file = "prompts/task.md"\n')
        result = self.install()
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("prompt_file", result.stderr)

    def test_preflight_fails_on_missing_repository_path(self):
        self.write_config("worker.toml",
                          '[repositories.proj]\npath = "/nonexistent/repo"\n')
        result = self.install()
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("[repositories.proj] path", result.stderr)

    def test_preflight_ignores_github_repository_tables(self):
        self.write_config("config.toml", '[github.repositories]\nproj = "owner/proj"\n')
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_preflight_resolves_relative_paths_against_the_config_dir(self):
        # MACH-67: a config migrated to /etc/machinist must resolve relative
        # paths against /etc/machinist, not the old location.
        token = self.root / "etc" / "server" / "worker.token"
        token.parent.mkdir(parents=True)
        token.write_text("secret")
        self.write_config("worker.toml",
                          '[control_plane]\ntoken_file = "server/worker.token"\n')
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)

        token.unlink()
        result = self.install()
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertIn("server/worker.token", result.stderr)

    def test_preflight_skipped_on_fresh_host(self):
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("nothing to check", result.stdout)

    # --- restart policy -----------------------------------------------------

    def restart_env(self):
        """Env for restart-policy tests: fake systemctl/machinist, no skip."""
        state = self.root / "systemctl-state"
        state.mkdir(exist_ok=True)
        (state / "active").write_text("")
        (state / "enabled").write_text("")
        (state / "restarts").write_text("")
        fake_systemctl = self.bin_dir / "systemctl"
        fake_systemctl.write_text(
            '#!/bin/sh\n'
            'state="%s"\n'
            'cmd="$1"; shift\n'
            'case "$cmd" in\n'
            '  daemon-reload) exit 0 ;;\n'
            '  is-active) [ "$1" = --quiet ] && shift; grep -Fqx "$1" "$state/active" 2>/dev/null ;;\n'
            '  is-enabled) [ "$1" = --quiet ] && shift; grep -Fqx "$1" "$state/enabled" 2>/dev/null ;;\n'
            '  restart) echo "$1" >> "$state/restarts" ;;\n'
            '  *) exit 0 ;;\n'
            'esac\n' % state)
        fake_systemctl.chmod(0o755)
        cli_state = self.root / "cli-state"
        cli_state.mkdir(exist_ok=True)
        (cli_state / "calls").write_text("")
        fake_cli = self.bin_dir / "machinist-cli"
        fake_cli.write_text(
            '#!/bin/sh\n'
            'case "$1" in pause|resume) echo "$1" >> "%s/calls" ;; esac\n' % cli_state)
        fake_cli.chmod(0o755)
        env = dict(self.env)
        del env["MACHINIST_SKIP_SYSTEMCTL"]
        env.update(
            MACHINIST_ASSUME_PRIVILEGED="1",
            MACHINIST_CLI=str(fake_cli),
            PATH=f"{self.bin_dir}:{self.env['PATH']}",
        )
        return env, state, cli_state

    def restart_test_config(self, lane="general-1", lock=True):
        cfg_dir = self.root / "etc"
        cfg_dir.mkdir(exist_ok=True)
        data = self.root / "lane" / lane
        data.mkdir(parents=True, exist_ok=True)
        (cfg_dir / ("worker-%s.toml" % lane)).write_text(
            'data_directory = "%s"\n' % data)
        if lock:
            (data / "lane.lock").write_text("42")

    def test_drain_restarts_busy_lane_only_after_it_drains(self):
        env, state, cli_state = self.restart_env()
        (state / "active").write_text("machinist-control.service\n"
                                      "machinist-worker@general-1.service\n")
        self.restart_test_config(lock=False)  # idle: no pause needed

        result = self.install(env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        restarts = (state / "restarts").read_text()
        self.assertIn("machinist-worker@general-1.service", restarts)
        self.assertNotIn("pause", (cli_state / "calls").read_text())

        self.restart_test_config(lock=True)  # now busy
        # Remove the installed units so the second install has something to
        # change (an idempotent rerun restarts nothing).
        shutil.rmtree(self.root / "systemd")
        env2, state2, cli_state2 = self.restart_env()
        (state2 / "active").write_text("machinist-worker@general-1.service\n")
        result = self.install("--drain-timeout", "2", env=env2)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("machinist-worker@general-1.service",
                         (state2 / "restarts").read_text())
        calls = (cli_state2 / "calls").read_text()
        self.assertIn("pause", calls)
        self.assertIn("resume", calls)
        self.assertIn("PENDING: machinist-worker@general-1.service", result.stdout)

    def test_no_restart_leaves_every_unit_alone(self):
        env, state, cli_state = self.restart_env()
        (state / "active").write_text("machinist-control.service\n")
        self.restart_test_config(lock=False)
        result = self.install("--no-restart", env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((state / "restarts").read_text(), "")
        self.assertIn("--no-restart: no unit restarted", result.stdout)

    def test_when_idle_skips_busy_lane_without_pausing(self):
        env, state, cli_state = self.restart_env()
        (state / "active").write_text("machinist-worker@general-1.service\n")
        self.restart_test_config(lock=True)
        result = self.install("--restart-lanes=when-idle", env=env)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertNotIn("machinist-worker@general-1.service",
                         (state / "restarts").read_text())
        self.assertEqual((cli_state / "calls").read_text(), "")
        self.assertIn("PENDING: machinist-worker@general-1.service", result.stdout)

    # --- unmanaged drop-ins -------------------------------------------------

    def test_unmanaged_dropin_is_reported_as_local_override(self):
        dropin = self.root / "systemd" / "machinist-worker.service.d"
        dropin.mkdir(parents=True)
        (dropin / "90-local.conf").write_text("[Service]\nEnvironment=PATH=/opt/bin\n")
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("local override (unmanaged drop-in): %s" % (dropin / "90-local.conf"),
                      result.stdout)

    def test_installs_omp_glm_pr_clean(self):
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)
        clean = self.root / "factory" / "omp-glm-pr-clean"
        self.assertTrue(clean.exists())
        self.assertTrue(os.access(clean, os.X_OK))

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
        self.assertEqual(manifest["binary"], str(self.fake_machinist))
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


if __name__ == "__main__":
    unittest.main()
