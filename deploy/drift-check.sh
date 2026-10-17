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
# Environment:
#   MACHINIST_MANIFEST       manifest written by install.sh (default /etc/machinist/installed.json)
#   MACHINIST_ALERTS_DIR     Factory Alerts spool (default $HOME/.machinist/alerts/pending)
#   MACHINIST_DRIFT_STATUS   health/status file (default $HOME/.machinist/drift.json)
#   MACHINIST_CONFIG         server config checked for [alerts] (default /etc/machinist/config.toml)
set -eu

manifest="${MACHINIST_MANIFEST:-/etc/machinist/installed.json}"
alerts_dir="${MACHINIST_ALERTS_DIR:-${MACHINIST_DATA_DIR:-$HOME/.machinist}/alerts/pending}"
status_file="${MACHINIST_DRIFT_STATUS:-${MACHINIST_DATA_DIR:-$HOME/.machinist}/drift.json}"
config_file="${MACHINIST_CONFIG:-/etc/machinist/config.toml}"

fail_setup() { printf 'drift-check: %s\n' "$*" >&2; exit 1; }

[ -r "$manifest" ] || fail_setup "manifest $manifest is missing; run deploy/install.sh first"
command -v python3 >/dev/null 2>&1 || fail_setup "python3 is required"

python3 - "$manifest" "$alerts_dir" "$status_file" "$config_file" <<'PY'
import hashlib, json, os, re, subprocess, sys, time

manifest_path, alerts_dir, status_file, config_file = sys.argv[1:5]
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

checked_at = time.strftime("%Y-%m-%dT%H:%M:%S%z")
ok = not drifted

# MACH-70: a config without [alerts] means failures alert nowhere — no file,
# no webhook. The overlay can carry the section, so both files count.
def has_alerts_section(path):
    try:
        return re.search(r"^\s*\[alerts\]", open(path).read(), re.M) is not None
    except OSError:
        return False

overlay = os.environ.get("MACHINIST_CONFIG_OVERLAY") or os.path.join(
    os.path.dirname(config_file), "config.local.toml")
alerts_configured = has_alerts_section(config_file) or has_alerts_section(overlay)

status = {"ok": ok, "sha": sha, "checked_at": checked_at, "drifted": drifted,
          "manifest": manifest_path, "alerts_configured": alerts_configured}
os.makedirs(os.path.dirname(status_file), exist_ok=True)
tmp = status_file + ".tmp"
with open(tmp, "w") as handle:
    json.dump(status, handle, indent=2, sort_keys=True)
    handle.write("\n")
os.replace(tmp, status_file)

if ok:
    print("drift-check: no drift", file=sys.stderr)
    if not alerts_configured:
        print("drift-check: WARNING: %s has no [alerts] section; failed runs "
              "alert nowhere (no spool file, no webhook) - see docs/configuration.md"
              % config_file, file=sys.stderr)
    sys.exit(0)

os.makedirs(alerts_dir, exist_ok=True)
alert = {"type": "machinist.drift", "detected_at": checked_at, "sha": sha,
         "repo": repo, "drifted": drifted,
         "status_file": status_file, "manifest": manifest_path}
alert_path = os.path.join(alerts_dir, "drift-%s.json" % time.strftime("%Y%m%dT%H%M%S"))
with open(alert_path, "w") as handle:
    json.dump(alert, handle, indent=2, sort_keys=True)
    handle.write("\n")
print("drift-check: %d drifted file(s): %s" % (len(drifted), ", ".join(d["path"] for d in drifted)),
      file=sys.stderr)
if not alerts_configured:
    print("drift-check: WARNING: %s has no [alerts] section; failed runs "
          "alert nowhere (no spool file, no webhook) - see docs/configuration.md"
          % config_file, file=sys.stderr)
print("drift-check: alert written to %s" % alert_path, file=sys.stderr)
sys.exit(2)
PY