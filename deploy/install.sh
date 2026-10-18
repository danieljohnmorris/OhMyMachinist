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
#   deploy/install.sh [--dry-run] [--repo <path>]
#
# The installer never overwrites the local config overlay
# (/etc/machinist/config.local.toml, worker.local.toml) or the secret env files
# (/etc/machinist/*.env): it only writes the tracked config/env templates as
# <name>.example and seeds config.toml/worker.toml the first time. It refuses
# to install from a tree that a gitleaks scan reports as containing secrets.
set -euo pipefail

dry_run=0
repo=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dry-run) dry_run=1 ;;
    --repo) shift; repo="${1:?--repo needs a path}" ;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
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

# --- secret scan -----------------------------------------------------------
# Refuse to install from a tree with secret findings, so a leaked credential
# can never be copied onto the host.
if command -v gitleaks >/dev/null 2>&1; then
  log "scanning $repo for secrets with gitleaks"
  if ! gitleaks detect --source "$repo" --no-git --redact >/dev/null; then
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

# --- config pre-flight (MACH-70) -------------------------------------------
# A server config without [alerts] means failed runs alert nowhere: no spool
# file for the Factory Alerts watcher and no webhook. The overlay can carry
# the section, so both files count.
alerts_configured=0
for candidate in "$config_dir/config.toml" \
  "${MACHINIST_CONFIG_OVERLAY:-$config_dir/config.local.toml}"; do
  if [ -f "$candidate" ] && grep -Eq '^[[:space:]]*\[alerts\]' "$candidate"; then
    alerts_configured=1
  fi
done
if [ "$alerts_configured" = 0 ]; then
  log "WARNING: $config_dir/config.toml has no [alerts] section; failed runs alert nowhere (no spool file, no webhook) - see deploy/config/config.toml.example"
fi

# --- alert spool access (MACH-72) ------------------------------------------
# The alert watcher runs as a separate local user that reads and moves spooled
# alerts without root. Provision a shared group, setgid group-rw spool
# directories with default ACLs, and a tmpfiles.d rule so it survives reboots.
#
#   [alerts] group    reader group (default "machinist-alerts")
#   [alerts] readers  optional extra local users allowed to read the spool
#
# The settings come from the seeded config or the overlay (the overlay wins).
# Readers that must traverse the service user's home get only --x by ACL on
# the parent directories, never a world-wide chmod.

service_user="${MACHINIST_SERVICE_USER:-machinist}"
tmpfiles_dir="${MACHINIST_TMPFILES_DIR:-/etc/tmpfiles.d}"

# alerts_setting <key> <fallback>: last value of key inside the [alerts]
# table of config.toml / the overlay. Strings are unquoted; arrays keep a
# comma-separated body.
alerts_setting() {
  local key="$1" fallback="$2" candidate value=""
  for candidate in "${MACHINIST_CONFIG_OVERLAY:-$config_dir/config.local.toml}" "$config_dir/config.toml"; do
    [ -f "$candidate" ] || continue
    value=$(awk -v key="$key" '
      /^[[:space:]]*\[/ { inalerts = ($0 ~ /^[[:space:]]*\[alerts\]/) }
      inalerts && $1 == key {
        sub(/^[^=]*=[[:space:]]*/, "")
        gsub(/^[[:space:]]+|[[:space:]]+$/, "")
        print
        exit
      }' "$candidate")
    if [ -n "$value" ]; then
      printf '%s\n' "$value" | sed -e 's/^"//; s/"$//'
      return 0
    fi
  done
  printf '%s\n' "$fallback"
}

alerts_group=$(alerts_setting group "machinist-alerts")

readers=""
readers_value=$(alerts_setting readers "")
if [ -n "$readers_value" ]; then
  readers=$(printf '%s\n' "$readers_value" | tr -d '[]"' | tr ',' ' ')
fi

# alert home expansion: a leading ~ in the configured directory is the service
# user's home, matching how the server resolves the path.
# The literal tilde is expanded to the service user's home below.
# shellcheck disable=SC2088
alerts_dir=$(alerts_setting directory "~/.machinist/alerts/pending")
# shellcheck disable=SC2088
service_home=${MACHINIST_SERVICE_HOME:-$(getent passwd "$service_user" 2>/dev/null | cut -d: -f6)}
case $alerts_dir in
  \~*) alerts_dir="${service_home:-/home/$service_user}${alerts_dir#\~}" ;;
esac
if [ "${alerts_dir%/}" = "$alerts_dir" ]; then
  alerts_spool="$alerts_dir"
else
  alerts_spool="${alerts_dir%/}"
fi
alerts_root=$(dirname "$alerts_spool")
alert_dirs=("$alerts_root" "$alerts_spool")
case $(basename "$alerts_spool") in
  pending) alert_dirs+=("$alerts_root/done") ;;
  *) alert_dirs+=("$alerts_spool/pending" "$alerts_spool/done") ;;
