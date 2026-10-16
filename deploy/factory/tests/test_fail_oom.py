"""Tests for OOM classification, the OOM Plane comment and stale-inbox detection
(MACH-62, deploy/factory/fail.py)."""
from __future__ import annotations

import importlib.util
import io
import json
import os
from pathlib import Path
import sqlite3
import tempfile
import time
import unittest
from unittest import mock

_SPEC = Path(__file__).resolve().parents[1] / "fail.py"
_spec = importlib.util.spec_from_file_location("fail_hook", _SPEC)
fail = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(fail)


class ClassifyOomTests(unittest.TestCase):
    def test_out_of_memory_is_oom(self) -> None:
        self.assertEqual(fail.classify("command failed: out of memory in build-pr, peak 12.0 GB"), "oom")
        self.assertEqual(fail.classify("Out Of Memory"), "oom")

    def test_oom_kill_record_is_oom(self) -> None:
        for text in ("oom_kill 3", "OOMKilled", "memory.events: oom_kill 1", "killed process 1234 (omp)"):
            with self.subTest(text=text):
                self.assertEqual(fail.classify(text), "oom")

    def test_oom_is_checked_before_transient(self) -> None:
        names = [name for name, _ in fail.PATTERNS]
        self.assertLess(names.index("oom"), names.index("transient"))
        # A killed-by-signal line (transient) alongside an OOM record classifies as oom.
        self.assertEqual(fail.classify("killed by signal 9\nmemory.events: oom_kill 1"), "oom")

    def test_transient_still_matches_transient(self) -> None:
        self.assertEqual(fail.classify("fatal: connection reset by peer"), "transient")

    def test_oom_is_never_auto_requeued(self) -> None:
        self.assertNotEqual(fail.classify("out of memory"), "transient")


class OomSummaryTests(unittest.TestCase):
    def test_summary_names_command_and_peak(self) -> None:
        alert = {"command": "build-pr", "peak_memory_bytes": 11 * 1024 ** 3, "plane": None}
        self.assertEqual(fail.oom_summary(alert), "out of memory in build-pr, peak 11.0 GB")

    def test_summary_prefers_plane_command_and_tolerates_missing_peak(self) -> None:
        self.assertEqual(fail.oom_summary({"plane": {"command": "build-pr"}}), "out of memory in build-pr")

    def test_summary_without_command_or_peak(self) -> None:
        self.assertEqual(fail.oom_summary({}), "out of memory in the job")


class BuildAlertOomFieldsTests(unittest.TestCase):
    def _db(self, d: str, *, oom_columns: bool) -> str:
        db = str(Path(d) / "machinist.db")
        con = sqlite3.connect(db)
        con.execute("create table runs (id text primary key, job_id text, repository text, command text)")
        con.execute("insert into runs values ('r1','j1','OhMyMachinist','build-pr')")
        if oom_columns:
            con.execute("alter table runs add column error_class text")
            con.execute("alter table runs add column peak_memory_bytes integer")
            con.execute("update runs set error_class='oom', peak_memory_bytes=12884901888")
        con.commit()
        con.close()
        return db

    def test_build_alert_tolerates_db_without_oom_columns(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            db = self._db(d, oom_columns=False)
            with mock.patch.object(fail, "DB", db), mock.patch.object(fail, "JOBS_DIR", str(Path(d) / "jobs")):
                alert = fail.build_alert("r1", 137, "run", ["out of memory in build-pr, peak 12.0 GB"])
            self.assertEqual(alert["classification"], "oom")
            self.assertEqual(alert["command"], "build-pr")
            self.assertIsNone(alert["error_class"])
            self.assertIsNone(alert["peak_memory_bytes"])
            self.assertFalse(alert["auto_requeue_allowed"])

    def test_build_alert_reads_oom_columns_when_present(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            db = self._db(d, oom_columns=True)
            with mock.patch.object(fail, "DB", db), mock.patch.object(fail, "JOBS_DIR", str(Path(d) / "jobs")):
                alert = fail.build_alert("r1", 137, "run", ["oom_kill 1"])
            self.assertEqual(alert["error_class"], "oom")
            self.assertEqual(alert["peak_memory_bytes"], 12884901888)
            self.assertEqual(fail.oom_summary(alert), "out of memory in build-pr, peak 12.0 GB")

    def test_row_opt_returns_none_for_missing_column(self) -> None:
        con = sqlite3.connect(":memory:")
        con.row_factory = sqlite3.Row
        con.execute("create table runs (id text, state text)")
        con.execute("insert into runs values ('r1','failed')")
        row = con.execute("select * from runs").fetchone()
        con.close()
        self.assertEqual(fail.row_opt(row, "state"), "failed")
        self.assertIsNone(fail.row_opt(row, "peak_memory_bytes"))
        self.assertIsNone(fail.row_opt(None, "state"))


class StaleInboxTests(unittest.TestCase):
    def test_inbox_stale_age_check(self) -> None:
        now = 1_000.0
        self.assertTrue(fail.inbox_stale(now - 400, now, 300))
        self.assertFalse(fail.inbox_stale(now - 10, now, 300))
        self.assertFalse(fail.inbox_stale(now - 300, now, 300))

    def test_warn_stale_inbox_flags_old_and_ignores_fresh(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            old, fresh = Path(d) / "old.json", Path(d) / "fresh.json"
            old.write_text("{}")
            fresh.write_text("{}")
            now = time.time()
            os.utime(old, (now - 3600, now - 3600))
            os.utime(fresh, (now, now))
            err = io.StringIO()
            with mock.patch.object(fail, "INBOX", d), \
                    mock.patch.object(fail, "FAIL_INBOX_WARN_S", 300), \
                    mock.patch("sys.stderr", err):
                fail.warn_stale_inbox()
            logged = err.getvalue()
            self.assertIn("old.json", logged)
            self.assertIn("stale", logged)
            self.assertNotIn("fresh.json", logged)

    def test_cmd_notify_drains_the_whole_inbox_then_warns(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            inbox, pending = Path(d) / "inbox", Path(d) / "pending"
            inbox.mkdir(); pending.mkdir()
            alert = {"classification": "oom", "stderr_tail": [], "plane": None, "stage": "run",
                     "exit_code": 137, "job_id": "MACH-1", "command": "build-pr",
                     "peak_memory_bytes": 12 * 1024 ** 3}
            (inbox / "MACH-1.json").write_text(json.dumps(alert))
            with mock.patch.object(fail, "INBOX", str(inbox)), \
                    mock.patch.object(fail, "PENDING", str(pending)), \
                    mock.patch.object(fail, "STATE", str(Path(d) / "state.json")), \
                    mock.patch.object(fail, "warn_stale_inbox") as warn, \
                    mock.patch("sys.stderr", io.StringIO()):
                fail.cmd_notify(None)
            self.assertTrue((pending / "MACH-1.json").exists())
            self.assertFalse((inbox / "MACH-1.json").exists())
            warn.assert_called_once_with()


if __name__ == "__main__":
    unittest.main()