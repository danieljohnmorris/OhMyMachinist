#!/usr/bin/env bash
# Restart a Machinist worker lane only when the lane is idle (MACH-59).
#
# A lane writes <data-dir>/lane.lock for the whole run, including the final
# steps (publishing outputs and reporting the completion). This script refuses
# to restart the worker while that marker is present, so a restart-when-idle
# never cuts a job off in the middle of its final steps and then fails every
# queued job behind it.
#
#   restart-when-idle.sh <systemd-unit> [data-dir]
#
# The data dir defaults to $MACHINIST_DATA_DIR, then ~/.machinist. Exits 0
# without restarting a busy lane, and 0 after restarting an idle one.
set -euo pipefail
unit="${1:?usage: restart-when-idle.sh <systemd-unit> [data-dir]}"
data_dir="${2:-${MACHINIST_DATA_DIR:-$HOME/.machinist}}"
lock="$data_dir/lane.lock"
if [ -s "$lock" ]; then
  printf 'restart-when-idle: lane busy with run %s; not restarting %s\n' "$(cat "$lock")" "$unit" >&2
  exit 0
fi
printf 'restart-when-idle: lane idle; restarting %s\n' "$unit" >&2
systemctl restart "$unit"