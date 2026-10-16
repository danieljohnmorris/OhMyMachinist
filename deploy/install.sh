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
# install.sh checks that the binary exists and answers `version` before it
# touches any unit; --install-binary copies a built binary there first. The
# units run /usr/local/bin/machinist, so a binary living elsewhere is linked
# into /usr/local/bin when install.sh runs as root.
#
# Restarts drain by default: worker lanes are only restarted once no job is
# running. Claims are paused with the factory control (`machinist pause`, PR
# #54) so a drained lane cannot claim the next job while we wait, and the
# factory is always resumed again (even on error). --restart-lanes=when-idle
# restarts only currently idle lanes, --no-restart leaves every unit alone;
# lanes still busy when --drain-timeout expires (default 600s) keep running
# the old code and are reported as pending.
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
    --restart-lanes=drain) restart_lanes="drain" ;;
    --restart-lanes=when-idle) restart_lanes="when-idle" ;;
    --drain-timeout) shift; drain_timeout="${1:?--drain-timeout needs seconds}" ;;
    -h|--help) sed -n '2,33p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
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

run() { if [ "$dry_run" = 1 ]; then printf '  dry-run: %s\n' "$*"; else "$@"; fi; }

log() { printf 'install.sh: %s\n' "$*"; }

# --- binary ----------------------------------------------------------------
# The units run the binary at a fixed path, so it must exist and work before
# any unit is touched. MACHINIST_BIN / --binary points at an existing install
# on an unusual path; --install-binary copies a built binary there first.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  else printf 'install.sh: sha256sum or shasum is required\n' >&2; exit 1; fi
}

manifest_entries=()

if [ -n "$binary_src" ]; then
  [ -f "$binary_src" ] || { printf 'install.sh: --install-binary %s is not a file\n' "$binary_src" >&2; exit 1; }
  if [ "$dry_run" = 1 ]; then
    log "dry-run: would install $binary_src to $bin_path"
  else
    log "install $bin_path from $binary_src"
    install -D -m 0755 "$binary_src" "$bin_path"
    manifest_entries+=("$bin_path|$binary_src|$(sha256_of "$binary_src")")
  fi
fi
if [ "$dry_run" != 1 ] || [ -z "$binary_src" ]; then
  if [ ! -x "$bin_path" ]; then
    printf 'install.sh: machinist binary %s does not exist or is not executable; set MACHINIST_BIN, pass --binary <path> or --install-binary <src>\n' "$bin_path" >&2
    exit 1
  fi
  if ! version_output="$("$bin_path" version 2>&1)"; then
    printf "install.sh: %s does not run; \`machinist version\` failed:\n%s\n" "$bin_path" "$version_output" >&2
    exit 1
  fi
  log "binary $bin_path: $version_output"
fi

# The units run /usr/local/bin/machinist; make that path work when the binary
# lives elsewhere (for example /opt/machinist/bin/machinist).
if [ "$bin_path" != /usr/local/bin/machinist ] && [ ! -e /usr/local/bin/machinist ]; then
  if [ "$dry_run" = 0 ] && [ "$(id -u)" = 0 ]; then
    ln -sfn "$bin_path" /usr/local/bin/machinist && log "linked /usr/local/bin/machinist -> $bin_path"
  else
    log "warning: the units run /usr/local/bin/machinist but the binary is at $bin_path and no link was created (not root)"
  fi
fi

# --- secret scan -----------------------------------------------------------
# Refuse to install from a tree with secret findings, so a leaked credential
# can never be copied onto the host. The scan runs from inside the repository
# with a relative source: gitleaks then writes fingerprints relative to the
# repo root, the same form used in .gitleaksignore, so allowlisted fixtures
# stay allowlisted. An absolute --source writes /abs/path/file:rule:line
# fingerprints that never match the ignore file and would refuse every install.
if command -v gitleaks >/dev/null 2>&1; then
  log "scanning the working tree for secrets with gitleaks"
  if ! scan_output="$(cd "$repo" && gitleaks detect --source . --no-git --redact 2>&1)"; then
    printf 'install.sh: gitleaks found secrets under %s; refusing to install\n' "$repo" >&2
    [ -n "$scan_output" ] && printf '%s\n' "$scan_output" >&2
    exit 1
  fi
