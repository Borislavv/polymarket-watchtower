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
		// v6 path uses the production scorer (analytics/score.Score)
		// so the projected fire counts match exactly what the
		// watchtower binary would emit. The legacy v4 multiplier-only
		// diagnose has been removed — call runDiagnoseAlertsV6 only.
		runDiagnoseAlertsV6(os.Args[2:])
	case "diagnose-alerts-v4":
		// Kept for one release as an operator escape hatch in case
		// they want the old multiplier-ladder breakdown. Will be
		// removed after the v6 path proves out.
		runDiagnoseAlerts(os.Args[2:])
	case "diagnose-ai":
		// Quick "is AI working?" smoke: prints the latest 20 alert
		// rows + the latest 20 alert-analysis rows (status, verdict,
		// truncated text, error). The two queries together tell an
		// operator whether alerts are being created and whether the
		// AI enricher is producing usable output.
		runDiagnoseAI(os.Args[2:])
	case "import-catalysts":
		// One-shot Political-Catalyst Intelligence import for a
		// single event slug. Fetches the event page, runs AI
		// extraction (when the key is wired), and prints the
		// extracted catalysts. Defaults to DRY RUN — no DB writes —
		// unless --persist is set.
		runImportCatalysts(os.Args[2:])
	case "evolve-predictions":
		// One-shot Prediction Evolution dry-run. Runs ONE cycle of
		// the evolution worker against a Postgres DSN, printing the
		// per-prediction summary (old/new state, AI refresh decision,
		// repricing status, matched alerts, decay, Telegram).
		runEvolvePredictions(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: cli <migrate|diagnose-alerts|diagnose-ai|import-catalysts> [flags]")
	fmt.Fprintln(os.Stderr, "  migrate          -dsn postgres://…")
	fmt.Fprintln(os.Stderr, "  diagnose-alerts  -dsn postgres://… [--lookback 24h]")
	fmt.Fprintln(os.Stderr, "  diagnose-ai      -dsn postgres://… [--limit 20]")
	fmt.Fprintln(os.Stderr, "  import-catalysts --event <slug> [--persist] [--ai-key $OPENAI_API_KEY] [--model gpt-4.1-mini]")
	fmt.Fprintln(os.Stderr, "  evolve-predictions --dsn postgres://… [--once] [--limit 10] [--dry-run]")
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
