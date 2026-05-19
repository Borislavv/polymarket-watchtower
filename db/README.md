# Database layer — schema, queries, sqlc

This directory is the single source of truth for the watchtower's
PostgreSQL schema and queries:

```
db/
├── migrations/         versioned schema (00001_…up.sql, 00001_…down.sql, …)
├── queries/            sqlc-fed query files (alerts.sql, trades.sql, …)
└── README.md           this file
```

The Go bindings live one directory over at
`internal/infra/postgres/sqlc` — every file there is **generated
output**. Editing those files by hand is a regression in disguise
because the next `sqlc generate` will silently overwrite the
modification.

## The contract

1. **Schema changes** go in `db/migrations/<NNNNN>_<slug>.up.sql` (and
   the matching `.down.sql`). The embedded migrator
   (`internal/infra/postgres/migrate.go` over the `db.Migrations`
   embed) applies them at boot.
2. **Query changes** go in `db/queries/*.sql` using sqlc's
   `-- name: <Name> :exec|:one|:many|:execrows` directive.
3. **After every change**, regenerate the Go bindings:

   ```bash
   make sqlc
   ```

4. **Commit the regenerated `internal/infra/postgres/sqlc/*.go` along
   with your SQL change.** Reviewers see exactly what runtime code
   the SQL produced.
5. **CI** runs `make sqlc-check`, which regenerates and fails if the
   working tree has uncommitted diffs in the sqlc output, queries,
   or migrations. This catches hand-patches.

## Local toolchain options

### Option 1 — install `sqlc` (recommended)

```bash
# macOS / Linux with Homebrew
brew install sqlc

# Linux without brew
go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest

# Verify
sqlc version
make sqlc
```

`go install` puts the binary at `$(go env GOPATH)/bin/sqlc` — ensure
that's on your `PATH`.

### Option 2 — Docker (zero install)

When `sqlc` is not on the developer's `PATH` (CI runners, fresh
laptops), use the official image:

```bash
make sqlc-docker
```

This runs `sqlc/sqlc:latest generate` against the project root via a
bind mount. Identical output to a local install.

### `make sqlc-check`

Used in CI and recommended pre-commit. Regenerates and fails if any
file in `internal/infra/postgres/sqlc/`, `db/queries/`, or
`db/migrations/` would have a diff after the regenerate. Prefers a
locally installed `sqlc`; falls back to Docker.

```bash
make sqlc-check
```

## Why this matters

Historical context: an earlier generation of this codebase hand-
patched generated `*.sql.go` files because no operator had `sqlc`
installed. The hand-patches were correct, but every new query
became a multi-file dance and the contract "this directory is
generated" broke. The Makefile targets and CI guard above prevent
recurrence.

## Adding a query — full workflow

```bash
# 1. Edit (or create) the query file
$EDITOR db/queries/alerts.sql

# 2. Regenerate bindings
make sqlc        # or: make sqlc-docker

# 3. Use the new method in a repository wrapper
$EDITOR internal/infra/repository/alert_repository.go

# 4. Add a unit test
$EDITOR internal/infra/repository/repository_test.go

# 5. Sanity check
go build ./...
go test ./...
git diff           # the diff should be SQL + generated + repo + test
```

## Migration ordering

Migrations are numeric. New migrations get the next number. The
`down.sql` MUST cleanly reverse the `up.sql`:

```bash
# Quick smoke test (against a throwaway DB)
make pg-up
docker exec -i watchtower-postgres psql -U watchtower -d watchtower < db/migrations/00009_my_change.up.sql
docker exec -i watchtower-postgres psql -U watchtower -d watchtower < db/migrations/00009_my_change.down.sql
```

The integration tests (`make pg-test`) run against a Postgres
container with all migrations applied; failing tests after a
migration change are usually a clue that the new schema doesn't
match the repository code or that the down migration is incomplete.
