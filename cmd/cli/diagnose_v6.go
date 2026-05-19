// Package main — v6 diagnose-alerts. Uses the production scorer
// (analytics/score.Score) so the projected fire counts match exactly
// what the watchtower binary would emit under the same config.
//
// Compared to the v4 diagnose-alerts.go (kept for archaeology), this
// command:
//
//   - loads the full app.Config from env (same code path the binary uses)
//   - queries baselines per (market, outcome) via the production
//     dbbaseline / traderbaseline providers
//   - feeds every candidate trade through score.Score with the loaded
//     Thresholds, exactly like detect.Loop does
//   - aggregates by (severity OR suppression reason) so an operator can
//     see what the binary would do
//
// Out of scope (deliberately) for this iteration: accumulation projection
// and ownership-fusion projection. Both require multi-row aggregates per
// (wallet, market) and the same MM-suppression read-path the binary
// uses; that doubles the diagnose surface for limited operator gain
// today. They are flagged in the output as "[unsupported in v6 diagnose]".
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/app"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/baseline"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/score"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
)

// v6Row is one trade row hydrated with baseline + scoring outcome.
type v6Row struct {
	tradeID    int64
	tradedAt   time.Time
	notional   float64
	odds       float64
	question   string
	categoryOK bool
	lifecycle  float64
	marketAge  time.Duration

	mktReady bool
	trdReady bool

	result score.Result
}

func runDiagnoseAlertsV6(args []string) {
	fs := flag.NewFlagSet("diagnose-alerts", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("POSTGRES_DSN"), "Postgres DSN (defaults to $POSTGRES_DSN)")
	lookback := fs.Duration("lookback", 24*time.Hour, "trade-lookback window")
	showCandidates := fs.Int("show-candidates", 0, "if >0, print the top-N firing candidate trades")
	_ = fs.Parse(args)

	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "diagnose-alerts: --dsn or POSTGRES_DSN required")
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cfg, err := app.LoadConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config load:", err)
		os.Exit(1)
	}

	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgx pool:", err)
		os.Exit(1)
	}
	defer pool.Close()

	thresholds := thresholdsFromConfig(cfg)
	whitelistLower := lowerSlice(cfg.CategoryFilter.Whitelist)

	rows, err := pullV6CandidateTrades(ctx, pool, *lookback, whitelistLower,
		cfg.Anomaly.MarketMinAge, cfg.Anomaly.LifecycleAlertFromPct)
	if err != nil {
		fmt.Fprintln(os.Stderr, "candidate query:", err)
		os.Exit(1)
	}

	scored := make([]v6Row, 0, len(rows))
	for _, r := range rows {
		mkt, trd, mktReady, trdReady := loadBaselinesForRow(ctx, pool, r, cfg, thresholds)
		r.mktReady = mktReady
		r.trdReady = trdReady
		r.result = score.Score(r.notional, 1.0/r.odds, mkt, trd, thresholds)
		scored = append(scored, r)
	}

	printV6Report(scored, *lookback, *showCandidates, cfg)
}

