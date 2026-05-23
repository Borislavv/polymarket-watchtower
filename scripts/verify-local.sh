#!/usr/bin/env bash
#
# scripts/verify-local.sh — v11.5 deterministic local quality gate.
# Watchtower has no CI today; this script is the single
# pre-merge gate every contributor runs.
#
# Steps (each MUST succeed; the script aborts on first failure):
#
#   1. toolchain check — go version must match go.mod
#   2. go mod verify
#   3. gofmt -l . (no diff allowed)
#   4. go vet ./...
#   5. go test ./...
#   6. golangci-lint run ./... (skipped if binary not on PATH;
#      surfaces a warning rather than failing)
#   7. migration sanity — db/migrations/*.up.sql must parse via
#      psql against the local docker-compose database. Skipped if
#      the watchtower-postgres container isn't running.
#
# Usage:
#
#   bash scripts/verify-local.sh
#
# Exit codes:
#   0 — all gates green
#   1 — one or more gates failed
#   2 — usage / setup error
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

# --- 0. log helpers ---
PASS="\033[1;32m✓\033[0m"
FAIL="\033[1;31m✗\033[0m"
INFO="\033[1;34m·\033[0m"
log_step()  { printf "%b %s\n" "$INFO" "$1"; }
log_pass()  { printf "%b %s\n" "$PASS" "$1"; }
log_fail()  { printf "%b %s\n" "$FAIL" "$1"; exit 1; }

# --- 1. toolchain check ---
log_step "toolchain check"
WANT_GO="$(awk '/^go [0-9]/ { print $2 }' go.mod)"
GOT_GO="$(go env GOVERSION | sed 's/^go//')"
if [ -z "${WANT_GO}" ]; then
  log_fail "go.mod missing 'go X.Y.Z' directive"
fi
if [ "${WANT_GO}" != "${GOT_GO}" ]; then
  # We do NOT auto-downgrade go.mod. The authoritative version is
  # the module's. If it doesn't match the local toolchain we tell
  # the operator how to fix it and abort.
  log_fail "toolchain mismatch: go.mod wants ${WANT_GO}; got ${GOT_GO}. Use container/devshell with the required version. Do NOT lower go.mod."
fi
log_pass "go ${GOT_GO} matches go.mod"

# --- 2. go mod verify ---
log_step "go mod verify"
go mod verify > /dev/null
log_pass "go mod verify"

# --- 3. gofmt ---
log_step "gofmt -l ."
DIFF="$(gofmt -l .)"
if [ -n "${DIFF}" ]; then
  echo "${DIFF}"
  log_fail "gofmt diff above"
fi
log_pass "gofmt clean"

# --- 4. go vet ---
log_step "go vet ./..."
go vet ./...
log_pass "go vet clean"

# --- 5. go test ---
log_step "go test ./..."
if ! go test ./... > /tmp/watchtower-verify.testlog 2>&1; then
  cat /tmp/watchtower-verify.testlog
  log_fail "go test failed"
fi
PKGS="$(grep -cE '^ok\s' /tmp/watchtower-verify.testlog || true)"
FAILS="$(grep -cE '^FAIL' /tmp/watchtower-verify.testlog || true)"
if [ "${FAILS}" != "0" ]; then
  cat /tmp/watchtower-verify.testlog
  log_fail "${FAILS} package(s) failed tests"
fi
log_pass "go test green (${PKGS} packages)"

# --- 6. golangci-lint (advisory) ---
#
# Watchtower carries pre-existing lint debt in legacy packages
# (errcheck/staticcheck/unused) unrelated to any single feature
# change. We surface the report loudly but do NOT abort the gate
# on it. The hard gates are go vet + go test; lint is advisory
# until the legacy debt is paid down.
#
# v11.5 code paths MUST stay lint-clean — the warning grep below
# explicitly fails if any v11.5 package shows up in the report.
if command -v golangci-lint > /dev/null 2>&1; then
  log_step "golangci-lint run ./... (advisory)"
  if golangci-lint run ./... > /tmp/watchtower-verify.lintlog 2>&1; then
    log_pass "golangci-lint clean"
  else
    LINT_COUNT="$(grep -cE ':[0-9]+:' /tmp/watchtower-verify.lintlog || true)"
    V11_5_LINT="$(grep -E '/(thesisaccum|holderdelta|catalystwindow|bookvacuum|repricinglag|walletcohort|conflictresolve|rulesrisk|cheaptail|shadowdecisions|strategybus|marketlinks|holdersync|riskscore|repricing|walletgraph)/|shadow_decisions_repository\.go|strategy_shadow\.sql\.go|strategy_config\.go|strategy_wiring\.go|strategy_config_test\.go|strategy_wiring_test\.go' /tmp/watchtower-verify.lintlog || true)"
    if [ -n "${V11_5_LINT}" ]; then
      echo "${V11_5_LINT}"
      log_fail "golangci-lint flagged v11.5 code — must be clean"
    fi
    printf "%b golangci-lint: %s pre-existing legacy issue(s); v11.5 code clean\n" "$INFO" "${LINT_COUNT}"
    printf "%b run \`golangci-lint run ./...\` to see the list. Not a hard fail.\n" "$INFO"
  fi
else
  log_step "golangci-lint: SKIP (binary not on PATH — gap; fix when possible)"
fi

# --- 7. migration sanity (optional if DB container is up) ---
log_step "migration sanity (Postgres)"
if docker ps --format '{{.Names}}' 2>/dev/null | grep -q '^watchtower-postgres$'; then
  if docker exec -e PGPASSWORD=watchtower watchtower-postgres \
      psql -U watchtower -d watchtower -c 'SELECT 1;' > /dev/null 2>&1; then
    # Apply every unapplied migration via the CLI; the CLI itself
    # is idempotent + golang-migrate handles versioning.
    if go run ./cmd/cli migrate -dsn "postgres://watchtower:watchtower@localhost:5433/watchtower?sslmode=disable" \
        > /tmp/watchtower-verify.miglog 2>&1; then
      log_pass "migrations applied / idempotent"
    else
      cat /tmp/watchtower-verify.miglog
      log_fail "migration apply failed"
    fi
  else
    log_step "Postgres container present but unresponsive — SKIP"
  fi
else
  log_step "watchtower-postgres container not running — SKIP (start via: docker compose -f deploy/docker-compose.yml up -d postgres)"
fi

echo
log_pass "verify-local: all green"
