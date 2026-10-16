#!/usr/bin/env bash
# MACH-62: THE single definition of a "dirty lane", shared by the Machinist
# runner and this wrapper's preflight. A lane is dirty when
# `git status --porcelain` in the repository lists anything: tracked
# modifications AND untracked files; gitignored files never count (`--porcelain`
# honours .gitignore for untracked files). The runner must use the exact same
# command so a lane the runner refuses to reuse is also a lane omp-glm-pr
# refuses to start on.
#
# Usage: lane-dirty.sh <repo>
# Prints the porcelain status (nothing when the lane is clean). Always exits 0.
set -uo pipefail
repo="${1:-}"
[ -n "$repo" ] || { echo "usage: lane-dirty.sh <repo>" >&2; exit 0; }
git -C "$repo" status --porcelain 2>/dev/null
exit 0