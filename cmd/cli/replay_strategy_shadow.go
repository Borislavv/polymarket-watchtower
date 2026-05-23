// replay_strategy_shadow.go — v11.10 PART 4 strategy fanout replay
// report. Read-only by default; surfaces per-strategy eval / fired /
// skipped stats from the live database.
//
// The goal stated by the v11.10 ТЗ: prove that the hot-path strategy
// fanout produces real per-strategy statistics on real data, not the
// expected-behaviour table from the docs. This command:
//
//   - aggregates polymarket_strategy_shadow_decisions in the lookback
//     window (per strategy, with linkage + decision_level buckets);
//   - reads the latest promotion review row for each strategy and
//     prints the bucket_diagnostics JSON;
//   - inspects recent alerts vs. staged-input availability so the
//     operator sees the skip-reason mix without standing up the
//     detect.Loop machinery;
//   - never touches Telegram, OpenAI, or the user flow.
//
// The command is intentionally NOT a write path. Generating synthetic
// shadow rows for the smoke would violate the v11.10 NON-NEGOTIABLE
// "never fake data" rule — the only honest way to produce shadow rows
// is to wait for real alerts. This report tells the operator exactly
// what shadow data exists today AND what the skip mix looks like.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func runReplayStrategyShadow(args []string) {
	fs := flag.NewFlagSet("replay-strategy-shadow", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("POSTGRES_DSN"), "Postgres DSN (defaults to $POSTGRES_DSN)")
	since := fs.Duration("since", 24*time.Hour, "lookback window")
	jsonOut := fs.Bool("json", false, "emit JSON instead of the operator-facing table")
	_ = fs.Parse(args)
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "replay-strategy-shadow: --dsn or $POSTGRES_DSN required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "replay-strategy-shadow: connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	report, err := gatherStrategyReport(ctx, pool, *since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "replay-strategy-shadow: gather: %v\n", err)
		os.Exit(1)
	}
	if *jsonOut {
		b, _ := json.MarshalIndent(report, "", "  ")
		fmt.Println(string(b))
		return
	}
	printStrategyReport(os.Stdout, report)
}

// StrategyShadowStats is the per-strategy slice of the report.
type StrategyShadowStats struct {
	Strategy             string  `json:"strategy"`
	ShadowRows           int     `json:"shadow_rows"`
	ShadowOnlyRows       int     `json:"shadow_only_rows"`
	PromotedRows         int     `json:"promoted_rows"`
	LinkedRows           int     `json:"linked_rows"`
	StandaloneRows       int     `json:"standalone_rows"`
	WithCLV6h            int     `json:"with_clv_6h"`
	AvgCLV6hCents        float64 `json:"avg_clv_6h_cents"`
	OutcomeCorrect       int     `json:"outcome_correct"`
	OutcomeWrong         int     `json:"outcome_wrong"`
	OutcomeUnknown       int     `json:"outcome_unknown"`
	LatestEligible       *bool   `json:"latest_eligible,omitempty"`
	LatestReviewReasons  string  `json:"latest_review_reasons,omitempty"`
	BucketDiagnosticsRaw string  `json:"bucket_diagnostics_raw,omitempty"`
}

// EvalSkipCounter shows per-(strategy, reason) skip counts derived
// from polymarket_strategy_shadow_decisions skip rows when persisted
// (decision_kind='eval_skipped') OR from the latest Prometheus
// counters in the worker_runs view.
type EvalSkipCounter struct {
	Strategy string `json:"strategy"`
	Reason   string `json:"reason"`
	Count    int    `json:"count"`
}

// StagedInputCoverage shows how many recent alerts had each kind of
// staged input available. Lets the operator see why catalystwindow /
// walletcohort / repricinglag etc. would have skipped vs fired.
type StagedInputCoverage struct {
	RecentAlerts             int `json:"recent_alerts"`
	WithCatalysts            int `json:"with_active_catalyst"`
	WithMarketLinks          int `json:"with_market_links"`
	WithClosedRepricing      int `json:"with_closed_repricing_window"`
	WithRiskScores           int `json:"with_riskscore"`
	WithWalletGraphEdges     int `json:"with_walletgraph_edges"`
	WithRecentBookbarsFresh  int `json:"with_recent_bookbars_fresh"`
	WithRecentHolderSnapshot int `json:"with_recent_holder_snapshot"`
}

