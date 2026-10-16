#!/usr/bin/env bash
# Machinist factory installer (MACH-34).
#
# Idempotent install and upgrade: copies the factory scripts, the systemd units
# and drop-ins, and the config/env templates from this repository onto the
# host, then reloads systemd and restarts only the services whose unit files
# changed. Every file it manages is recorded with its sha256 and the deployed
# git SHA in /etc/machinist/installed.json, which deploy/drift-check.sh diffs
# against the repository.
#
#   deploy/install.sh [--dry-run] [--repo <path>] [--binary <path>]
#                     [--install-binary <src>] [--no-restart]
#                     [--restart-lanes=when-idle] [--drain-timeout <seconds>]
#
# The binary path defaults to $MACHINIST_BIN, then /usr/local/bin/machinist.
# install.sh checks that it exists and runs `version` before it touches any
# unit; --install-binary copies a built binary to the path first, and a binary
# living outside /usr/local/bin is linked there so the units find it.
#
# Restarts drain by default: worker lanes are only restarted once no job is
# running (claims are paused with the factory control, PR #54, and resumed
# afterwards); lanes still busy when the drain timeout expires are reported as
# pending. --restart-lanes=when-idle restores the old restart-if-idle
# behaviour and --no-restart leaves every unit alone.
#
# The installer never overwrites the local config overlay
# (/etc/machinist/config.local.toml, worker.local.toml) or the secret env files
# (/etc/machinist/*.env): it only writes the tracked config/env templates as
# <name>.example and seeds config.toml/worker.toml the first time. It refuses
# to install from a tree that a gitleaks scan reports as containing secrets.
set -euo pipefail

dry_run=0
repo=""
bin_path="${MACHINIST_BIN:-/usr/local/bin/machinist}"
binary_src=""
restart_lanes="drain"
drain_timeout="${MACHINIST_DRAIN_TIMEOUT:-600}"
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) dry_run=1 ;;
    --repo) shift; repo="${1:?--repo needs a path}" ;;
    --binary) shift; bin_path="${1:?--binary needs a path}" ;;
    --install-binary) shift; binary_src="${1:?--install-binary needs a path}" ;;
    --no-restart) restart_lanes="no" ;;
    --restart-lanes=when-idle) restart_lanes="when-idle" ;;
    --restart-lanes=drain) restart_lanes="drain" ;;
    --drain-timeout) shift; drain_timeout="${1:?--drain-timeout needs seconds}" ;;
    -h|--help) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) printf 'install.sh: unknown argument %s\n' "$1" >&2; exit 2 ;;
  esac
  shift
done

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="${repo:-$(cd "$script_dir/.." && pwd)}"
repo="$(cd "$repo" && pwd)"

factory_dir="${MACHINIST_FACTORY_DIR:-/usr/local/lib/machinist-factory}"
systemd_dir="${MACHINIST_SYSTEMD_DIR:-/etc/systemd/system}"
config_dir="${MACHINIST_CONFIG_DIR:-/etc/machinist}"
manifest="${MACHINIST_MANIFEST:-$config_dir/installed.json}"
machinist_user="${MACHINIST_USER:-machinist}"
machinist_home="${MACHINIST_HOME:-$(getent passwd "$machinist_user" 2>/dev/null | cut -d: -f6)}"
machinist_home="${machinist_home:-$HOME}"
systemctl_cmd="${MACHINIST_SYSTEMCTL:-systemctl}"

run() { if [ "$dry_run" = 1 ]; then printf '  dry-run: %s\n' "$*"; else "$@"; fi; }

log() { printf 'install.sh: %s\n' "$*"; }

# --- secret scan -----------------------------------------------------------
# Refuse to install from a tree with secret findings, so a leaked credential
# can never be copied onto the host. The scan runs from inside the repository
# with a relative source: gitleaks then writes fingerprints relative to it
# (<file>:<rule>:<line>), which is the form .gitleaksignore allowlists. An
# absolute --source writes /abs/path/file:... fingerprints that never match.
if command -v gitleaks >/dev/null 2>&1; then
  log "scanning $repo for secrets with gitleaks"
  if ! scan_output="$(cd "$repo" && gitleaks detect --source . --no-git --redact 2>&1)"; then
    printf '%s\n' "$scan_output" >&2
    printf 'install.sh: gitleaks found secrets under %s; refusing to install\n' "$repo" >&2
    exit 1
  fi
else
  log "gitleaks not found; skipping the pre-install secret scan"
fi

[ -f "$repo/deploy/factory/deliver-pr.py" ] || { printf 'install.sh: %s does not look like the OhMyMachinist repository\n' "$repo" >&2; exit 1; }