// thresholdsFromConfig duplicates the wiring in internal/app/app.go so
// the projection sees the exact same numbers production does. Kept
// inline rather than exported because diagnose is the only caller.
func thresholdsFromConfig(cfg *app.Config) anomaly.Thresholds {
	a := cfg.Anomaly
	return anomaly.Thresholds{
		Info: anomaly.Tier{
			MinNotionalUSD: a.InfoMinNotionalUSD, MinOdds: a.InfoMinOdds,
			MinProfitUSD:      a.InfoMinProfitUSD,
			MinMarketP95Ratio: a.InfoMinMarketP95Ratio, MinMarketP99Ratio: a.InfoMinMarketP99Ratio,
			MinTraderP95Ratio: a.InfoMinTraderP95Ratio, MinTraderP99Ratio: a.InfoMinTraderP99Ratio,
			MinMultiplier: a.InfoMinMultiplier,
		},
		Warning: anomaly.Tier{
			MinNotionalUSD: a.WarningMinNotionalUSD, MinOdds: a.WarningMinOdds,
			MinProfitUSD:      a.WarningMinProfitUSD,
			MinMarketP95Ratio: a.WarningMinMarketP95Ratio, MinMarketP99Ratio: a.WarningMinMarketP99Ratio,
			MinTraderP95Ratio: a.WarningMinTraderP95Ratio, MinTraderP99Ratio: a.WarningMinTraderP99Ratio,
			MinMultiplier: a.WarningMinMultiplier,
		},
		Critical: anomaly.Tier{
			MinNotionalUSD: a.CriticalMinNotionalUSD, MinOdds: a.CriticalMinOdds,
			MinProfitUSD:      a.CriticalMinProfitUSD,
			MinMarketP95Ratio: a.CriticalMinMarketP95Ratio, MinMarketP99Ratio: a.CriticalMinMarketP99Ratio,
			MinTraderP95Ratio: a.CriticalMinTraderP95Ratio, MinTraderP99Ratio: a.CriticalMinTraderP99Ratio,
			MinMultiplier: a.CriticalMinMultiplier,
		},
		MinBaselineTrades:                a.SingleMinBaselineTrades,
		MinBaselineNotionalUSD:           a.SingleMinBaselineNotionalUSD,
		LowBaselineCapEnabled:            a.LowBaselineCapEnabled,
		LowBaselineSingleMaxSeverity:     anomaly.Severity(a.LowBaselineSingleMaxSeverity),
		LowBaselineAllowCriticalAbsolute: a.LowBaselineAllowCriticalAbsolute,
	}
}

// pullV6CandidateTrades selects every trade in the lookback whose
// market clears (a) the category whitelist, (b) MarketMinAge, (c)
// lifecycle ≥ LifecycleAlertFromPct. The price > 0 && < 1 filter is
// the same one score.Score enforces internally; we apply it upstream
// to keep the count exact.
func pullV6CandidateTrades(ctx context.Context, pool *pgxpool.Pool, lookback time.Duration, whitelist []string, minAge time.Duration, lifecycleFrom float64) ([]v6Row, error) {
	q := `
SELECT DISTINCT ON (t.id)
    t.id, t.traded_at, t.notional_usd, t.price, t.market_id, t.trader_id,
    t.outcome_token, m.question,
    100.0 * EXTRACT(EPOCH FROM (NOW() - m.start_date)) / NULLIF(EXTRACT(EPOCH FROM (m.end_date - m.start_date)), 0) AS lifecycle_pct
FROM polymarket_trades t
JOIN polymarket_markets m ON m.id = t.market_id
JOIN polymarket_market_categories mc ON mc.market_id = m.id
JOIN polymarket_categories c ON c.id = mc.category_id
WHERE t.traded_at >= NOW() - $1::interval
  AND t.price > 0 AND t.price < 1
  AND m.start_date IS NOT NULL AND m.end_date IS NOT NULL
  AND m.start_date <= NOW() - $2::interval
  AND ` + categoryFilterSQL(whitelist) + `
ORDER BY t.id, t.traded_at DESC`

	rows, err := pool.Query(ctx, q, lookback, minAge)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	out := make([]v6Row, 0, 4096)
	for rows.Next() {
		var (
			r          v6Row
			marketID   int64
			traderID   *int64
			outcomeTok string
			price      float64
			lifecycle  *float64
		)
		if err := rows.Scan(&r.tradeID, &r.tradedAt, &r.notional, &price, &marketID, &traderID, &outcomeTok, &r.question, &lifecycle); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if price <= 0 || price >= 1 {
			continue
		}
		r.odds = 1.0 / price
		if lifecycle != nil {
			r.lifecycle = *lifecycle
		}
		if r.lifecycle < lifecycleFrom {
			continue
		}
		// Stash the foreign keys for the per-row baseline pull (handled
		// in loadBaselinesForRow).
		r.categoryOK = true
		r = withRefs(r, marketID, outcomeTok, traderID)
		out = append(out, r)
	}
	return out, rows.Err()
}

// rowRefs carries the row's foreign keys + outcome token through to
// the baseline-load step without polluting v6Row's public surface.
type rowRefs struct {
	marketID   int64
	outcomeTok string
	traderID   *int64
}

var v6Refs = map[int64]rowRefs{} // module-scoped, reset per run

func withRefs(r v6Row, mid int64, tok string, tid *int64) v6Row {
	v6Refs[r.tradeID] = rowRefs{marketID: mid, outcomeTok: tok, traderID: tid}
	return r
}

