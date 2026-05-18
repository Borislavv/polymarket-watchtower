// Package main is the watchtower cli. Sub-commands are exposed via the
// first positional argument:
//
//   - migrate          — apply embedded SQL migrations against a DSN.
//   - diagnose-alerts  — connect to a live DB and print a gate-by-gate
//     breakdown that explains, with real counts, why a given config
//     either produces or suppresses alerts. Hits the database read-only.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "migrate":
		runMigrate(os.Args[2:])
	case "diagnose-alerts":
		runDiagnoseAlerts(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: cli <migrate|diagnose-alerts> [flags]")
	fmt.Fprintln(os.Stderr, "  migrate          -dsn postgres://…")
	fmt.Fprintln(os.Stderr, "  diagnose-alerts  -dsn postgres://… [--lookback 24h]")
}

func runMigrate(args []string) {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("POSTGRES_DSN"), "Postgres DSN (defaults to $POSTGRES_DSN)")
	_ = fs.Parse(args)
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "migrate: -dsn or $POSTGRES_DSN required")
		os.Exit(2)
	}
	if err := postgres.Migrate(*dsn); err != nil {
		fmt.Fprintf(os.Stderr, "migrate: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("migrate: ok")
}
