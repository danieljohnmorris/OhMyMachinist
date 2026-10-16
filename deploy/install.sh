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