esac

# traverse_dirs: ancestors of the spool between the service user's home
# (inclusive) and the spool's parent. Readers only ever get --x on these, and
# nothing outside the service user's home is ever touched.
traverse_dirs=()
if [ -n "$service_home" ]; then
  case "$alerts_root" in
    "$service_home" | "$service_home"/*)
      parent="$(dirname "$alerts_root")"
      while :; do
        traverse_dirs+=("$parent")
        [ "$parent" = "$service_home" ] && break
        parent=$(dirname "$parent")
        case "$parent" in / | .) traverse_dirs=(); break ;; esac
      done
      ;;
  esac
fi

alert_acl_args=()
if command -v setfacl >/dev/null 2>&1; then
  alert_acl_args=(-m "g:$alerts_group:rwx" -m "d:g:$alerts_group:rwx" -m "d:u:$service_user:rwx")
fi

provision_alerts_access() {
  local dir user
  if ! getent group "$alerts_group" >/dev/null 2>&1; then
    log "create alert group $alerts_group"
    groupadd -r "$alerts_group"
  fi
  for user in "$service_user" $readers; do
    if ! getent passwd "$user" >/dev/null 2>&1; then
      log "WARNING: alert reader $user does not exist; skipped"
      continue
    fi
    if ! id -nG "$user" | tr ' ' '\n' | grep -qxF "$alerts_group"; then
      log "add $user to group $alerts_group"
      usermod -aG "$alerts_group" "$user"
    fi
  done
  for dir in "${alert_dirs[@]}"; do
    log "provision alert directory $dir (2770 $service_user:$alerts_group, default ACLs)"
    mkdir -p "$dir"
    chown "$service_user:$alerts_group" "$dir"
    chmod 2770 "$dir"
    if [ "${#alert_acl_args[@]}" -gt 0 ]; then
      setfacl "${alert_acl_args[@]}" "$dir"
    else
      log "WARNING: setfacl not found; skipped default ACLs for $dir (the tmpfiles a+ rule still applies them at boot)"
    fi
  done
  # Readers need to traverse the service user's home to reach the spool:
  # grant only --x by ACL on the parent directories, never a world-wide chmod.
  # A filesystem that refuses the ACL (e.g. an id-mapped tmpfs) only warns:
  # the grant is a convenience, the spool ACLs themselves stay mandatory.
  for dir in "${traverse_dirs[@]}"; do
    for user in $readers; do
      [ "${#alert_acl_args[@]}" -gt 0 ] || break
      if ! setfacl -m "u:$user:--x" "$dir" 2>/dev/null; then
        log "WARNING: could not grant $user --x on $dir; the reader may not reach the spool"
      fi
    done
  done
  mkdir -p "$tmpfiles_dir"
  log "install $tmpfiles_dir/machinist-alerts.conf"
  {
    printf '# Managed by OhMyMachinist deploy/install.sh (MACH-72); rewritten on every install.\n'
    printf '# Recreates the setgid group-rw alert spool and its default ACLs after reboots.\n'
    for dir in "${alert_dirs[@]}"; do
      printf 'd %s 2770 %s %s -\n' "$dir" "$service_user" "$alerts_group"
    done
    # One ACL entry per a+ line: the argument field is a single ACL text.
    for dir in "${alert_dirs[@]}"; do
      printf 'a+ %s - - - - d:g:%s:rwx\n' "$dir" "$alerts_group"
      printf 'a+ %s - - - - d:u:%s:rwx\n' "$dir" "$service_user"
    done
  } >"$tmpfiles_dir/machinist-alerts.conf"
}

# Retire the root chmod stopgap (MACH-72): with group access provisioned the
# machinist-alerts-perms watcher is redundant and is removed. Only units in
# the managed systemd directory are touched.
retire_alerts_perms_stopgap() {
  local unit
  for unit in machinist-alerts-perms.path machinist-alerts-perms.service; do
    [ -e "$systemd_dir/$unit" ] || continue
    log "retire stopgap unit $unit"
    if [ "${MACHINIST_SKIP_SYSTEMCTL:-0}" != 1 ]; then
      run systemctl disable --now "$unit" 2>/dev/null || true
    fi
    run rm -f "$systemd_dir/$unit"
  done
}

if [ "$dry_run" = 1 ]; then
  for dir in "${alert_dirs[@]}"; do
    printf '  dry-run: mkdir -p %s && chown %s:%s && chmod 2770 && setfacl d:g:%s:rwx\n' \
      "$dir" "$service_user" "$alerts_group" "$alerts_group"
  done
  printf '  dry-run: install %s/machinist-alerts.conf (d and a+ rules for %s)\n' "$tmpfiles_dir" "$alerts_group"
  for unit in machinist-alerts-perms.path machinist-alerts-perms.service; do
    [ -e "$systemd_dir/$unit" ] && printf '  dry-run: disable and remove stopgap unit %s\n' "$unit"
  done
  alerts_provisioned=0
elif [ "$(id -u)" = 0 ] || [ "${MACHINIST_ALERTS_PROVISION:-0}" = 1 ]; then
  # MACHINIST_ALERTS_PROVISION=1 lets privileged-but-not-root contexts
  # (containers) run the provisioning; it is not needed for a normal sudo run.
  alerts_provisioned=1
  provision_alerts_access
  retire_alerts_perms_stopgap
  if [ "${MACHINIST_SKIP_SYSTEMCTL:-0}" != 1 ] && command -v systemd-tmpfiles >/dev/null 2>&1; then
    run systemd-tmpfiles --create "$tmpfiles_dir/machinist-alerts.conf"
  fi
else
  alerts_provisioned=0
  log "not running as root; skipped alert group, spool directory, ACL and tmpfiles provisioning (MACH-72)"
fi
if [ "$alerts_provisioned" = 1 ]; then
  manifest_entries+=("$tmpfiles_dir/machinist-alerts.conf||$(sha256_of "$tmpfiles_dir/machinist-alerts.conf")")
fi

# --- manifest --------------------------------------------------------------
if [ "$dry_run" = 1 ]; then
  log "dry-run: would record ${#manifest_entries[@]} files and SHA $sha in $manifest"
else
  mkdir -p "$(dirname "$manifest")"
  {
    printf '{\n  "sha": "%s",\n  "repo": "%s",\n  "installed_at": "%s",\n  "files": [\n' \
      "$sha" "$repo" "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
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
if [ "${MACHINIST_SKIP_SYSTEMCTL:-0}" = 1 ]; then
  log "MACHINIST_SKIP_SYSTEMCTL set; skipped daemon-reload and restarts"
elif [ "$(id -u)" != 0 ]; then
  log "not running as root; skipped daemon-reload and restarts"
elif command -v systemctl >/dev/null 2>&1; then
  run systemctl daemon-reload
  for unit in "${changed_units[@]}"; do
    if systemctl is-active --quiet "$unit" || systemctl is-enabled --quiet "$unit" 2>/dev/null; then
      log "restart $unit"
      run systemctl restart "$unit"
    fi
  done
else
  log "systemctl not found; skipped daemon-reload and restarts"
fi

log "done (dry-run=$dry_run, sha=$sha)"