func loadBaselinesForRow(ctx context.Context, pool *pgxpool.Pool, r v6Row, cfg *app.Config, t anomaly.Thresholds) (mkt, trd baseline.Stats, mktReady, trdReady bool) {
	refs, ok := v6Refs[r.tradeID]
	if !ok {
		return baseline.Stats{}, baseline.Stats{}, false, false
	}
	since := time.Time{}
	if cfg.Anomaly.BaselineWindow > 0 {
		since = time.Now().Add(-cfg.Anomaly.BaselineWindow)
	}
	mkt = pullDistribution(ctx, pool, refs.marketID, refs.outcomeTok, since)
	mktReady = mkt.Count >= t.MinBaselineTrades && mkt.TotalUSD >= t.MinBaselineNotionalUSD && mkt.P95USD > 0
	if refs.traderID != nil {
		traderSince := time.Time{}
		if cfg.Anomaly.TraderBaselineWindow > 0 {
			traderSince = time.Now().Add(-cfg.Anomaly.TraderBaselineWindow)
		}
		trd = pullTraderDistribution(ctx, pool, *refs.traderID, traderSince)
		trdReady = trd.Count >= t.MinBaselineTrades && trd.P95USD > 0
	}
	return mkt, trd, mktReady, trdReady
}

func pullDistribution(ctx context.Context, pool *pgxpool.Pool, marketID int64, token string, since time.Time) baseline.Stats {
	const q = `
SELECT COUNT(*),
       COALESCE(SUM(notional_usd), 0),
       COALESCE(AVG(notional_usd), 0),
       COALESCE(PERCENTILE_CONT(0.5)  WITHIN GROUP (ORDER BY notional_usd), 0),
       COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY notional_usd), 0),
       COALESCE(PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY notional_usd), 0),
       MIN(traded_at), MAX(traded_at)
FROM polymarket_trades
WHERE market_id = $1 AND outcome_token = $2
  AND ($3::timestamptz IS NULL OR traded_at >= $3::timestamptz)`
	var (
		s          baseline.Stats
		oldest, nw *time.Time
		sinceArg   any = since
	)
	if since.IsZero() {
		sinceArg = nil
	}
	row := pool.QueryRow(ctx, q, marketID, token, sinceArg)
	if err := row.Scan(&s.Count, &s.TotalUSD, &s.MeanUSD, &s.MedianUSD, &s.P95USD, &s.P99USD, &oldest, &nw); err != nil {
		return baseline.Stats{}
	}
	if oldest != nil && nw != nil && s.Count >= 2 {
		s.SpanActual = nw.Sub(*oldest)
		s.OldestAt = *oldest
	}
	return s
}

func pullTraderDistribution(ctx context.Context, pool *pgxpool.Pool, traderID int64, since time.Time) baseline.Stats {
	const q = `
SELECT COUNT(*),
       COALESCE(SUM(notional_usd), 0),
       COALESCE(AVG(notional_usd), 0),
       COALESCE(PERCENTILE_CONT(0.5)  WITHIN GROUP (ORDER BY notional_usd), 0),
       COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY notional_usd), 0),
       COALESCE(PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY notional_usd), 0),
       MIN(traded_at), MAX(traded_at)
FROM polymarket_trades
WHERE trader_id = $1
  AND ($2::timestamptz IS NULL OR traded_at >= $2::timestamptz)`
	var (
		s          baseline.Stats
		oldest, nw *time.Time
		sinceArg   any = since
	)
	if since.IsZero() {
		sinceArg = nil
	}
	row := pool.QueryRow(ctx, q, traderID, sinceArg)
	if err := row.Scan(&s.Count, &s.TotalUSD, &s.MeanUSD, &s.MedianUSD, &s.P95USD, &s.P99USD, &oldest, &nw); err != nil {
		return baseline.Stats{}
	}
	if oldest != nil && nw != nil && s.Count >= 2 {
		s.SpanActual = nw.Sub(*oldest)
		s.OldestAt = *oldest
	}
	return s
}

