#!/usr/bin/env bash
# Install the Machinist PR-review skills where omp discovers them, and install
# the pinned, checksum-verified TruffleHog the built-in review uses to scan a
# pull request for credentials.
#
# omp loads user skills from ~/.agents/skills (skills.enableAgentsUser defaults
# to true). Run this script as each user that should carry the skills
# (the factory worker user, plus any user running omp interactively).
#
# Usage: install-review-skills.sh [target-dir]
#   target-dir    skills destination (default $HOME/.agents/skills)
#   MACHINIST_TOOL_DIR   where to install the trufflehog binary
#                        (default /usr/local/bin when writable, else ~/.local/bin)
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Skills live in review/skills next to this script (both in the repo and in a
# flat install); fall back to a sibling skills/ for older layouts.
if [[ -d "$script_dir/skills" ]]; then
  source_dir="$script_dir/skills"
elif [[ -d "$script_dir/review/skills" ]]; then
  source_dir="$script_dir/review/skills"
else
  source_dir="$script_dir/skills"
fi
target_dir="${1:-$HOME/.agents/skills}"

if [[ ! -d "$source_dir" ]]; then
  echo "skills source not found: $source_dir" >&2
  exit 1
fi

mkdir -p "$target_dir"
cp -R "$source_dir"/. "$target_dir"/
echo "Installed Machinist review skills to $target_dir:"
find "$target_dir" -mindepth 1 -maxdepth 1 -type d -printf '  %f\n'

# --- TruffleHog (pinned, checksum-verified) --------------------------------
# The built-in review shells out to `trufflehog git` over the PR range, so
# factory hosts need the binary on PATH. The version and its release
# checksums are pinned here; a mismatch refuses the install.
trufflehog_version="3.99.2"

trufflehog_sha256() {
  case "$1/$2" in
    linux/amd64) echo "3c7b0d1dc4278294269bfdd7594bca8bb28c78da0a4a06ad202ded37b48deca4" ;;
    linux/arm64) echo "929b81efffca752eafd58bf6f17bf725d354f338636a7bbbee65c086d2f9ddf3" ;;
    darwin/amd64) echo "c23a1b10522b8b84cfd04dc88a3a8819d3dc23b20f15e23540ba442b4b2fbeed" ;;
    darwin/arm64) echo "b612143d840b4c75d41fe40436c5932efead001951283f18c87013c4145e1675" ;;
    *) echo "" ;;
  esac
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    echo "trufflehog: sha256sum or shasum is required" >&2
    exit 1
  fi
}

install_trufflehog() {
  if command -v trufflehog >/dev/null 2>&1; then
    echo "trufflehog already installed: $(command -v trufflehog)"
    return 0
  fi
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) echo "trufflehog: unsupported OS $(uname -s); install it manually" >&2; return 1 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) echo "trufflehog: unsupported architecture $(uname -m); install it manually" >&2; return 1 ;;
  esac
  expected="$(trufflehog_sha256 "$os" "$arch")"
  if [[ -z "$expected" ]]; then
    echo "trufflehog: no pinned checksum for $os/$arch; install it manually" >&2
    return 1
  fi
  dest_dir="${MACHINIST_TOOL_DIR:-}"
  if [[ -z "$dest_dir" ]]; then
    if [[ -w /usr/local/bin ]]; then dest_dir=/usr/local/bin; else dest_dir="$HOME/.local/bin"; fi
  fi
  archive="trufflehog_${trufflehog_version}_${os}_${arch}.tar.gz"
  base_url="https://github.com/trufflesecurity/trufflehog/releases/download/v${trufflehog_version}"
  tmp="$(mktemp -d)"
  trap 'rm -rf "$tmp"' EXIT
  curl -fsSL "$base_url/$archive" -o "$tmp/$archive"
  actual="$(sha256_of "$tmp/$archive")"
  if [[ "$actual" != "$expected" ]]; then
    echo "trufflehog: checksum mismatch for $archive (want $expected, got $actual); refusing to install" >&2
    return 1
  fi
  tar -xzf "$tmp/$archive" -C "$tmp" trufflehog
  mkdir -p "$dest_dir"
  install -m 0755 "$tmp/trufflehog" "$dest_dir/trufflehog"
  echo "Installed trufflehog $trufflehog_version to $dest_dir/trufflehog"
  case ":$PATH:" in
    *":$dest_dir:"*) ;;
    *) echo "add $dest_dir to PATH so the review can run trufflehog" ;;
  esac
}

for tool in curl tar; do
  command -v "$tool" >/dev/null 2>&1 || { echo "trufflehog: $tool is required" >&2; exit 1; }
done

install_trufflehog
