// Package db exposes the SQL migration files as an embedded filesystem so
// the watchtower binary is self-contained at runtime. The same files are
// the source for sqlc generation (`sqlc.yaml` schema dir) and for the
// migrate runner in internal/infra/postgres.
package db

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS
