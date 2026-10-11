#!/usr/bin/env bash
# Install the Machinist PR-review skills where omp discovers them.
#
# omp loads user skills from ~/.agents/skills (skills.enableAgentsUser defaults
# to true). Run this script as each user that should carry the skills
# (the factory worker user, plus any user running omp interactively).
#
# Usage: install-review-skills.sh [target-dir]
set -euo pipefail

source_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/skills"
target_dir="${1:-$HOME/.agents/skills}"

if [[ ! -d "$source_dir" ]]; then
  echo "skills source not found: $source_dir" >&2
  exit 1
fi

mkdir -p "$target_dir"
cp -R "$source_dir"/. "$target_dir"/
echo "Installed Machinist review skills to $target_dir:"
find "$target_dir" -mindepth 1 -maxdepth 1 -type d -printf '  %f\n'
