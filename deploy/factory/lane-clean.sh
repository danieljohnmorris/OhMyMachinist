#!/usr/bin/env bash
# Dirty-lane guard for the Machinist worker units.
# Usage: lane-clean.sh <worker-toml>
# Runs as ExecStartPre=+ on machinist worker units, i.e. BEFORE the worker process
# starts, so this worker cannot hold any lane yet. Each lane checkout belongs to one
# worker only (LANES.md). It still refuses to touch anything if the DB shows a run
# 'running' (or the DB can't be read) for this worker name.
# For each repo in the toml whose basename is in CLEAN_REPOS and whose tracked files are
# modified: save patch + files list to backups/lane-clean/, git stash push (labelled),
# leave the branch as is (the worker checks out the base branch itself). Never fails the unit.
#
# CLEAN_REPOS defaults to every repository in the toml when unset; set it to a
# space-separated basename list to limit which lanes are cleaned.
set -uo pipefail
TOML="${1:?worker toml}"
CLEAN_REPOS="${CLEAN_REPOS:-}"
MACHINIST_USER="${MACHINIST_USER:-machinist}"
DB="${MACHINIST_DB:-$HOME/.machinist/server/machinist.db}"
OUT="${MACHINIST_LANE_CLEAN_DIR:-$HOME/.machinist/server/backups/lane-clean}"
log() { echo "lane-clean: $*" >&2; }
[ -r "$TOML" ] || { log "no $TOML; skip"; exit 0; }
W=$(sed -n 's/^name *= *"\(.*\)"/\1/p' "$TOML" | head -1)
[ -n "$W" ] || { log "no worker name in $TOML; skip"; exit 0; }
n=$(runuser -u "$MACHINIST_USER" -- sqlite3 -readonly "$DB" ".timeout 5000" "select count(*) from runs where state='running' and worker_name='$W'" 2>/dev/null)
[ "$n" = 0 ] || { log "$W has running run(s) or DB unreadable (n='$n'); not touching lanes"; exit 0; }
mkdir -p "$OUT"; chown "$MACHINIST_USER:$MACHINIST_USER" "$OUT"
sed -n 's/^path *= *"\(.*\)"/\1/p' "$TOML" | while read -r repo; do
  b=$(basename "$repo")
  if [ -n "$CLEAN_REPOS" ]; then
    case " $CLEAN_REPOS " in *" $b "*) ;; *) continue;; esac
  fi
  [ -d "$repo/.git" ] || continue
  g() { runuser -u "$MACHINIST_USER" -- git -C "$repo" -c core.hooksPath=/dev/null "$@"; }
  dirty=$(g status --porcelain -uno 2>/dev/null)
  [ -n "$dirty" ] || continue
  ts=$(date +%Y%m%d-%H%M%S); base="$OUT/${W}-${b}-${ts}"
  br=$(g rev-parse --abbrev-ref HEAD 2>/dev/null); head=$(g rev-parse HEAD 2>/dev/null)
  g diff --binary HEAD > "$base.patch" || { log "diff failed for $repo; skip"; continue; }
  { echo "worker: $W"; echo "repo: $repo"; echo "branch: $br"; echo "HEAD: $head"; echo "$dirty"; } > "$base.files.txt"
  chown "$MACHINIST_USER:$MACHINIST_USER" "$base.patch" "$base.files.txt"
  if g stash push -q -m "lane-clean $W $ts (dirty on worker start; patch $base.patch)"; then
    log "stashed dirty $repo [$br] -> $base.patch"
  else
    log "stash failed for $repo; left as is"
  fi
done
exit 0