else
  log "gitleaks not found; skipping the pre-install secret scan"
fi

[ -f "$repo/deploy/factory/deliver-pr.py" ] || { printf 'install.sh: %s does not look like the OhMyMachinist repository\n' "$repo" >&2; exit 1; }

# --- config pre-flight (MACH-67) -------------------------------------------
# Relative paths in a config resolve against the config file's directory, so a
# config migrated from ~/.machinist to /etc/machinist resolves token_file =
# "server/worker.token" under /etc/machinist and the worker crash-loops. Load
# every config the units will use from its new location and check that every
# referenced file (token, prompt, repository) exists before changing anything.
# A fresh host has no *.toml yet, so the check is skipped there.
if [ -n "$(find "$config_dir" -maxdepth 1 -name '*.toml' -print -quit 2>/dev/null)" ]; then
  machinist_home="${MACHINIST_HOME:-$(getent passwd machinist 2>/dev/null | cut -d: -f6)}"
  log "pre-flight: checking existing configs in $config_dir"
  if ! preflight_output="$(python3 - "$config_dir" "${machinist_home:-}" <<'PY'
import os, sys, tomllib

config_dir, home = sys.argv[1], sys.argv[2]

def resolve(raw, base_dir):
    p = raw.strip()
    if p.startswith("~"):
        if not home:
            return None
        return os.path.join(home, p[1:].lstrip("/"))
    if not os.path.isabs(p):
        return os.path.join(base_dir, p)
    return p

errors, checked = [], 0
try:
    names = sorted(fn for fn in os.listdir(config_dir)
                   if fn.endswith(".toml") and (fn.startswith("config") or fn.startswith("worker")))
except OSError as err:
    sys.exit("cannot list %s: %s" % (config_dir, err))

for name in names:
    path = os.path.join(config_dir, name)
    try:
        with open(path, "rb") as handle:
            data = tomllib.load(handle)
    except (OSError, tomllib.TOMLDecodeError) as err:
        errors.append("%s: cannot parse: %s" % (path, err))
        continue
    checked += 1

    def walk(node, top=False):
        for key, value in node.items():
            if isinstance(value, dict):
                # Only the top-level [repositories.*] tables map Machinist
                # names to checkouts; [github.repositories] maps names to
                # "owner/name" strings and must not be touched.
                if top and key == "repositories":
                    for repo, entry in value.items():
                        if isinstance(entry, dict) and isinstance(entry.get("path"), str):
                            check(entry["path"], "[repositories.%s] path" % repo)
                walk(value)
            elif key in ("token_file", "worker_token_file", "prompt_file") and isinstance(value, str):
                check(value, key)

    def check(raw, label):
        target = resolve(raw, config_dir)
        if target is None:
            errors.append("%s: %s = \"%s\" needs the machinist home, which could not be resolved" % (path, label, raw))
        elif not os.path.exists(target):
            errors.append("%s: %s = \"%s\" does not exist (looked for %s); rewrite it as an absolute or ~/ path"
                          % (path, label, raw, target))

    walk(data, top=True)

if errors:
    for err in errors:
        print(err, file=sys.stderr)
    sys.exit(1)
print("%d config file(s) checked, every referenced path exists" % checked)
PY
)"; then
    printf '%s\n' "$preflight_output" >&2
    printf 'install.sh: config pre-flight failed; nothing was changed\n' >&2
    exit 1
  fi
  log "pre-flight: $preflight_output"
else
  log "pre-flight: no existing configs in $config_dir; nothing to check"
fi

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

