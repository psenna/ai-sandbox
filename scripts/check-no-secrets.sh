#!/usr/bin/env bash
# check-no-secrets.sh — fail if a real secret appears in git-tracked files.
#
# A backstop against committing credentials. git-proxy holds the real GitHub PAT;
# this repo should only ever contain placeholders (REPLACE_WITH_GITHUB_PAT,
# your_ollama_cloud_api_key, agent-token-1). This script scans tracked files for
# high-signal secret patterns and exits non-zero if any look real.
#
# Run manually before committing:
#   bash scripts/check-no-secrets.sh
# Or wire it as a git pre-commit hook:
#   ln -s ../../scripts/check-no-secrets.sh .git/hooks/pre-commit
set -euo pipefail

cd "$(git rev-parse --show-toplevel 2>/dev/null || echo .)"

# Patterns that indicate a REAL secret (not a placeholder). ERE syntax — braces
# are NOT escaped. High-signal enough that the placeholder strings
# (REPLACE_WITH_GITHUB_PAT, your_ollama_cloud_api_key, agent-token-1,
# "AAAAAAAAAAAAA") do not match.
patterns=(
  'ghp_[0-9A-Za-z]{36,}'               # GitHub classic PAT
  'github_pat_[0-9A-Za-z_]{20,}'       # GitHub fine-grained PAT
  'gho_[0-9A-Za-z]{36,}'              # GitHub OAuth token
  'ghs_[0-9A-Za-z]{36,}'              # GitHub app token
  'ghr_[0-9A-Za-z]{36,}'              # GitHub refresh token
  'sk-[0-9A-Za-z]{20,}'               # OpenAI-style key
  '-----BEGIN (RSA |EC |OPENSSH |DSA |)PRIVATE KEY-----'
)

# Scan only files git tracks (respects .gitignore; ignores .git).
mapfile -t files < <(git ls-files 2>/dev/null || true)
if [[ ${#files[@]} -eq 0 ]]; then
  printf '\033[1;33m[no-secrets] no tracked files to scan\033[0m\n'
  exit 0
fi

status=0
for pat in "${patterns[@]}"; do
  matches=$(grep -En "$pat" "${files[@]}" 2>/dev/null || true)
  if [[ -n "$matches" ]]; then
    printf '\033[1;31m[no-secrets] possible secret found (pattern: %s)\033[0m\n' "$pat" >&2
    printf '%s\n' "$matches" >&2
    status=1
  fi
done

if [[ $status -ne 0 ]]; then
  printf '\n\033[1;31m[no-secrets] FAILED\033[0m — remove/replace the real secret(s) above with placeholders before committing.\n' >&2
else
  printf '\033[1;32m[no-secrets] ok\033[0m — no real-secret patterns in tracked files.\n'
fi
exit $status