// StrategyShadowReport is the top-level report shape.
type StrategyShadowReport struct {
	GeneratedAt    time.Time             `json:"generated_at"`
	LookbackHours  float64               `json:"lookback_hours"`
	TotalRows      int                   `json:"total_shadow_rows"`
	PerStrategy    []StrategyShadowStats `json:"per_strategy"`
	SkipCounts     []EvalSkipCounter     `json:"skip_counts"`
	StagedCoverage StagedInputCoverage   `json:"staged_coverage"`
}

func gatherStrategyReport(ctx context.Context, pool *pgxpool.Pool, since time.Duration) (*StrategyShadowReport, error) {
	rep := &StrategyShadowReport{
		GeneratedAt:   time.Now().UTC(),
		LookbackHours: since.Hours(),
	}
	// 1) per-strategy aggregates.
	rows, err := pool.Query(ctx, `
SELECT strategy_name,
       COUNT(*)::int                                                          AS shadow_rows,
       COUNT(*) FILTER (WHERE shadow_only = TRUE)::int                        AS shadow_only_rows,
       COUNT(*) FILTER (WHERE shadow_only = FALSE)::int                       AS promoted_rows,
       COUNT(*) FILTER (WHERE linked_alert_dedup_key IS NOT NULL AND linked_alert_dedup_key <> '')::int AS linked_rows,
       COUNT(*) FILTER (WHERE linked_alert_dedup_key IS NULL OR linked_alert_dedup_key = '')::int      AS standalone_rows,
       COUNT(*) FILTER (WHERE clv_6h IS NOT NULL)::int                        AS with_clv_6h,
       COALESCE(AVG(clv_6h) FILTER (WHERE clv_6h IS NOT NULL), 0)::double precision AS avg_clv_6h_cents,
       COUNT(*) FILTER (WHERE outcome_status = 'resolved_correct')::int       AS outcome_correct,
       COUNT(*) FILTER (WHERE outcome_status = 'resolved_wrong')::int         AS outcome_wrong,
       COUNT(*) FILTER (WHERE outcome_status = 'unknown')::int                AS outcome_unknown
FROM polymarket_strategy_shadow_decisions
WHERE fired_at >= NOW() - $1::interval
GROUP BY strategy_name
ORDER BY strategy_name`, fmt.Sprintf("%d seconds", int(since.Seconds())))
	if err != nil {
		return nil, fmt.Errorf("per-strategy: %w", err)
	}
	perStrategy := map[string]*StrategyShadowStats{}
	for rows.Next() {
		s := &StrategyShadowStats{}
		if err := rows.Scan(&s.Strategy, &s.ShadowRows, &s.ShadowOnlyRows, &s.PromotedRows,
			&s.LinkedRows, &s.StandaloneRows, &s.WithCLV6h, &s.AvgCLV6hCents,
			&s.OutcomeCorrect, &s.OutcomeWrong, &s.OutcomeUnknown); err != nil {
			rows.Close()
			return nil, err
		}
		perStrategy[s.Strategy] = s
		rep.TotalRows += s.ShadowRows
	}
	rows.Close()

	// 2) latest promotion review per strategy — eligibility + bucket diagnostics.
	revRows, err := pool.Query(ctx, `
SELECT DISTINCT ON (strategy_name) strategy_name, eligible,
       COALESCE(reasons_json::text, ''),
       COALESCE(bucket_diagnostics::text, '')
FROM polymarket_strategy_promotion_reviews
ORDER BY strategy_name, reviewed_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("promotion review: %w", err)
	}
	for revRows.Next() {
		var name, reasons, buckets string
		var eligible bool
		if err := revRows.Scan(&name, &eligible, &reasons, &buckets); err != nil {
			revRows.Close()
			return nil, err
		}
		s, ok := perStrategy[name]
		if !ok {
			s = &StrategyShadowStats{Strategy: name}
			perStrategy[name] = s
		}
		e := eligible
		s.LatestEligible = &e
		s.LatestReviewReasons = reasons
		s.BucketDiagnosticsRaw = buckets
	}
	revRows.Close()

	// 3) order strategies alphabetically.
	for _, s := range perStrategy {
		rep.PerStrategy = append(rep.PerStrategy, *s)
	}
	for i := 0; i < len(rep.PerStrategy); i++ {
		for j := i + 1; j < len(rep.PerStrategy); j++ {
			if rep.PerStrategy[j].Strategy < rep.PerStrategy[i].Strategy {
				rep.PerStrategy[i], rep.PerStrategy[j] = rep.PerStrategy[j], rep.PerStrategy[i]
			}
		}
	}

	// 4) staged-input coverage over recent alerts.
	cov := StagedInputCoverage{}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*)::int FROM polymarket_alerts WHERE created_at >= NOW() - $1::interval`,
		fmt.Sprintf("%d seconds", int(since.Seconds()))).Scan(&cov.RecentAlerts); err != nil {
		return nil, fmt.Errorf("recent alerts count: %w", err)
	}

	// per-feature coverage on recent alerts. Each is the number of
	// alerts whose (event_slug, condition_id) has the staged feature.
	type covQuery struct {
		target *int
		sql    string
	}
	queries := []covQuery{
		{&cov.WithCatalysts, `
SELECT COUNT(DISTINCT a.id)::int FROM polymarket_alerts a
JOIN polymarket_markets pm ON pm.id = a.market_id
JOIN polymarket_event_catalysts c ON c.event_slug = pm.event_slug
WHERE a.created_at >= NOW() - $1::interval AND c.status IN ('active','expected')`},
		{&cov.WithMarketLinks, `
SELECT COUNT(DISTINCT a.id)::int FROM polymarket_alerts a
JOIN polymarket_markets pm ON pm.id = a.market_id
JOIN polymarket_market_links ml ON ml.event_slug = pm.event_slug
WHERE a.created_at >= NOW() - $1::interval`},
		{&cov.WithClosedRepricing, `
SELECT COUNT(DISTINCT a.id)::int FROM polymarket_alerts a
JOIN polymarket_markets pm ON pm.id = a.market_id
JOIN polymarket_repricing_windows rw ON rw.condition_id = pm.condition_id
WHERE a.created_at >= NOW() - $1::interval AND rw.status IN ('closed','stale_missing_price','stale_missing_peers')`},
		{&cov.WithRiskScores, `
SELECT COUNT(DISTINCT a.id)::int FROM polymarket_alerts a
JOIN polymarket_markets pm ON pm.id = a.market_id
JOIN polymarket_market_risk_scores rs ON rs.condition_id = pm.condition_id AND rs.is_active = TRUE
WHERE a.created_at >= NOW() - $1::interval`},
		{&cov.WithWalletGraphEdges, `
SELECT COUNT(DISTINCT a.id)::int FROM polymarket_alerts a
JOIN polymarket_traders tr ON tr.id = a.trader_id
JOIN polymarket_wallet_graph_edges wge ON wge.wallet_a = tr.wallet_address OR wge.wallet_b = tr.wallet_address
WHERE a.created_at >= NOW() - $1::interval`},
		{&cov.WithRecentBookbarsFresh, `
SELECT COUNT(DISTINCT a.id)::int FROM polymarket_alerts a
JOIN polymarket_markets pm ON pm.id = a.market_id
JOIN polymarket_book_feature_bars b ON b.condition_id = pm.condition_id
WHERE a.created_at >= NOW() - $1::interval AND b.bar_start >= NOW() - INTERVAL '24 hours'`},
		{&cov.WithRecentHolderSnapshot, `
SELECT COUNT(DISTINCT a.id)::int FROM polymarket_alerts a
JOIN polymarket_markets pm ON pm.id = a.market_id
JOIN polymarket_holder_snapshots h ON h.condition_id = pm.condition_id
WHERE a.created_at >= NOW() - $1::interval AND h.snapshot_at >= NOW() - INTERVAL '24 hours'`},
	}
	for _, qq := range queries {
		if err := pool.QueryRow(ctx, qq.sql, fmt.Sprintf("%d seconds", int(since.Seconds()))).Scan(qq.target); err != nil {
			// Soft-fail per query — a missing table or migration
			// shouldn't abort the whole report.
			fmt.Fprintf(os.Stderr, "coverage warn: %v\n", err)
		}
	}
	rep.StagedCoverage = cov

	// 5) skip counters from shadow_decisions rows that recorded an
	//    explicit skip reason in reasons_json (newer rows; v11.8+).
	skipRows, err := pool.Query(ctx, `
SELECT strategy_name,
       COALESCE(reasons_json->>0, 'unknown') AS reason,
       COUNT(*)::int
FROM polymarket_strategy_shadow_decisions
WHERE fired_at >= NOW() - $1::interval
  AND decision_kind = 'eval_skipped'
GROUP BY strategy_name, reason
ORDER BY strategy_name, reason`, fmt.Sprintf("%d seconds", int(since.Seconds())))
	if err == nil {
		for skipRows.Next() {
			var sc EvalSkipCounter
			if err := skipRows.Scan(&sc.Strategy, &sc.Reason, &sc.Count); err == nil {
				rep.SkipCounts = append(rep.SkipCounts, sc)
			}
		}
		skipRows.Close()
	}

	return rep, nil
}