install_file() { # <src> <dest>
  local src="$1" dest="$2" mode
  mode=0644
  [ -x "$src" ] && mode=0755
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

# --- unmanaged drop-ins in managed unit directories -------------------------
# Drop-ins that live in a managed <unit>.d/ directory but are not shipped by
# the repo are local overrides: they are never overwritten, but they are
# reported here and by the drift check so they stay visible (MACH-67 gap 2:
# the hand-made 90-local.conf PATH drop-ins were invisible to both).
declare -A managed_conf=()
for entry in "${manifest_entries[@]}"; do
  IFS='|' read -r dest _source _sum <<<"$entry"
  managed_conf["$dest"]=1
done
for src in "${unit_files[@]}"; do
  case "$src" in
    *.d/*.conf) ;;
    *) continue ;;
  esac
  dest_dir="$systemd_dir/$(dirname "${src#deploy/systemd/}")"
  [ -d "$dest_dir" ] || continue
  for conf in "$dest_dir"/*.conf; do
    [ -e "$conf" ] || continue
    [ -n "${managed_conf[$conf]:-}" ] && continue
    log "local override (unmanaged drop-in): $conf"
  done
done

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
if [ "$dry_run" = 1 ]; then
  log "dry-run: would record ${#manifest_entries[@]} files and SHA $sha in $manifest"
else
  mkdir -p "$(dirname "$manifest")"
  {
    printf '{\n  "sha": "%s",\n  "repo": "%s",\n  "binary": "%s",\n  "installed_at": "%s",\n  "files": [\n' \
      "$sha" "$repo" "$bin_path" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
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
# Worker lanes follow the restart policy; everything else (control plane,
# timers, ...) restarts straight away when active. The drain uses the factory
# pause/drain control (machinist pause, PR #54) so a lane that goes idle
# cannot claim the next job while we wait; the EXIT trap makes sure claims are
# always resumed again, even when install.sh is interrupted.
is_worker_unit() { case "$1" in machinist-worker*) return 0 ;; *) return 1 ;; esac; }

lane_config_of() { # <unit> -> worker config path
  case "$1" in
    *@*) lane="${1#*@}"; lane="${lane%.service}"; printf '%s/worker-%s.toml' "$config_dir" "$lane" ;;
    *)   printf '%s/worker.toml' "$config_dir" ;;
  esac
}

# lane_idle <unit>: a lane writes <data-dir>/lane.lock for the whole run
# (restart-when-idle.sh, MACH-59); an empty or missing lock means idle.
lane_idle() {
  local unit="$1" cfg data_dir home
  cfg="$(lane_config_of "$unit")"
  data_dir="$(sed -n 's/^data_directory *= *"\([^"]*\)".*/\1/p' "$cfg" 2>/dev/null | head -1)"
  data_dir="${data_dir:-~/.machinist/worker}"
  if [ "${data_dir#\~}" != "$data_dir" ]; then
    home="${MACHINIST_HOME:-$(getent passwd machinist 2>/dev/null | cut -d: -f6)}"
    [ -n "$home" ] || return 0
    data_dir="${data_dir/\~/$home}"
  fi
  [ ! -s "$data_dir/lane.lock" ]
}

factory_cli() { # the pause/resume control must read the control plane's DB,
                # so run it as the machinist user when we are root
  local cli="${MACHINIST_CLI:-$bin_path}"
  if [ -z "${MACHINIST_CLI:-}" ] && [ "$(id -u)" = 0 ] \
     && command -v runuser >/dev/null 2>&1 && getent passwd machinist >/dev/null 2>&1; then
    runuser -u machinist -- "$cli" "$@"
  else
    "$cli" "$@"
  fi
}

factory_pause() {
  if ! factory_cli pause --reason "install.sh: draining worker lanes before restart" >/dev/null 2>&1; then
    log "could not pause claims with the factory control ($bin_path pause)"
    return 1
  fi
}

factory_resume() {
  if ! factory_cli resume >/dev/null 2>&1; then
    log "warning: could not resume claims; run 'machinist resume' to start dispatch again"
  fi
}

pending_workers=()
if [ "${MACHINIST_SKIP_SYSTEMCTL:-0}" = 1 ]; then
  log "MACHINIST_SKIP_SYSTEMCTL set; skipped daemon-reload and restarts"
elif [ "$(id -u)" != 0 ] && [ "${MACHINIST_ASSUME_PRIVILEGED:-0}" != 1 ]; then
  log "not running as root; skipped daemon-reload and restarts"
elif ! command -v systemctl >/dev/null 2>&1; then
  log "systemctl not found; skipped daemon-reload and restarts"
elif [ "$restart_lanes" = no ]; then
  run systemctl daemon-reload
  log "--no-restart: no unit restarted; lanes keep running their current code"
