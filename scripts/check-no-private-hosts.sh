#!/usr/bin/env bash
# Fails when a tracked file contains a Tailscale address (100.64.0.0/10) or a
# personal hostname listed in scripts/private-hosts.txt (MACH-64). Scans every
# tracked file, including built bundles under internal/controlplane/web/dist.
set -euo pipefail
cd "$(dirname "$0")/.."

hosts_file="scripts/private-hosts.txt"
tailscale_re='100\.(6[4-9]|7[0-9]|8[0-9]|9[0-9]|1[0-1][0-9]|12[0-7])\.[0-9]{1,3}\.[0-9]{1,3}'

# Lines the guard must tolerate: the documented CGNAT range text and the fixed
# test address. Entries are "<file>:<extended regex that must match the line>".
allowlist=(
  'docs/api.md:100\.64\.0\.0/10'
  'internal/controlplane/api.go:100\.64\.0\.0/10'
  'internal/controlplane/api_auth_test.go:100\.100\.1\.1'
)

status=0

allowed() {
  local file="$1" line="$2" entry pattern
  for entry in "${allowlist[@]}"; do
    if [[ "$entry" == "$file:"* ]]; then
      pattern="${entry#*:}"
      if [[ "$line" =~ $pattern ]]; then
        return 0
      fi
    fi
  done
  return 1
}

scan() {
  local file="$1" pattern="$2" label="$3" hit lineno text
  while IFS= read -r hit; do
    lineno="${hit%%:*}"
    text="${hit#*:}"
    if ! allowed "$file" "$text"; then
      echo "$file:$lineno: $label: $text" >&2
      status=1
    fi
  done < <(grep -anE -- "$pattern" "$file" 2>/dev/null || true)
}

while IFS= read -r -d '' file; do
  case "$file" in
    scripts/check-no-private-hosts.sh | scripts/private-hosts.txt) continue ;;
  esac
  scan "$file" "$tailscale_re" "Tailscale address"
  while IFS= read -r host; do
    [[ -z "$host" || "$host" == \#* ]] && continue
    scan "$file" "$(printf '%s' "$host" | sed 's/[][^.*+?${}\\()|]/\\&/g')" "private host $host"
  done < "$hosts_file"
done < <(git ls-files -z)

if [ "$status" -ne 0 ]; then
  echo "check-no-private-hosts: private hostnames or Tailscale addresses found (see scripts/private-hosts.txt)" >&2
fi
exit "$status"
