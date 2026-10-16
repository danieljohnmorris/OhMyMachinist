"""MACH-59: restart-when-idle must not restart a lane during a job's final
steps. The lane writes <data-dir>/lane.lock while a run is in flight."""
from __future__ import annotations

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "restart-when-idle.sh"


class RestartWhenIdleTests(unittest.TestCase):
    def _run(self, root: Path, data_dir: Path) -> tuple[subprocess.CompletedProcess, Path]:
        bin_dir = root / "bin"
        bin_dir.mkdir()
        calls = root / "systemctl.calls"
        script = bin_dir / "systemctl"
        script.write_text(f'#!/bin/sh\nprintf "%s\\n" "$*" >> "{calls}"\n', encoding="utf-8")
        os.chmod(script, 0o755)
        env = os.environ.copy()
        env["PATH"] = f"{bin_dir}:{env['PATH']}"
        result = subprocess.run(
            ["bash", str(SCRIPT), "machinist-worker.service", str(data_dir)],
            capture_output=True,
            text=True,
            env=env,
        )
        return result, calls

    def test_busy_lane_is_not_restarted(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            data_dir = root / "data"
            data_dir.mkdir()
            (data_dir / "lane.lock").write_text("run_123\n", encoding="utf-8")
            result, calls = self._run(root, data_dir)
            self.assertEqual(result.returncode, 0)
            self.assertFalse(calls.exists())
            self.assertIn("lane busy", result.stderr)

    def test_idle_lane_is_restarted(self) -> None:
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            data_dir = root / "data"
            data_dir.mkdir()
            result, calls = self._run(root, data_dir)
            self.assertEqual(result.returncode, 0)
            self.assertEqual(calls.read_text(encoding="utf-8").strip(), "restart machinist-worker.service")


if __name__ == "__main__":
    unittest.main()