elif [ "$dry_run" = 1 ]; then
  run systemctl daemon-reload
  for unit in "${changed_units[@]}"; do
    if systemctl is-active --quiet "$unit"; then
      printf '  dry-run: systemctl restart %s\n' "$unit"
    fi
  done
else
  run systemctl daemon-reload
  worker_units=()
  for unit in "${changed_units[@]}"; do
    if is_worker_unit "$unit"; then
      worker_units+=("$unit")
    elif systemctl is-active --quiet "$unit" || systemctl is-enabled --quiet "$unit" 2>/dev/null; then
      log "restart $unit"
      run systemctl restart "$unit"
    fi
  done

  # A changed machinist-worker@.service template means every lane unit needs
  # the policy; expand it to the lanes defined by their configs. Lanes that
  # are neither active nor enabled are skipped by the policy below.
  expanded=()
  for unit in "${worker_units[@]:-}"; do
    [ -n "$unit" ] || continue
    case "$unit" in
      *@*.service)
        for cfg in "$config_dir"/worker-*.toml; do
          [ -e "$cfg" ] || continue
          lane="$(basename "$cfg")"; lane="${lane#worker-}"; lane="${lane%.toml}"
          expanded+=("machinist-worker@$lane.service")
        done
        ;;
      *) expanded+=("$unit") ;;
    esac
  done
  worker_units=("${expanded[@]:-}")

  case "$restart_lanes" in
    when-idle)
      for unit in "${worker_units[@]:-}"; do
        [ -n "$unit" ] || continue
        if systemctl is-active --quiet "$unit" && lane_idle "$unit"; then
          log "restart $unit (idle)"
          run systemctl restart "$unit"
        elif systemctl is-active --quiet "$unit"; then
          pending_workers+=("$unit")
        fi
      done
      ;;
    drain)
      # Busy lanes: pause claims first so no lane can grab the next job while
      # we wait, then restart each lane as soon as it has drained.
      busy_workers=()
      for unit in "${worker_units[@]:-}"; do
        [ -n "$unit" ] || continue
        if systemctl is-active --quiet "$unit" && ! lane_idle "$unit"; then
          busy_workers+=("$unit")
        fi
      done
      if [ ${#busy_workers[@]} -eq 0 ]; then
        for unit in "${worker_units[@]:-}"; do
          [ -n "$unit" ] || continue
          if systemctl is-active --quiet "$unit" || systemctl is-enabled --quiet "$unit" 2>/dev/null; then
            log "restart $unit (no job running)"
            run systemctl restart "$unit"
          fi
        done
      elif factory_pause; then
        trap factory_resume EXIT
        log "waiting for jobs on ${busy_workers[*]} to finish (drain timeout ${drain_timeout}s)"
        deadline=$(( $(date +%s) + drain_timeout ))
        remaining=("${busy_workers[@]}")
        while [ ${#remaining[@]} -gt 0 ] && [ "$(date +%s)" -lt "$deadline" ]; do
          sleep 2
          next=()
          for unit in "${remaining[@]}"; do
            lane_idle "$unit" || next+=("$unit")
          done
          remaining=("${next[@]}")
        done
        # Restart the drained lanes while claims are still paused, so none of
        # them can claim a new job before it has been restarted.
        for unit in "${busy_workers[@]}"; do
          if lane_idle "$unit"; then
            log "restart $unit (drained)"
            run systemctl restart "$unit"
          else
            pending_workers+=("$unit")
          fi
        done
        factory_resume
        trap - EXIT
      else
        # No factory control: fall back to restart-when-idle and report.
        for unit in "${busy_workers[@]}"; do
          if lane_idle "$unit"; then
            log "restart $unit (idle)"
            run systemctl restart "$unit"
          else
            pending_workers+=("$unit")
          fi
        done
      fi
      ;;
  esac

  for unit in "${pending_workers[@]:-}"; do
    [ -n "$unit" ] || continue
    log "PENDING: $unit is still busy and keeps running the old code until its job finishes; rerun install.sh once it drained"
  done
fi

log "done (dry-run=$dry_run, sha=$sha)"