#!/usr/bin/env bash
#
# scripts/audit-env.sh — v11.9 deterministic config audit.
#
# Compares `.env`, `.env.example`, and the env tags declared in
# internal/app/config.go + internal/app/strategy_config.go. Prints
# three tables:
#
#   1. Keys in .env but NOT in .env.example
#   2. Keys in .env.example but NOT in .env
#   3. Keys in either file but with no env-tag binding in config
#
# Exits 0 when (1) and (2) are empty (the only NON-NEGOTIABLE
# invariant) — drift between the two files is a config-safety bug.
# Unbound keys in (3) are surfaced as a warning, not a hard fail,
# because legacy stale env keys are intentionally listed in
# staleEnvKeys{} for explicit rejection at boot.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PASS="\033[1;32m✓\033[0m"
FAIL="\033[1;31m✗\033[0m"
WARN="\033[1;33m!\033[0m"

env_keys() {
  grep -E "^[A-Z][A-Z0-9_]+=" "$1" | awk -F= '{print $1}' | sort -u
}

config_keys() {
  grep -rohE 'env:"[A-Z_]+"' internal/app/config.go internal/app/strategy_config.go \
    | sed 's/env:"\(.*\)"/\1/' | sort -u
}

ENV_KEYS=$(env_keys .env)
ENV_EXAMPLE_KEYS=$(env_keys .env.example)
CONFIG_KEYS=$(config_keys)

ONLY_IN_ENV="$(comm -23 <(echo "$ENV_KEYS") <(echo "$ENV_EXAMPLE_KEYS"))"
ONLY_IN_EXAMPLE="$(comm -13 <(echo "$ENV_KEYS") <(echo "$ENV_EXAMPLE_KEYS"))"
UNBOUND_KEYS="$(comm -23 <(echo "$ENV_KEYS") <(echo "$CONFIG_KEYS"))"

EXIT=0
if [ -n "$ONLY_IN_ENV" ]; then
  printf "%b keys in .env missing from .env.example:\n" "$FAIL"
  printf "  %s\n" $ONLY_IN_ENV
  EXIT=1
fi
if [ -n "$ONLY_IN_EXAMPLE" ]; then
  printf "%b keys in .env.example missing from .env:\n" "$FAIL"
  printf "  %s\n" $ONLY_IN_EXAMPLE
  EXIT=1
fi
if [ -z "$ONLY_IN_ENV" ] && [ -z "$ONLY_IN_EXAMPLE" ]; then
  printf "%b .env / .env.example fully synchronized\n" "$PASS"
fi

if [ -n "$UNBOUND_KEYS" ]; then
  UNBOUND_COUNT="$(echo "$UNBOUND_KEYS" | wc -l | tr -d ' ')"
  printf "%b %s key(s) in .env with no env-tag in config (likely legacy / stale — verify against staleEnvKeys{}):\n" "$WARN" "$UNBOUND_COUNT"
  echo "$UNBOUND_KEYS" | head -20 | sed 's/^/  /'
fi

exit $EXIT
