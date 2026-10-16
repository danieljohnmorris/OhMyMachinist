#!/usr/bin/env bash
# Machinist drift check (MACH-34).
#
# Diffs every file the installer manages against the repository at the deployed
# git SHA and reports the ones that drifted. The config overlay
# (/etc/machinist/config.local.toml, worker.local.toml) and the secret env files
# (/etc/machinist/*.env) are never part of the manifest, so the drift check never
# reads or diffs them.
#
#   drift-check.sh
#
# Exits 0 when everything matches, 2 when drift is found (an alert is written
# to the Factory Alerts spool and a health/status file is updated), and 1 on a
# setup error (no manifest, unreadable manifest).
#
# The status file and any alert also carry two install-time reports: the
# unmanaged drop-ins found in the managed unit directories ("local overrides")
# and the worker lanes still running a binary or unit older than the installed
# one ("stale workers", possible after a drain-limited install). They are
# reported but do not change the exit code.
#
# Environment:
#   MACHINIST_MANIFEST       manifest written by install.sh (default /etc/machinist/installed.json)
#   MACHINIST_ALERTS_DIR     Factory Alerts spool (default $HOME/.machinist/alerts/pending)
#   MACHINIST_DRIFT_STATUS   health/status file (default $HOME/.machinist/drift.json)
set -eu

manifest="${MACHINIST_MANIFEST:-/etc/machinist/installed.json}"
alerts_dir="${MACHINIST_ALERTS_DIR:-${MACHINIST_DATA_DIR:-$HOME/.machinist}/alerts/pending}"
status_file="${MACHINIST_DRIFT_STATUS:-${MACHINIST_DATA_DIR:-$HOME/.machinist}/drift.json}"

fail_setup() { printf 'drift-check: %s\n' "$*" >&2; exit 1; }

[ -r "$manifest" ] || fail_setup "manifest $manifest is missing; run deploy/install.sh first"
command -v python3 >/dev/null 2>&1 || fail_setup "python3 is required"

python3 - "$manifest" "$alerts_dir" "$status_file" <<'PY'
import hashlib, json, os, subprocess, sys, time

manifest_path, alerts_dir, status_file = sys.argv[1:4]
try:
    manifest = json.load(open(manifest_path))
except (OSError, ValueError) as err:
    sys.exit(f"drift-check: cannot read manifest {manifest_path}: {err}")

sha = manifest.get("sha") or ""
repo = manifest.get("repo") or ""
files = manifest.get("files") or []

def sha256_file(path):
    try:
        with open(path, "rb") as handle:
            return hashlib.sha256(handle.read()).hexdigest()
    except OSError:
        return None

def repo_blob_sha(source):
    """sha256 of the repo file at the deployed SHA, or None when unavailable."""
    if not (sha and repo and source and os.path.isdir(repo)):
        return None
    result = subprocess.run(["git", "-C", repo, "show", f"{sha}:{source}"],
                            capture_output=True)
    if result.returncode != 0:
        return None
    return hashlib.sha256(result.stdout).hexdigest()

drifted = []
for entry in files:
    dest = entry.get("path", "")
    source = entry.get("source", "")
    recorded = entry.get("sha256", "")
    actual = sha256_file(dest)
    reasons = []
    if actual is None:
        reasons.append("missing")
    elif recorded and actual != recorded:
        reasons.append("edited on host")
    blob_sha = repo_blob_sha(source)
    if blob_sha and actual is not None and blob_sha != actual:
        reasons.append(f"differs from repo@{sha[:12]}")
    if reasons:
        drifted.append({"path": dest, "source": source, "reasons": reasons})

# --- local overrides ---------------------------------------------------------
# Drop-ins in a managed unit's directory that the installer does not manage (a
# hand-made 90-local.conf, for example) would otherwise be invisible. List
# them so they stay in sync and are visible; move them into the repo when they
# are wanted for good.
managed_paths = {entry.get("path", "") for entry in files}
dropin_dirs = set()
for entry in files:
    dest = entry.get("path", "")
    if dest.endswith((".service", ".timer", ".path", ".target")):
        dropin_dirs.add(dest + ".d")
    elif dest.endswith(".conf"):
        parent = os.path.dirname(dest)
        if parent.endswith(".d"):
            dropin_dirs.add(parent)