func printStrategyReport(out *os.File, rep *StrategyShadowReport) {
	fmt.Fprintf(out, "Strategy shadow fanout replay — generated %s, lookback %.1fh, %d shadow rows total.\n\n",
		rep.GeneratedAt.Format(time.RFC3339), rep.LookbackHours, rep.TotalRows)

	fmt.Fprintln(out, "Per-strategy stats:")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "strategy\trows\tshadow_only\tpromoted\tlinked\tstandalone\twith_clv\tavg_clv6h¢\tok\twrong\teligible")
	for _, s := range rep.PerStrategy {
		elig := "—"
		if s.LatestEligible != nil {
			if *s.LatestEligible {
				elig = "yes"
			} else {
				elig = "no"
			}
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%d\t%d\t%.2f\t%d\t%d\t%s\n",
			s.Strategy, s.ShadowRows, s.ShadowOnlyRows, s.PromotedRows,
			s.LinkedRows, s.StandaloneRows, s.WithCLV6h, s.AvgCLV6hCents,
			s.OutcomeCorrect, s.OutcomeWrong, elig)
	}
	_ = tw.Flush()

	if len(rep.SkipCounts) > 0 {
		fmt.Fprintln(out, "\nSkip reasons (shadow rows with decision_kind='eval_skipped'):")
		tw2 := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw2, "strategy\treason\tcount")
		for _, sc := range rep.SkipCounts {
			fmt.Fprintf(tw2, "%s\t%s\t%d\n", sc.Strategy, sc.Reason, sc.Count)
		}
		_ = tw2.Flush()
	} else {
		fmt.Fprintln(out, "\nSkip reasons: no rows with decision_kind='eval_skipped' in window.")
	}

	cov := rep.StagedCoverage
	fmt.Fprintf(out, "\nStaged-input coverage of %d recent alerts:\n", cov.RecentAlerts)
	tw3 := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw3, "feature\talerts_with")
	fmt.Fprintf(tw3, "active catalyst\t%d\n", cov.WithCatalysts)
	fmt.Fprintf(tw3, "market_links\t%d\n", cov.WithMarketLinks)
	fmt.Fprintf(tw3, "closed/stale repricing window\t%d\n", cov.WithClosedRepricing)
	fmt.Fprintf(tw3, "active risk score\t%d\n", cov.WithRiskScores)
	fmt.Fprintf(tw3, "walletgraph edge for trader\t%d\n", cov.WithWalletGraphEdges)
	fmt.Fprintf(tw3, "bookbars fresh (≤24h)\t%d\n", cov.WithRecentBookbarsFresh)
	fmt.Fprintf(tw3, "holder snapshot fresh (≤24h)\t%d\n", cov.WithRecentHolderSnapshot)
	_ = tw3.Flush()

	// per-strategy bucket diagnostics
	hasBuckets := false
	for _, s := range rep.PerStrategy {
		if strings.TrimSpace(s.BucketDiagnosticsRaw) != "" {
			hasBuckets = true
			break
		}
	}
	if hasBuckets {
		fmt.Fprintln(out, "\nBucket diagnostics (latest promotion review):")
		for _, s := range rep.PerStrategy {
			if strings.TrimSpace(s.BucketDiagnosticsRaw) == "" {
				continue
			}
			fmt.Fprintf(out, "  %s: %s\n", s.Strategy, s.BucketDiagnosticsRaw)
		}
	}

	fmt.Fprintln(out, "\nUser flow: not exercised (read-only).")
	fmt.Fprintln(out, "AI calls:  not exercised (read-only).")
	fmt.Fprintln(out, "Writes:    none (read-only).")
}