sha="unknown"
if command -v git >/dev/null 2>&1; then
  sha="$(git -C "$repo" rev-parse HEAD 2>/dev/null || echo unknown)"
  if [ -n "$(git -C "$repo" status --porcelain 2>/dev/null)" ]; then
    log "warning: $repo has uncommitted changes; drift-check compares installed files against commit $sha"
  fi
fi

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else printf 'install.sh: sha256sum or shasum is required\n' >&2; exit 1; fi
}

# --- machinist binary -------------------------------------------------------
# The units run a hardcoded path and fail.py defaults to it, so the binary is
# checked (and optionally installed) before anything is copied. A binary that
# lives elsewhere is linked into /usr/local/bin so the units find it.
if [ -n "$binary_src" ]; then
  log "install binary $binary_src -> $bin_path"
  run install -D -m 0755 "$binary_src" "$bin_path"
fi
if [ -n "$binary_src" ] && [ ! -x "$bin_path" ] && [ "$dry_run" = 1 ]; then
  log "dry-run: binary would be installed to $bin_path; skipping the version check"
elif [ ! -x "$bin_path" ]; then
  printf 'install.sh: machinist binary not found at %s; set MACHINIST_BIN or pass --binary <path> (or --install-binary <src>); refusing to touch any unit\n' "$bin_path" >&2
  exit 1
elif ! "$bin_path" version >/dev/null 2>&1; then
  printf 'install.sh: %s does not run "version"; refusing to touch any unit\n' "$bin_path" >&2
  exit 1
else
  log "binary ok: $bin_path ($("$bin_path" version 2>/dev/null | head -1))"
fi
if [ "$bin_path" != /usr/local/bin/machinist ] && [ ! -e /usr/local/bin/machinist ]; then
  if [ "$(id -u)" = 0 ]; then
    log "link /usr/local/bin/machinist -> $bin_path (the units and fail.py use the hardcoded path)"
    run ln -s "$bin_path" /usr/local/bin/machinist
  else
    log "warning: not root; cannot link /usr/local/bin/machinist -> $bin_path, so the units will not find the binary"
  fi
fi

# --- config pre-flight ------------------------------------------------------
# Relative paths in a config resolve against the config file's directory, so
# configs migrated from ~/.machinist to /etc/machinist silently point their
# token_file/prompt_file/repository paths under /etc/machinist and the workers
# fail to start. Before any unit is changed, load every config the units will
# use from its new location and check that every referenced file exists. Only
# configs that already existed are checked; freshly seeded templates are left
# to first-time setup (the token is created with `machinist tokens create`).
preflight_configs=()
for name in config.toml config.local.toml worker.toml worker.local.toml; do
  if [ -f "$config_dir/$name" ]; then preflight_configs+=("$name"); fi
done
for cfg in "$config_dir"/worker-*.toml; do
  if [ -e "$cfg" ]; then preflight_configs+=("$(basename "$cfg")"); fi
done
if [ "${#preflight_configs[@]}" -gt 0 ]; then
  log "pre-flight: checking ${#preflight_configs[@]} config(s) in $config_dir"
  if ! python3 - "$config_dir" "$machinist_home" "${preflight_configs[@]}" <<'PY'
import os, re, sys

config_dir, home = sys.argv[1], sys.argv[2]
configs = sys.argv[3:]
key_re = re.compile(r'^(token_file|worker_token_file|prompt_file|path)\s*=\s*"([^"]+)"')
problems = []
for name in configs:
    path = os.path.join(config_dir, name)
    table = ""
    with open(path, encoding="utf-8") as handle:
        for lineno, raw in enumerate(handle, 1):
            line = raw.split("#", 1)[0].strip()
            header = re.match(r"^\[([^\]]+)\]$", line)
            if header:
                table = header.group(1).strip()
                continue
            match = key_re.match(line)
            if not match:
                continue
            key, value = match.groups()
            if key == "path" and not table.startswith("repositories"):
                continue  # repositories.<name>.path is the only checked "path"
            if value.startswith("~"):
                resolved = os.path.join(home, value[2:]) if value.startswith("~/") else home
            elif os.path.isabs(value):
                resolved = value
            else:
                resolved = os.path.join(config_dir, value)
            if not os.path.exists(resolved):
                problems.append(
                    "%s:%d: %s = %r resolves to %s, which does not exist"
                    % (path, lineno, key, value, resolved)
                )