local_overrides = []
for directory in sorted(dropin_dirs):
    try:
        names = sorted(os.listdir(directory))
    except OSError:
        continue
    for name in names:
        if name.endswith(".conf"):
            path = os.path.join(directory, name)
            if path not in managed_paths:
                local_overrides.append(path)

# --- stale workers -------------------------------------------------------------
# A worker whose process started before the binary or its unit file was last
# updated is still running old code: the installer only restarts drained lanes
# and reports the rest. Compare each running worker's start time (the mtime of
# /proc/<pid>) against the recorded binary and the installed unit files.
def worker_units():
    units = []
    for entry in files:
        dest = entry.get("path", "")
        base = os.path.basename(dest)
        if base.endswith(".service") and base.startswith("machinist-worker"):
            units.append((base, dest))
    return units


def unit_pids(unit):
    try:
        out = subprocess.run(["systemctl", "show", unit, "--property", "MainPID",
                              "--value"], capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.SubprocessError):
        return []
    if out.returncode != 0:
        return []
    pid = out.stdout.strip()
    return [int(pid)] if pid.isdigit() and int(pid) > 0 else []


def worker_instances():
    try:
        out = subprocess.run(["systemctl", "list-units", "--plain", "--no-legend",
                              "--state=active", "machinist-worker@*"],
                             capture_output=True, text=True, timeout=10)
    except (OSError, subprocess.SubprocessError):
        return []
    return [line.split()[0] for line in out.stdout.splitlines() if line.strip()]


stale_workers = []
binary = manifest.get("binary") or {}
binary_mtime = int(binary.get("mtime") or 0)
for unit, dest in worker_units():
    instances = worker_instances() if unit.endswith("@.service") else [unit]
    for instance in instances:
        for pid in unit_pids(instance):
            try:
                started = os.stat("/proc/%d" % pid).st_mtime
            except OSError:
                continue
            reasons = []
            if binary_mtime and binary_mtime > started:
                reasons.append("binary updated after start")
            try:
                if os.path.getmtime(dest) > started:
                    reasons.append("unit updated after start")
            except OSError:
                pass
            if reasons:
                stale_workers.append({"unit": instance, "pid": pid, "reasons": reasons})

checked_at = time.strftime("%Y-%m-%dT%H:%M:%S%z")
ok = not drifted
status = {"ok": ok, "sha": sha, "checked_at": checked_at, "drifted": drifted,
          "local_overrides": local_overrides, "stale_workers": stale_workers,
          "manifest": manifest_path}
os.makedirs(os.path.dirname(status_file), exist_ok=True)
tmp = status_file + ".tmp"
with open(tmp, "w") as handle:
    json.dump(status, handle, indent=2, sort_keys=True)
    handle.write("\n")
os.replace(tmp, status_file)

if ok:
    print("drift-check: no drift", file=sys.stderr)
    for path in local_overrides:
        print("drift-check: local override (not repo-managed): %s" % path, file=sys.stderr)
    for worker in stale_workers:
        print("drift-check: stale worker %s (pid %s): %s"
              % (worker["unit"], worker["pid"], "; ".join(worker["reasons"])), file=sys.stderr)
    sys.exit(0)

os.makedirs(alerts_dir, exist_ok=True)
alert = {"type": "machinist.drift", "detected_at": checked_at, "sha": sha,
         "repo": repo, "drifted": drifted, "local_overrides": local_overrides,
         "stale_workers": stale_workers,
         "status_file": status_file, "manifest": manifest_path}
alert_path = os.path.join(alerts_dir, "drift-%s.json" % time.strftime("%Y%m%dT%H%M%S"))
with open(alert_path, "w") as handle:
    json.dump(alert, handle, indent=2, sort_keys=True)
    handle.write("\n")
print("drift-check: %d drifted file(s): %s" % (len(drifted), ", ".join(d["path"] for d in drifted)),
      file=sys.stderr)
print("drift-check: alert written to %s" % alert_path, file=sys.stderr)
sys.exit(2)
PY