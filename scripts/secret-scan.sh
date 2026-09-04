#!/usr/bin/env bash
#
# secret-scan.sh — fail the build if likely credentials appear in tracked files.
#
# This is a lightweight, dependency-free gate (part of `make release-check`). It
# scans only git-tracked files for high-signal secret patterns. Test fixtures use
# obviously-fake values (no PEM blocks, no vendor key prefixes, no full raw API
# keys), so they do not match; real secrets live only in deploy/.env, which is
# gitignored and must never be committed.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

# High-signal patterns: matching one of these in a tracked file is almost always
# a real leaked credential.
patterns=(
  '-----BEGIN [A-Z ]*PRIVATE KEY-----'  # PEM private keys
  'AKIA[0-9A-Z]{16}'                    # AWS access key id
  'gh[oprsu]_[A-Za-z0-9]{36}'           # GitHub tokens (ghp_/gho_/ghs_/ghr_/ghu_)
  'xox[baprs]-[A-Za-z0-9-]{10,}'        # Slack tokens
  'AIza[0-9A-Za-z_-]{35}'               # Google API key
  'qlk_[A-Za-z0-9_-]{32,}'              # QuorumLimiter raw API keys (real ones)
)

# Exclusions: this script itself (it contains the patterns above), the example
# env file (placeholders only), and test files (dummy fixtures).
excludes=( ':!scripts/secret-scan.sh' ':!deploy/.env.example' ':!*_test.go' )

found=0
for p in "${patterns[@]}"; do
  hits="$(git grep -nIE -e "$p" -- "${excludes[@]}" || true)"
  if [ -n "$hits" ]; then
    echo "secret-scan: potential secret matching /$p/:"
    echo "$hits"
    found=1
  fi
done

# A tracked deploy/.env would carry real secrets — it must stay gitignored.
if git ls-files --error-unmatch deploy/.env >/dev/null 2>&1; then
  echo "secret-scan: deploy/.env is tracked by git — it must remain gitignored"
  found=1
fi

if [ "$found" -ne 0 ]; then
  echo "secret-scan: FAILED"
  exit 1
fi
echo "secret-scan: OK (no secrets in tracked files)"