if problems:
    sys.stderr.write(
        "install.sh: config pre-flight failed; relative paths in a config"
        " resolve against the config file's directory, so rewrite them as"
        " absolute or ~/ paths when a config moves to %s"
        " (see deploy/README.md):\n" % config_dir
    )
    for problem in problems:
        sys.stderr.write("  %s\n" % problem)
    sys.exit(1)
PY
  then
    exit 1
  fi
fi

# changed_units collects systemd units whose file content changed, so only the
# owning services are restarted afterwards.
changed_units=()
declare -A changed_set=()

mark_changed() { # <unit>
  local unit="$1"
  [ -n "${changed_set[$unit]:-}" ] && return 0
  changed_set[$unit]=1
  changed_units+=("$unit")
}

manifest_entries=()
declare -A managed_dests=()

install_file() { # <src> <dest>
  local src="$1" dest="$2" mode
  mode=0644
  [ -x "$src" ] && mode=0755
  managed_dests["$dest"]=1
  local src_sum dest_sum=""
  src_sum="$(sha256_of "$src")"
  [ -f "$dest" ] && dest_sum="$(sha256_of "$dest")"
  if [ "$src_sum" != "$dest_sum" ]; then
    log "install $dest"
    if [ "$dry_run" = 1 ]; then
      printf '  dry-run: install -m%s %s %s\n' "$mode" "$src" "$dest"
    else
      install -D -m "$mode" "$src" "$dest"
    fi
    case "$dest" in
      "$systemd_dir"/*)
        local rel="${dest#"$systemd_dir"/}" unit
        case "$rel" in
          */*) unit="$(basename "$(dirname "$rel")")"; unit="${unit%.d}" ;;
          *) unit="$rel" ;;
        esac
        mark_changed "$unit"
        ;;
    esac
  fi
  manifest_entries+=("$dest|${src#"$repo"/}|$src_sum")
}

# --- factory scripts -------------------------------------------------------
mapfile -t factory_files < <(
  cd "$repo" && find deploy/factory -type f \
    ! -path '*/__pycache__/*' ! -name '*.pyc' ! -path '*/tests/*' | sort
)
for src in "${factory_files[@]}"; do
  install_file "$repo/$src" "$factory_dir/${src#deploy/factory/}"
done
install_file "$repo/deploy/drift-check.sh" "$factory_dir/drift-check.sh"

# --- systemd units and drop-ins -------------------------------------------
mapfile -t unit_files < <(
  cd "$repo" && find deploy/systemd -type f | sort
)
for src in "${unit_files[@]}"; do
  install_file "$repo/$src" "$systemd_dir/${src#deploy/systemd/}"
done

