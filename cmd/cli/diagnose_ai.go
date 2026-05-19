// Operator-facing "is the AI alert path actually working?" diagnostic.
// Two queries, read-only:
//
//  1. Latest N alert-analysis rows — Status / Verdict / model /
//     truncated text / LastError / created_at. The single best way
//     to confirm AnalyzeAndStore is being called AND seeing what it
//     produces.
//  2. Latest N alert rows — id / severity / kind / status. Tells
//     you whether the upstream pipeline is creating alerts at all.
//
// Run as:
//
//	go run ./cmd/cli diagnose-ai -dsn "$POSTGRES_DSN" -limit 20
//
// Output is plain text — pipe into grep/jq/less. No prompts, no
// secrets.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

func runDiagnoseAI(args []string) {
	fs := flag.NewFlagSet("diagnose-ai", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("POSTGRES_DSN"), "Postgres DSN (defaults to $POSTGRES_DSN)")
	limit := fs.Int("limit", 20, "rows per section")
	_ = fs.Parse(args)
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "diagnose-ai: -dsn or $POSTGRES_DSN required")
		os.Exit(2)
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "diagnose-ai: connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	if err := dumpAlertAnalyses(ctx, pool, *limit); err != nil {
		fmt.Fprintf(os.Stderr, "diagnose-ai: alert_analyses: %v\n", err)
		os.Exit(1)
	}
	fmt.Println()
	if err := dumpAlerts(ctx, pool, *limit); err != nil {
		fmt.Fprintf(os.Stderr, "diagnose-ai: alerts: %v\n", err)
		os.Exit(1)
	}
}

func dumpAlertAnalyses(ctx context.Context, pool *pgxpool.Pool, limit int) error {
	const q = `
SELECT alert_id, version, status,
       COALESCE(verdict, '') AS verdict,
       model,
       COALESCE(left(analysis_text, 120), '') AS text_preview,
       COALESCE(last_error, '') AS last_error,
       created_at
  FROM polymarket_alert_analyses
 ORDER BY created_at DESC
 LIMIT $1`
	rows, err := pool.Query(ctx, q, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Printf("=== polymarket_alert_analyses (latest %d) ===\n", limit)
	fmt.Println("alert_id  v  status   verdict      model              text_preview                                                                                                  last_error                created_at")
	count := 0
	for rows.Next() {
		var alertID int64
		var version int32
		var status, verdict, model, text, lastErr string
		var createdAt any
		if err := rows.Scan(&alertID, &version, &status, &verdict, &model, &text, &lastErr, &createdAt); err != nil {
			return err
		}
		fmt.Printf("%-8d  %-2d %-8s %-12s %-18s %-150s %-25s %v\n",
			alertID, version, status, trunc(verdict, 12), trunc(model, 18),
			trunc(strings.ReplaceAll(text, "\n", " "), 150),
			trunc(lastErr, 25), createdAt)
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count == 0 {
		fmt.Println("(no rows — AI service has not persisted any analysis yet)")
	}
	return nil
}

func dumpAlerts(ctx context.Context, pool *pgxpool.Pool, limit int) error {
	const q = `
SELECT id, created_at, severity, kind, status,
       COALESCE(last_send_error, '') AS last_send_error
  FROM polymarket_alerts
 ORDER BY id DESC
 LIMIT $1`
	rows, err := pool.Query(ctx, q, limit)
	if err != nil {
		return err
	}
	defer rows.Close()
	fmt.Printf("=== polymarket_alerts (latest %d) ===\n", limit)
	fmt.Println("id        created_at                      severity   kind                      status          last_send_error")
	count := 0
	for rows.Next() {
		var id int64
		var createdAt any
		var severity, kind, status, lastSendErr string
		if err := rows.Scan(&id, &createdAt, &severity, &kind, &status, &lastSendErr); err != nil {
			return err
		}
		fmt.Printf("%-8d  %-30v  %-8s   %-24s  %-13s   %s\n",
			id, createdAt, severity, kind, status, trunc(lastSendErr, 80))
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count == 0 {
		fmt.Println("(no rows — pipeline has not produced any alerts yet)")
	}
	return nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}
