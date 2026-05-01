#!/usr/bin/env bash
# check-public-safe.sh — scan staged or about-to-push content for personal /
# internal patterns that don't belong in a public repo.
#
# Modes:
#   commit   — scan `git diff --cached` (the staged tree)
#   push     — scan everything from upstream..HEAD (commits about to leave the laptop)
#
# Exit codes:
#   0  — clean, allow the operation
#   2  — at least one pattern matched, block the operation
#   3  — usage error
#
# Override (single operation, intentionally noisy to set):
#   DINGDONG_PUBLIC_OVERRIDE=1 git push
#
# Why a script and not just patterns inline in settings.json?
#   - Shellable from a real git hook too (see CLAUDE.md § Public-repo Safety).
#   - One source of truth for the pattern list.
#   - Easier to test in isolation: `bash .claude/scripts/check-public-safe.sh commit`.

set -uo pipefail

mode="${1:-}"
case "$mode" in
  commit|push) ;;
  *)
    echo "usage: $0 <commit|push>" >&2
    exit 3
    ;;
esac

if [ "${DINGDONG_PUBLIC_OVERRIDE:-}" = "1" ]; then
  echo "check-public-safe: DINGDONG_PUBLIC_OVERRIDE=1, skipping scan" >&2
  exit 0
fi

# Collect the diff to scan.
case "$mode" in
  commit)
    diff_text=$(git diff --cached --no-color 2>/dev/null || true)
    ;;
  push)
    # Compare against the configured upstream. If none, fall back to origin/main.
    if upstream=$(git rev-parse --abbrev-ref --symbolic-full-name '@{u}' 2>/dev/null); then
      base="$upstream"
    else
      base="origin/main"
    fi
    if ! git rev-parse --verify "$base" >/dev/null 2>&1; then
      # No upstream and no origin/main — nothing to compare against.
      exit 0
    fi
    diff_text=$(git diff --no-color "$base"...HEAD 2>/dev/null || true)
    ;;
esac

if [ -z "$diff_text" ]; then
  exit 0
fi

# Patterns. Each entry: "label|extended-regex".
# Add new patterns at the bottom; keep them anchored enough to avoid noise.
patterns=(
  # Homelab DNS reservation. The .home.arpa zone is RFC8375 — fine in code, but
  # specific personal subdomains shouldn't ship publicly.
  "personal home.arpa hostname|[A-Za-z0-9_-]+\.home\.arpa\b"

  # Personal name fragments. Tighten or relax for your own forks.
  "personal email (gmail)|[A-Za-z0-9._%+-]+@(gmail|googlemail)\.com"

  # 1Password vault paths leak vault/item naming structure.
  "1Password vault path|vaults/[A-Za-z0-9_-]+/items/[A-Za-z0-9_-]+"

  # Tailscale CGNAT IP space (100.64.0.0/10). Routable only on your tailnet.
  "Tailscale CGNAT IP|\\b100\\.(6[4-9]|[7-9][0-9]|1[01][0-9]|12[0-7])\\.[0-9]{1,3}\\.[0-9]{1,3}\\b"

  # Long hex tokens on a line — likely DINGDONG_TOKEN / openssl rand -hex output.
  # 32+ hex chars in a row. Excludes SHA-1/SHA-256 fingerprints embedded in URLs
  # by requiring the token to look standalone (not part of a longer alnum word).
  "long hex token literal|(^|[^A-Za-z0-9])[a-f0-9]{32,}([^A-Za-z0-9]|$)"

  # Real personal hostname/machine names that shouldn't be in examples. Extend.
  "personal machine name|\\b(mbp|mba|mac.studio|mac-studio|bryans-mac)\\b"
)

violations=()

# Walk the diff one file at a time so we can skip the scanner script itself
# (which legitimately contains every pattern it's looking for) and only inspect
# added lines from each file's hunk.
current_file=""
declare -a per_file_added=()

flush_file() {
  local file="$1"
  shift
  local lines=("$@")
  if [ -z "$file" ] || [ ${#lines[@]} -eq 0 ]; then
    return
  fi
  # The scanner itself defines every pattern it matches — exclude it.
  case "$file" in
    .claude/scripts/check-public-safe.sh) return ;;
  esac
  local joined
  joined=$(printf '%s\n' "${lines[@]}")
  for entry in "${patterns[@]}"; do
    local label="${entry%%|*}"
    local regex="${entry#*|}"
    local hits
    if hits=$(printf '%s\n' "$joined" | grep -nE -i "$regex" 2>/dev/null); then
      if [ -n "$hits" ]; then
        # Filter out lines that are obvious placeholders meant to be replaced.
        local filtered
        filtered=$(printf '%s\n' "$hits" | grep -vE 'REPLACE_ME|<your-[a-z-]+>|EXAMPLE_ONLY' || true)
        if [ -n "$filtered" ]; then
          violations+=("→ $label  ($file)")
          while IFS= read -r line; do
            violations+=("    $line")
          done <<<"$filtered"
        fi
      fi
    fi
  done
}

while IFS= read -r line; do
  if [[ "$line" == "diff --git "* ]]; then
    flush_file "$current_file" "${per_file_added[@]:-}"
    current_file=$(printf '%s' "$line" | sed -E 's@^diff --git a/(.+) b/.+$@\1@')
    per_file_added=()
    continue
  fi
  # Only collect added lines (strip leading +). Skip the diff's own +++ header.
  if [[ "$line" == "+++ "* ]]; then
    continue
  fi
  if [[ "$line" == +* ]]; then
    per_file_added+=("${line:1}")
  fi
done <<<"$diff_text"
flush_file "$current_file" "${per_file_added[@]:-}"

if [ ${#violations[@]} -gt 0 ]; then
  {
    echo "BLOCKED: public-repo safety scan matched personal/internal patterns"
    echo "  mode: $mode"
    printf '  %s\n' "${violations[@]}"
    echo
    echo "If a match is intentional and safe to publish, override for one operation:"
    echo "  DINGDONG_PUBLIC_OVERRIDE=1 git $mode ..."
    echo "Or refine the pattern in .claude/scripts/check-public-safe.sh."
  } >&2
  exit 2
fi

exit 0