# --- unmanaged drop-ins (local overrides) -----------------------------------
# Drop-ins that sit in a managed unit's directory but are not installed by the
# repo (a hand-made 90-local.conf, for example) are invisible to the drift
# check. List them so they stay visible and can move into the repo.
local_overrides=()
for src in "${unit_files[@]}"; do
  rel="${src#deploy/systemd/}"
  case "$rel" in
    */*) dropin_dir="$systemd_dir/$(dirname "$rel")" ;;
    *)   dropin_dir="$systemd_dir/$rel.d" ;;
  esac
  [ -d "$dropin_dir" ] || continue
  for conf in "$dropin_dir"/*.conf; do
    if [ -f "$conf" ] && [ -z "${managed_dests[$conf]:-}" ]; then
      log "local override (not repo-managed): $conf"
      local_overrides+=("$conf")
    fi
  done
done
if [ "${#local_overrides[@]}" -gt 0 ]; then
  log "warning: ${#local_overrides[@]} unmanaged drop-in(s); the drift check cannot see them"
fi

# --- config and env templates ---------------------------------------------
# Templates are always refreshed; the seeded configs and the env files are only
# written when absent so an existing instance is never overwritten.
for src in "$repo"/deploy/config/*.example; do
  install_file "$src" "$config_dir/$(basename "$src")"
done
for src in "$repo"/deploy/env/*.example; do
  install_file "$src" "$config_dir/$(basename "$src")"
done

seed_if_absent() { # <example> <dest>
  local example="$1" dest="$2"
  if [ -e "$dest" ]; then
    log "keep existing $dest"
    return 0
  fi
  log "seed $dest from $(basename "$example")"
  if [ "$dry_run" = 1 ]; then
    printf '  dry-run: install -m0644 %s %s\n' "$example" "$dest"
  else
    install -D -m 0644 "$example" "$dest"
  fi
}
seed_if_absent "$config_dir/config.toml.example" "$config_dir/config.toml"
seed_if_absent "$config_dir/worker.toml.example" "$config_dir/worker.toml"

# --- manifest --------------------------------------------------------------
binary_json=""
if [ -x "$bin_path" ]; then
  binary_mtime="$(stat -c %Y "$bin_path" 2>/dev/null || stat -f %m "$bin_path" 2>/dev/null || echo 0)"
  binary_json=$(printf '"binary": {"path": "%s", "sha256": "%s", "mtime": %s},\n  ' \
    "$bin_path" "$(sha256_of "$bin_path")" "$binary_mtime")
fi
if [ "$dry_run" = 1 ]; then
  log "dry-run: would record ${#manifest_entries[@]} files and SHA $sha in $manifest"
else
  mkdir -p "$(dirname "$manifest")"
  {
    printf '{\n  %s"sha": "%s",\n  "repo": "%s",\n  "installed_at": "%s",\n  "files": [\n' \
      "$binary_json" "$sha" "$repo" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    first=1
    for entry in "${manifest_entries[@]}"; do
      IFS='|' read -r dest source sum <<<"$entry"
      [ "$first" = 1 ] || printf ',\n'
      first=0
      printf '    {"path": "%s", "source": "%s", "sha256": "%s"}' "$dest" "$source" "$sum"
    done
    printf '\n  ]\n}\n'
  } >"$manifest"
  printf '%s\n' "$sha" >"$config_dir/deployed.sha"
  log "recorded ${#manifest_entries[@]} files and SHA $sha in $manifest"
fi

# --- reload and restart changed services ----------------------------------
# Control plane, fail-notify and the other non-worker units restart straight
# away. Worker lanes drain first (default): lane claims are paused with the
# factory control (PR #54), each lane is restarted once no job is running, and
# dispatch is resumed afterwards. A lane still busy when the drain timeout
# expires keeps its job and is reported as pending, still running old code.
# --restart-lanes=when-idle restores the old restart-only-if-idle behaviour
# and --no-restart leaves every unit alone.
declare -a pending_lanes=()

machinist_ctl() { # <args...> machinist CLI against the installed control-plane config
  if [ "$(id -u)" = 0 ] && command -v runuser >/dev/null 2>&1 && id "$machinist_user" >/dev/null 2>&1; then
    runuser -u "$machinist_user" -- env HOME="$machinist_home" "$bin_path" --config "$config_dir/config.toml" "$@"
  else
    env HOME="$machinist_home" "$bin_path" --config "$config_dir/config.toml" "$@"
  fi
}

ctl() { "$systemctl_cmd" "$@"; }

unit_active() { ctl is-active --quiet "$1" 2>/dev/null; }

restart_unit() { # <unit>
  log "restart $1"
  run "$systemctl_cmd" restart "$1"
}

report_pending() { # <unit>
  log "lane $1 still running a job; NOT restarted (still running old code)"
  pending_lanes+=("$1")
}

# collect_worker_targets <array-name> <unit...> expands worker units into the
# instances that are actually running (machinist-worker@.service changes mean
# every active lane, not the template itself).
collect_worker_targets() {
  local -n __worker_targets="$1"
  shift
  local unit instance
  for unit in "$@"; do
    case "$unit" in
      machinist-worker@*)
        while IFS= read -r instance; do
          [ -n "$instance" ] && __worker_targets+=("$instance")
        done < <(ctl list-units --plain --no-legend --state=active 'machinist-worker@*' 2>/dev/null | awk '{print $1}')
        ;;
      machinist-worker*)
        if unit_active "$unit"; then __worker_targets+=("$unit"); fi
        ;;
    esac
  done
}

# lane_data_dir <unit> prints the worker's DataDirectory, read from its config
# (and the local overlay), expanded the way the worker expands it: ~ against
# the machinist user's home, relative paths against the config's directory.
lane_data_dir() {
  local unit="$1" lane="" cfg
  case "$unit" in
    machinist-worker@*) lane="${unit#machinist-worker@}"; lane="${lane%.service}" ;;
  esac
  if [ -n "$lane" ]; then cfg="$config_dir/worker-$lane.toml"; else cfg="$config_dir/worker.toml"; fi
  python3 - "$cfg" "$config_dir/worker.local.toml" "$machinist_home" <<'PY'
import os, re, sys

cfg, overlay, home = sys.argv[1:4]
value = None
for path in (cfg, overlay):
    try:
        lines = open(path, encoding="utf-8").read().splitlines()
    except OSError:
        continue
    for line in lines:
        line = line.split("#", 1)[0].strip()
        match = re.match(r'data_directory\s*=\s*"([^"]+)"', line)
        if match:
            value = match.group(1)
if not value:
    value = "~/.machinist/worker"
if value.startswith("~/"):
    value = os.path.join(home, value[2:])
elif value == "~":
    value = home
elif not value.startswith("/"):
    value = os.path.join(os.path.dirname(cfg) or "/", value)
print(value)
PY
}

lane_busy() { # <unit>: the lane holds its lock (a job is running or finishing)
  [ -s "$(lane_data_dir "$1")/lane.lock" ]
}

restart_workers() { # <unit...>
  local -a targets=()
  collect_worker_targets targets "$@"
  [ "${#targets[@]}" -gt 0 ] || return 0
  if [ "$dry_run" = 1 ]; then
    for target in "${targets[@]}"; do
      log "dry-run: would drain and restart $target"
    done
    return 0
  fi
  local -a busy=() idle=()
  local target
  for target in "${targets[@]}"; do
    if lane_busy "$target"; then busy+=("$target"); else idle+=("$target"); fi
  done
  if [ "$restart_lanes" = when-idle ]; then
    for target in "${idle[@]:-}"; do [ -n "$target" ] && restart_unit "$target"; done
    for target in "${busy[@]:-}"; do [ -n "$target" ] && report_pending "$target"; done
    return 0
  fi
  if [ "${#busy[@]}" -eq 0 ]; then
    for target in "${idle[@]}"; do restart_unit "$target"; done
    return 0
  fi
  # Drain: pause claims so a finishing job is not immediately replaced, wait
  # for the busy lanes, restart them, then resume dispatch. The factory is
  # never left paused, even if the installer dies halfway.
  local paused=0
  resume_factory() {
    [ "$paused" = 1 ] || return 0
    if machinist_ctl resume >/dev/null 2>&1; then
      log "resumed lane claims"
    else
      log "warning: resume failed; run: machinist resume"
    fi
  }
  trap resume_factory EXIT
  if machinist_ctl pause >/dev/null 2>&1; then
    paused=1
    log "paused lane claims (factory drain); waiting for busy lanes"
  else
    log "warning: could not pause the factory (is the control plane down?); restarting idle lanes only"
  fi
  for target in "${idle[@]}"; do restart_unit "$target"; done
  local deadline=$(( $(date +%s) + drain_timeout ))
  local busy_count=${#busy[@]}
  while [ "$busy_count" -gt 0 ] && [ "$(date +%s)" -lt "$deadline" ]; do
    sleep 5
    local -a still_busy=()
    for target in "${busy[@]}"; do
      if lane_busy "$target"; then still_busy+=("$target"); else idle+=("$target"); fi
    done
    busy=("${still_busy[@]}")
    busy_count=${#busy[@]}
  done
  for target in "${idle[@]:-}"; do [ -n "$target" ] && restart_unit "$target"; done
  for target in "${busy[@]:-}"; do [ -n "$target" ] && report_pending "$target"; done
  resume_factory
  paused=0
  trap - EXIT
}

if [ "${MACHINIST_SKIP_SYSTEMCTL:-0}" = 1 ]; then
  log "MACHINIST_SKIP_SYSTEMCTL set; skipped daemon-reload and restarts"
elif ! command -v "$systemctl_cmd" >/dev/null 2>&1; then
  log "systemctl not found; skipped daemon-reload and restarts"
elif [ "$(id -u)" != 0 ] && [ "${MACHINIST_ASSUME_PRIVILEGED:-0}" != 1 ]; then
  log "not running as root; skipped daemon-reload and restarts"
else
  run "$systemctl_cmd" daemon-reload
  if [ "$restart_lanes" != no ]; then
    declare -a worker_changed=() plain_changed=()
    local_unit=""
    for local_unit in "${changed_units[@]}"; do
      case "$local_unit" in
        machinist-worker|machinist-worker@*) worker_changed+=("$local_unit") ;;
        *) plain_changed+=("$local_unit") ;;
      esac
    done
    for local_unit in "${plain_changed[@]:-}"; do
      [ -n "$local_unit" ] || continue
      if unit_active "$local_unit" || ctl is-enabled --quiet "$local_unit" 2>/dev/null; then
        restart_unit "$local_unit"
      fi
    done
    restart_workers "${worker_changed[@]:-}"
  else
    log "skipping restarts (--no-restart); units keep running until restarted manually"
  fi
  if [ "${#pending_lanes[@]}" -gt 0 ]; then
    log "lanes pending restart (still running old code): ${pending_lanes[*]}"
    log "rerun install.sh once the lanes drain, or restart them manually when idle"
  fi
fi

log "done (dry-run=$dry_run, sha=$sha)"