func printV6Report(rows []v6Row, lookback time.Duration, topN int, cfg *app.Config) {
	const sep = "─────────────────────────────────────────────────────────────────────"
	days := lookback.Hours() / 24
	if days <= 0 {
		days = 1
	}

	fired := map[anomaly.Severity]int{}
	suppressed := map[string]int{}
	capped := 0
	tooOld := 0

	for _, r := range rows {
		if cfg.Anomaly.LiveAlertMaxLag > 0 && time.Since(r.tradedAt) > cfg.Anomaly.LiveAlertMaxLag {
			tooOld++
		}
		if r.result.Fired {
			fired[r.result.Severity]++
			if r.result.SeverityCapped {
				capped++
			}
			continue
		}
		suppressed[r.result.SuppressedReason]++
	}

	fmt.Println(sep)
	fmt.Println("diagnose-alerts (v6) — production-scorer parity")
	fmt.Printf("lookback: %s   strategy: %s\n", lookback, anomaly.StrategyIdentity)
	fmt.Printf("category whitelist: %v\n", cfg.CategoryFilter.Whitelist)
	fmt.Println(sep)

	fmt.Printf("Candidate trades (post category + lifecycle + market-age gates): %d\n", len(rows))
	fmt.Printf("  too_old_for_live_alert (would not Telegram): %d\n", tooOld)
	fmt.Printf("  severity-capped by low-baseline rule:        %d\n\n", capped)

	fmt.Println("Would-fire by severity:")
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, sev := range []anomaly.Severity{anomaly.SeverityCritical, anomaly.SeverityWarning, anomaly.SeverityInfo} {
		n := fired[sev]
		fmt.Fprintf(tw, "  %s\t%d\t%.2f/day\n", string(sev), n, float64(n)/days)
	}
	tw.Flush()
	fmt.Println()

	fmt.Println("Suppressed by reason:")
	keys := make([]string, 0, len(suppressed))
	for k := range suppressed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	tw = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, k := range keys {
		fmt.Fprintf(tw, "  %s\t%d\n", k, suppressed[k])
	}
	tw.Flush()
	fmt.Println()

	fmt.Println("Unsupported in v6 diagnose:")
	fmt.Println("  accumulation projection      — multi-row, MM-suppression-aware (skipped)")
	fmt.Println("  ownership-fusion projection  — same complexity (skipped)")
	fmt.Println()

	if topN > 0 {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].result.Fired != rows[j].result.Fired {
				return rows[i].result.Fired
			}
			return rows[i].result.ProfitIfWinUSD > rows[j].result.ProfitIfWinUSD
		})
		fmt.Printf("Top %d candidates (by profit-if-win):\n", topN)
		tw = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  severity\tnotional\todds\tprofit\tmkt-p95-ratio\ttrd-p95-ratio\tquestion")
		for i, r := range rows {
			if i >= topN {
				break
			}
			fmt.Fprintf(tw, "  %s\t$%.0f\t%.2f\t$%.0f\t%.2f\t%.2f\t%s\n",
				severityCell(r.result), r.notional, r.odds, r.result.ProfitIfWinUSD,
				r.result.MarketP95Ratio, r.result.TraderP95Ratio,
				truncate(r.question, 60))
		}
		tw.Flush()
	}
	v6Refs = map[int64]rowRefs{} // free memory
}

func severityCell(r score.Result) string {
	if r.Fired {
		return string(r.Severity)
	}
	if r.SuppressedReason != "" {
		return "—"
	}
	return "—"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func lowerSlice(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = toLower(s)
	}
	return out
}

func toLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// categoryFilterSQL builds an inline OR-of-LIKEs against
// (slug || ' ' || name). Empty whitelist → "TRUE" (no filtering).
// Safe: whitelist comes from env, not from user input over a network.
func categoryFilterSQL(whitelist []string) string {
	if len(whitelist) == 0 {
		return "TRUE"
	}
	parts := make([]string, 0, len(whitelist))
	for _, w := range whitelist {
		// Defensive — strip any literal single quotes (whitelist is
		// operator-controlled but better safe than sorry).
		safe := ""
		for _, c := range w {
			if c == '\'' {
				continue
			}
			safe += string(c)
		}
		parts = append(parts, fmt.Sprintf("lower(coalesce(c.slug,'') || ' ' || coalesce(c.name,'')) LIKE '%%%s%%'", safe))
	}
	out := "(" + parts[0]
	for _, p := range parts[1:] {
		out += " OR " + p
	}
	return out + ")"
}
