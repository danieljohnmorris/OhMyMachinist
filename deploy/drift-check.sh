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

# MACH-72: alert spool access for the group watcher. When the config has an
# [alerts] section, verify the group exists, the spool directories are setgid
# group-rw with default ACLs, the tmpfiles rule is installed, recent alert
# files are group-readable, and the retired root-chmod stopgap units are gone.
def unquote(value):
    value = value.strip()
    if len(value) >= 2 and value[0] == value[-1] == '"':
        return value[1:-1]
    return value

def read_alerts_settings(path):
    settings = {}
    try:
        lines = open(path).read().splitlines()
    except OSError:
        return settings
    in_alerts = False
    for line in lines:
        stripped = line.strip()
        if stripped.startswith("["):
            in_alerts = stripped == "[alerts]"
            continue
        if in_alerts and "=" in stripped:
            key, _, value = stripped.partition("=")
            settings[key.strip()] = value
    return settings

alerts_cfg = {}
for path in (overlay, config_file):  # the overlay wins
    alerts_cfg.update(read_alerts_settings(path))

def service_user_name():
    override = os.environ.get("MACHINIST_SERVICE_USER")
    if override:
        return override
    for unit_dir in (os.environ.get("MACHINIST_SYSTEMD_DIR"), "/etc/systemd/system"):
        if not unit_dir:
            continue
        try:
            for line in open(os.path.join(unit_dir, "machinist-control.service")):
                if line.strip().startswith("User="):
                    return line.split("=", 1)[1].strip() or "machinist"
        except OSError:
            continue
    return "machinist"

def check_alerts_access():
    import grp
    import pwd
    import stat as stat_module

    group = unquote(alerts_cfg.get("group", "machinist-alerts"))
    file_mode = unquote(alerts_cfg.get("file_mode", "0640"))
    directory = unquote(alerts_cfg.get("directory", "~/.machinist/alerts/pending"))
    user = service_user_name()
    tmpfiles_conf = os.path.join(
        os.environ.get("MACHINIST_TMPFILES_DIR", "/etc/tmpfiles.d"),
        "machinist-alerts.conf")

    try:
        entry = grp.getgrnam(group)
    except KeyError:
        return False, ["alert group %s does not exist" % group], None

    if directory.startswith("~"):
        try:
            directory = pwd.getpwnam(user).pw_dir + directory[1:]
        except KeyError:
            return False, ["cannot resolve home of service user %s" % user], None
    spool = directory.rstrip("/")
    root = os.path.dirname(spool)
    if os.path.basename(spool) == "pending":
        dirs = [root, spool, os.path.join(root, "done")]
    else:
        dirs = [root, os.path.join(spool, "pending"), os.path.join(spool, "done")]

    try:
        uid = pwd.getpwnam(user).pw_uid
    except KeyError:
        uid = None
    reasons = []
    for directory in dirs:
        try:
            info = os.stat(directory)
        except OSError:
            reasons.append("alert directory %s is missing" % directory)
            continue
        if uid is not None and info.st_uid != uid:
            reasons.append("alert directory %s is owned by uid %d, want %s"
                           % (directory, info.st_uid, user))
        if info.st_gid != entry.gr_gid:
            reasons.append("alert directory %s group is gid %d, want %s"
                           % (directory, info.st_gid, group))
        if stat_module.S_IMODE(info.st_mode) != 0o2770:
            reasons.append("alert directory %s mode is %04o, want setgid 2770"
                           % (directory, stat_module.S_IMODE(info.st_mode)))
        getfacl = subprocess.run(
            ["getfacl", "-p", "--absolute-names", directory], capture_output=True)
        if getfacl.returncode == 0:
            text = getfacl.stdout.decode()
            if "default:group:%s:rwx" % group not in text:
                reasons.append("alert directory %s lacks the default ACL for group %s"
                               % (directory, group))

    if os.path.isfile(tmpfiles_conf):
        body = open(tmpfiles_conf).read()
        for directory in dirs:
            if ("d %s 2770 %s %s" % (directory, user, group)) not in body:
                reasons.append("tmpfiles rule for %s missing from %s"
                               % (directory, tmpfiles_conf))
    else:
        reasons.append("tmpfiles rule %s is not installed" % tmpfiles_conf)

    try:
        wanted_group_read = bool(int(file_mode, 8) & 0o040)
    except ValueError:
        wanted_group_read = True
    if os.path.isdir(spool):
        pending_files = sorted(
            (os.path.join(spool, name) for name in os.listdir(spool)
             if name.endswith(".json") and not name.startswith(".")),
            key=lambda path: os.stat(path).st_mtime)[-5:]
        for path in pending_files:
            if wanted_group_read and not os.stat(path).st_mode & 0o040:
                reasons.append("recent alert file %s is not group-readable (file_mode %s)"
                               % (path, file_mode))

    systemd_dir = os.environ.get("MACHINIST_SYSTEMD_DIR", "/etc/systemd/system")
    for unit in ("machinist-alerts-perms.path", "machinist-alerts-perms.service"):
        if os.path.exists(os.path.join(systemd_dir, unit)):
            reasons.append("stopgap unit %s is still installed; remove it" % unit)

    return not reasons, reasons, file_mode

alerts_access_ok, alerts_access_reasons, alerts_file_mode = True, [], None
if alerts_configured:
    alerts_access_ok, alerts_access_reasons, alerts_file_mode = check_alerts_access()

checked_at = time.strftime("%Y-%m-%dT%H:%M:%S%z")
ok = not drifted and alerts_access_ok

status = {"ok": ok, "sha": sha, "checked_at": checked_at, "drifted": drifted,
          "manifest": manifest_path, "alerts_configured": alerts_configured,
          "alerts_access": {"ok": alerts_access_ok, "reasons": alerts_access_reasons}}
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
         "alerts_access": {"ok": alerts_access_ok, "reasons": alerts_access_reasons},
         "status_file": status_file, "manifest": manifest_path}
alert_path = os.path.join(alerts_dir, "drift-%s.json" % time.strftime("%Y%m%dT%H%M%S"))
with open(alert_path, "w") as handle:
    json.dump(alert, handle, indent=2, sort_keys=True)
    handle.write("\n")
# The group watcher reads and moves spooled alerts without root (MACH-72).
if alerts_file_mode:
    try:
        os.chmod(alert_path, int(alerts_file_mode, 8))
    except ValueError:
        pass
print("drift-check: %d drifted file(s): %s" % (len(drifted), ", ".join(d["path"] for d in drifted)),
      file=sys.stderr)
if alerts_access_reasons:
    print("drift-check: alerts access drift: %s" % "; ".join(alerts_access_reasons),
          file=sys.stderr)
if not alerts_configured:
    print("drift-check: WARNING: %s has no [alerts] section; failed runs "
          "alert nowhere (no spool file, no webhook) - see docs/configuration.md"
          % config_file, file=sys.stderr)
print("drift-check: alert written to %s" % alert_path, file=sys.stderr)
sys.exit(2)
PY