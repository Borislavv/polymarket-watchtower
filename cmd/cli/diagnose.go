package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// runDiagnoseAlerts produces a gate-by-gate breakdown explaining, with
// real counts from the supplied Postgres, why the current config either
// produces or suppresses alerts. Read-only.
//
// The configuration knobs (lifecycle pct, baseline gates, ladders) are
// read from environment variables in the same way the watchtower app
// reads them — so running `set -a && source .env && cli diagnose-alerts`
// gives a faithful projection of what would fire under the active
// config without actually starting the detector.
//
// Every gate also accepts a CLI flag override, so two configs can be
// compared against the same DB snapshot without editing `.env`:
//
//   cli diagnose-alerts \
//     --baseline-min-trades 20 --baseline-min-notional 2000 --baseline-min-span 3h \
//     --lifecycle-from 60 --market-min-age 6h \
//     --info-min-notional 2500 --info-min-odds 2 --info-min-multiplier 20 \
//     --warning-min-notional 10000 --warning-min-multiplier 50 \
//     --critical-min-notional 25000 --critical-min-multiplier 100
func runDiagnoseAlerts(args []string) {
	fs := flag.NewFlagSet("diagnose-alerts", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("POSTGRES_DSN"), "Postgres DSN (defaults to $POSTGRES_DSN)")
	lookbackFlag := fs.Duration("lookback", 24*time.Hour, "trade-lookback window used when projecting candidate volume")

	// Gate-override flags. Each defaults to NaN/-1/empty so we can tell
	// "operator did not set" apart from "operator explicitly set 0".
	bnTrades := fs.Int("baseline-min-trades", -1, "override SINGLE_MIN_BASELINE_TRADES")
	bnNotional := fs.Float64("baseline-min-notional", -1, "override SINGLE_MIN_BASELINE_NOTIONAL_USD")
	bnSpan := fs.Duration("baseline-min-span", 0, "override BASELINE_MIN_READY_WINDOW")
	lcFrom := fs.Float64("lifecycle-from", -1, "override LIFECYCLE_ALERT_FROM_PCT (percent, e.g. 65)")
	mkAge := fs.Duration("market-min-age", 0, "override MARKET_MIN_AGE")
	infoNotional := fs.Float64("info-min-notional", -1, "override ALERT_INFO_MIN_NOTIONAL_USD")
	infoOdds := fs.Float64("info-min-odds", -1, "override ALERT_INFO_MIN_ODDS")
	infoMult := fs.Float64("info-min-multiplier", -1, "override ALERT_INFO_MIN_MULTIPLIER")
	warnNotional := fs.Float64("warning-min-notional", -1, "override ALERT_WARNING_MIN_NOTIONAL_USD")
	warnOdds := fs.Float64("warning-min-odds", -1, "override ALERT_WARNING_MIN_ODDS")
	warnMult := fs.Float64("warning-min-multiplier", -1, "override ALERT_WARNING_MIN_MULTIPLIER")
	critNotional := fs.Float64("critical-min-notional", -1, "override ALERT_CRITICAL_MIN_NOTIONAL_USD")
	critOdds := fs.Float64("critical-min-odds", -1, "override ALERT_CRITICAL_MIN_ODDS")
	critMult := fs.Float64("critical-min-multiplier", -1, "override ALERT_CRITICAL_MIN_MULTIPLIER")
	showCandidates := fs.Int("show-candidates", 0, "if >0, print the top-N firing candidate trades over the lookback")

	_ = fs.Parse(args)
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "diagnose-alerts: -dsn or $POSTGRES_DSN required")
		os.Exit(2)
	}

	gates := readGatesFromEnv()
	if *bnTrades >= 0 {
		gates.minBaselineTrades = *bnTrades
	}
	if *bnNotional >= 0 {
		gates.minBaselineNotionalUSD = *bnNotional
	}
	if *bnSpan > 0 {
		gates.minReadyWindow = *bnSpan
	}
	if *lcFrom >= 0 {
		gates.lifecycleFromPct = *lcFrom
	}
	if *mkAge > 0 {
		gates.marketMinAge = *mkAge
	}
	if *infoNotional >= 0 {
		gates.infoMinNotionalUSD = *infoNotional
	}
	if *infoOdds >= 0 {
		gates.infoMinOdds = *infoOdds
	}
	if *infoMult >= 0 {
		gates.infoMinMultiplier = *infoMult
	}
	if *warnNotional >= 0 {
		gates.warnMinNotionalUSD = *warnNotional
	}
	if *warnOdds >= 0 {
		gates.warnMinOdds = *warnOdds
	}
	if *warnMult >= 0 {
		gates.warnMinMultiplier = *warnMult
	}
	if *critNotional >= 0 {
		gates.critMinNotionalUSD = *critNotional
	}
	if *critOdds >= 0 {
		gates.critMinOdds = *critOdds
	}
	if *critMult >= 0 {
		gates.critMinMultiplier = *critMult
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "diagnose-alerts: pool: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	fmt.Println("== watchtower diagnose-alerts ==")
	fmt.Printf("lookback: %s\n", *lookbackFlag)
	fmt.Printf("gates (env+flags):\n%s\n", gates.summary())

	// 1) Universe — markets / categories
	var totalCats, enabledCats int64
	_ = pool.QueryRow(ctx, `SELECT COUNT(*), SUM(CASE WHEN enabled THEN 1 ELSE 0 END) FROM polymarket_categories`).Scan(&totalCats, &enabledCats)
	fmt.Printf("\n[1] categories: total=%d enabled=%d\n", totalCats, enabledCats)

	var totalMkt, activeMkt, lifecycleUnknown, lifecycleKnown int64
	_ = pool.QueryRow(ctx, `
		SELECT
		  COUNT(*),
		  SUM(CASE WHEN active THEN 1 ELSE 0 END),
		  SUM(CASE WHEN active AND (start_date IS NULL OR end_date IS NULL) THEN 1 ELSE 0 END),
		  SUM(CASE WHEN active AND start_date IS NOT NULL AND end_date IS NOT NULL AND end_date > start_date THEN 1 ELSE 0 END)
		FROM polymarket_markets`).Scan(&totalMkt, &activeMkt, &lifecycleUnknown, &lifecycleKnown)
	fmt.Printf("[2] markets: total=%d active=%d lifecycle_unknown=%d lifecycle_known=%d\n",
		totalMkt, activeMkt, lifecycleUnknown, lifecycleKnown)

	// Active markets in enabled categories that pass age + lifecycle gates.
	var activeWL, ageOK, pctOK int64
	q := `
		WITH ap AS (
		  SELECT DISTINCT m.id, m.start_date, m.end_date
		  FROM polymarket_markets m
		  JOIN polymarket_market_categories mc ON mc.market_id = m.id
		  JOIN polymarket_categories c ON c.id = mc.category_id
		  WHERE m.active = TRUE AND c.enabled = TRUE
		)
		SELECT
		  COUNT(*),
		  SUM(CASE WHEN start_date IS NOT NULL
		            AND (NOW() - start_date) >= make_interval(secs => $1) THEN 1 ELSE 0 END),
		  SUM(CASE WHEN start_date IS NOT NULL AND end_date IS NOT NULL AND end_date > start_date
		            AND EXTRACT(EPOCH FROM (NOW() - start_date))
		                  / NULLIF(EXTRACT(EPOCH FROM (end_date - start_date)), 0) >= $2 THEN 1 ELSE 0 END)
		FROM ap`
	_ = pool.QueryRow(ctx, q, gates.marketMinAge.Seconds(), gates.lifecycleFromPct/100.0).Scan(&activeWL, &ageOK, &pctOK)
	fmt.Printf("[3] active whitelisted markets: %d (age≥%s: %d, lifecycle≥%.0f%%: %d)\n",
		activeWL, gates.marketMinAge, ageOK, gates.lifecycleFromPct, pctOK)

	// 4) Baseline readiness per (market, outcome)
	var totalPairs, readyPairs, readyLifecycled int64
	q = `
		WITH bo AS (
		  SELECT t.market_id, t.outcome_token,
		         COUNT(*) AS n, SUM(t.notional_usd) AS total_usd,
		         PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY t.notional_usd) AS median_usd,
		         (MAX(t.traded_at) - MIN(t.traded_at)) AS span
		  FROM polymarket_trades t
		  JOIN polymarket_markets m ON m.id = t.market_id AND m.active = TRUE
		  JOIN polymarket_market_categories mc ON mc.market_id = m.id
		  JOIN polymarket_categories c ON c.id = mc.category_id AND c.enabled = TRUE
		  GROUP BY t.market_id, t.outcome_token
		)
		SELECT
		  COUNT(*),
		  SUM(CASE WHEN n >= $1 AND total_usd >= $2
		            AND span >= make_interval(secs => $3)
		            AND median_usd > 0 THEN 1 ELSE 0 END),
		  SUM(CASE WHEN n >= $1 AND total_usd >= $2
		            AND span >= make_interval(secs => $3)
		            AND median_usd > 0
		            AND EXISTS (
		              SELECT 1 FROM polymarket_markets m
		              WHERE m.id = bo.market_id
		                AND m.start_date IS NOT NULL AND m.end_date IS NOT NULL
		                AND m.end_date > m.start_date
		                AND (NOW() - m.start_date) >= make_interval(secs => $4)
		                AND EXTRACT(EPOCH FROM (NOW() - m.start_date))
		                      / NULLIF(EXTRACT(EPOCH FROM (m.end_date - m.start_date)), 0) >= $5
		            ) THEN 1 ELSE 0 END)
		FROM bo`
	if err := pool.QueryRow(ctx, q,
		gates.minBaselineTrades,
		gates.minBaselineNotionalUSD,
		gates.minReadyWindow.Seconds(),
		gates.marketMinAge.Seconds(),
		gates.lifecycleFromPct/100.0,
	).Scan(&totalPairs, &readyPairs, &readyLifecycled); err != nil {
		fmt.Fprintf(os.Stderr, "readiness query: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("[4] (market, outcome) pairs with any trades: %d\n", totalPairs)
	fmt.Printf("    pairs passing all baseline readiness gates: %d\n", readyPairs)
	fmt.Printf("    pairs passing readiness AND lifecycle/age: %d  ← eligible universe\n", readyLifecycled)

	// 5) Trades in the lookback window — gate breakdown.
	// We also project Warning and Critical fire counts so the operator
	// sees the severity distribution under the active ladders.
	q = `
		WITH bo AS (
		  SELECT t.market_id, t.outcome_token,
		         COUNT(*) AS n, SUM(t.notional_usd) AS total_usd,
		         PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY t.notional_usd) AS median_usd,
		         (MAX(t.traded_at) - MIN(t.traded_at)) AS span
		  FROM polymarket_trades t
		  JOIN polymarket_markets m ON m.id = t.market_id AND m.active = TRUE
		  JOIN polymarket_market_categories mc ON mc.market_id = m.id
		  JOIN polymarket_categories c ON c.id = mc.category_id AND c.enabled = TRUE
		  GROUP BY t.market_id, t.outcome_token
		), eligible AS (
		  SELECT bo.market_id, bo.outcome_token, bo.median_usd
		  FROM bo
		  JOIN polymarket_markets m ON m.id = bo.market_id
		  WHERE bo.n >= $1 AND bo.total_usd >= $2
		    AND bo.span >= make_interval(secs => $3)
		    AND bo.median_usd > 0
		    AND m.start_date IS NOT NULL AND m.end_date IS NOT NULL AND m.end_date > m.start_date
		    AND (NOW() - m.start_date) >= make_interval(secs => $4)
		    AND EXTRACT(EPOCH FROM (NOW() - m.start_date))
		          / NULLIF(EXTRACT(EPOCH FROM (m.end_date - m.start_date)), 0) >= $5
		)
		SELECT
		  COUNT(*)                                                                                 AS total,
		  SUM(CASE WHEN e.market_id IS NULL THEN 1 ELSE 0 END)                                     AS not_eligible,
		  SUM(CASE WHEN e.market_id IS NOT NULL AND t.notional_usd < $6 THEN 1 ELSE 0 END)         AS low_n_info,
		  SUM(CASE WHEN e.market_id IS NOT NULL AND (1.0/NULLIF(t.price,0)) < $7 THEN 1 ELSE 0 END) AS low_o_info,
		  SUM(CASE WHEN e.market_id IS NOT NULL AND t.notional_usd / NULLIF(e.median_usd,0) < $8 THEN 1 ELSE 0 END) AS low_m_info,
		  SUM(CASE WHEN e.market_id IS NOT NULL AND t.notional_usd >= $6
		             AND (1.0/NULLIF(t.price,0)) >= $7
		             AND t.notional_usd / NULLIF(e.median_usd,0) >= $8 THEN 1 ELSE 0 END)          AS info_fires,
		  SUM(CASE WHEN e.market_id IS NOT NULL AND t.notional_usd >= $9
		             AND (1.0/NULLIF(t.price,0)) >= $10
		             AND t.notional_usd / NULLIF(e.median_usd,0) >= $11 THEN 1 ELSE 0 END)         AS warn_fires,
		  SUM(CASE WHEN e.market_id IS NOT NULL AND t.notional_usd >= $12
		             AND (1.0/NULLIF(t.price,0)) >= $13
		             AND t.notional_usd / NULLIF(e.median_usd,0) >= $14 THEN 1 ELSE 0 END)         AS crit_fires
		FROM polymarket_trades t
		LEFT JOIN eligible e ON e.market_id = t.market_id AND e.outcome_token = t.outcome_token
		WHERE t.traded_at >= NOW() - make_interval(secs => $15)`
	var totalT, notElig, lowN, lowO, lowM, infoFires, warnFires, critFires int64
	if err := pool.QueryRow(ctx, q,
		gates.minBaselineTrades, gates.minBaselineNotionalUSD, gates.minReadyWindow.Seconds(),
		gates.marketMinAge.Seconds(), gates.lifecycleFromPct/100.0,
		gates.infoMinNotionalUSD, gates.infoMinOdds, gates.infoMinMultiplier,
		gates.warnMinNotionalUSD, gates.warnMinOdds, gates.warnMinMultiplier,
		gates.critMinNotionalUSD, gates.critMinOdds, gates.critMinMultiplier,
		lookbackFlag.Seconds(),
	).Scan(&totalT, &notElig, &lowN, &lowO, &lowM, &infoFires, &warnFires, &critFires); err != nil {
		fmt.Fprintf(os.Stderr, "gate breakdown query: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("\n[5] gate breakdown over the last %s (single-trade Info ladder):\n", *lookbackFlag)
	fmt.Printf("    total trades observed             : %d\n", totalT)
	fmt.Printf("    skipped: market not eligible      : %d\n", notElig)
	fmt.Printf("    skipped: notional < $%.0f         : %d\n", gates.infoMinNotionalUSD, lowN)
	fmt.Printf("    skipped: odds < %.2f               : %d\n", gates.infoMinOdds, lowO)
	fmt.Printf("    skipped: multiplier < %.0f×        : %d\n", gates.infoMinMultiplier, lowM)
	fmt.Printf("    -- would fire Info     (pre-MM, pre-dedup) : %d\n", infoFires)
	fmt.Printf("    -- would fire Warning  (pre-MM, pre-dedup) : %d\n", warnFires)
	fmt.Printf("    -- would fire Critical (pre-MM, pre-dedup) : %d\n", critFires)

	// 6) Alerts table snapshot
	var pending, failed, sent int64
	_ = pool.QueryRow(ctx, `
		SELECT
		  SUM(CASE WHEN status='pending' THEN 1 ELSE 0 END),
		  SUM(CASE WHEN status='failed'  THEN 1 ELSE 0 END),
		  SUM(CASE WHEN status='sent'    THEN 1 ELSE 0 END)
		FROM polymarket_alerts`).Scan(&pending, &failed, &sent)
	fmt.Printf("\n[6] alerts table: pending=%d failed=%d sent=%d\n", pending, failed, sent)

	// 7) Optional: top firing-candidate trades.
	if *showCandidates > 0 {
		printCandidates(ctx, pool, gates, *lookbackFlag, *showCandidates)
	}
}

// printCandidates emits up to `limit` recent trades that would fire under
// the current gates, newest first. Best-effort — read errors are logged
// but do not fail the command.
func printCandidates(ctx context.Context, pool *pgxpool.Pool, g gateConfig, lookback time.Duration, limit int) {
	q := `
		WITH bo AS (
		  SELECT t.market_id, t.outcome_token,
		         COUNT(*) AS n, SUM(t.notional_usd) AS total_usd,
		         PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY t.notional_usd) AS median_usd,
		         (MAX(t.traded_at) - MIN(t.traded_at)) AS span
		  FROM polymarket_trades t
		  JOIN polymarket_markets m ON m.id = t.market_id AND m.active = TRUE
		  JOIN polymarket_market_categories mc ON mc.market_id = m.id
		  JOIN polymarket_categories c ON c.id = mc.category_id AND c.enabled = TRUE
		  GROUP BY t.market_id, t.outcome_token
		), eligible AS (
		  SELECT bo.market_id, bo.outcome_token, bo.median_usd
		  FROM bo JOIN polymarket_markets m ON m.id = bo.market_id
		  WHERE bo.n >= $1 AND bo.total_usd >= $2
		    AND bo.span >= make_interval(secs => $3)
		    AND bo.median_usd > 0
		    AND m.start_date IS NOT NULL AND m.end_date IS NOT NULL AND m.end_date > m.start_date
		    AND (NOW() - m.start_date) >= make_interval(secs => $4)
		    AND EXTRACT(EPOCH FROM (NOW() - m.start_date))
		          / NULLIF(EXTRACT(EPOCH FROM (m.end_date - m.start_date)), 0) >= $5
		)
		SELECT t.id, t.traded_at, m.question,
		       ROUND(t.notional_usd::numeric, 0) AS notional,
		       ROUND((1.0 / NULLIF(t.price,0))::numeric, 2) AS odds,
		       ROUND((t.notional_usd / NULLIF(e.median_usd,0))::numeric, 1) AS mult,
		       t.side
		FROM polymarket_trades t
		JOIN eligible e ON e.market_id = t.market_id AND e.outcome_token = t.outcome_token
		JOIN polymarket_markets m ON m.id = t.market_id
		WHERE t.traded_at >= NOW() - make_interval(secs => $9)
		  AND t.notional_usd >= $6
		  AND (1.0/NULLIF(t.price,0)) >= $7
		  AND t.notional_usd / NULLIF(e.median_usd,0) >= $8
		ORDER BY t.traded_at DESC
		LIMIT $10`
	rows, err := pool.Query(ctx, q,
		g.minBaselineTrades, g.minBaselineNotionalUSD, g.minReadyWindow.Seconds(),
		g.marketMinAge.Seconds(), g.lifecycleFromPct/100.0,
		g.infoMinNotionalUSD, g.infoMinOdds, g.infoMinMultiplier,
		lookback.Seconds(), int32(limit),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "candidate query: %v\n", err)
		return
	}
	defer rows.Close()
	fmt.Printf("\n[7] top %d firing candidates (Info ladder, newest first):\n", limit)
	fmt.Printf("    %-6s  %-19s  %-10s  %-6s  %-9s  %-4s  %s\n", "id", "traded_at", "notional", "odds", "mult", "side", "market")
	for rows.Next() {
		var id int64
		var ts time.Time
		var question, side string
		var notional, odds, mult float64
		if err := rows.Scan(&id, &ts, &question, &notional, &odds, &mult, &side); err != nil {
			fmt.Fprintf(os.Stderr, "scan: %v\n", err)
			return
		}
		if len(question) > 60 {
			question = question[:57] + "..."
		}
		fmt.Printf("    %-6d  %-19s  $%-9.0f  %-6.2f  %-9.1f  %-4s  %s\n",
			id, ts.UTC().Format("2006-01-02 15:04:05"), notional, odds, mult, side, question)
	}
}

// gateConfig captures the env-derived runtime gates the diagnoser
// projects against the DB. Keep in sync with internal/app/config.go.
type gateConfig struct {
	lifecycleFromPct       float64
	marketMinAge           time.Duration
	minBaselineTrades      int
	minBaselineNotionalUSD float64
	minReadyWindow         time.Duration
	infoMinNotionalUSD     float64
	infoMinOdds            float64
	infoMinMultiplier      float64
	warnMinNotionalUSD     float64
	warnMinOdds            float64
	warnMinMultiplier      float64
	critMinNotionalUSD     float64
	critMinOdds            float64
	critMinMultiplier      float64
}

func (g gateConfig) summary() string {
	return fmt.Sprintf(
		"  lifecycle_from_pct=%.0f%%  market_min_age=%s\n"+
			"  baseline: n≥%d, total≥$%.0f, span≥%s\n"+
			"  Info     ladder: notional≥$%.0f, odds≥%.2f, multiplier≥%.0fx\n"+
			"  Warning  ladder: notional≥$%.0f, odds≥%.2f, multiplier≥%.0fx\n"+
			"  Critical ladder: notional≥$%.0f, odds≥%.2f, multiplier≥%.0fx",
		g.lifecycleFromPct, g.marketMinAge,
		g.minBaselineTrades, g.minBaselineNotionalUSD, g.minReadyWindow,
		g.infoMinNotionalUSD, g.infoMinOdds, g.infoMinMultiplier,
		g.warnMinNotionalUSD, g.warnMinOdds, g.warnMinMultiplier,
		g.critMinNotionalUSD, g.critMinOdds, g.critMinMultiplier,
	)
}

func readGatesFromEnv() gateConfig {
	return gateConfig{
		lifecycleFromPct:       envFloat("LIFECYCLE_ALERT_FROM_PCT", 75),
		marketMinAge:           envDuration("MARKET_MIN_AGE", 24*time.Hour),
		minBaselineTrades:      int(envFloat("SINGLE_MIN_BASELINE_TRADES", 100)),
		minBaselineNotionalUSD: envFloat("SINGLE_MIN_BASELINE_NOTIONAL_USD", 10000),
		minReadyWindow:         envDuration("BASELINE_MIN_READY_WINDOW", 24*time.Hour),
		infoMinNotionalUSD:     envFloat("ALERT_INFO_MIN_NOTIONAL_USD", 10000),
		infoMinOdds:            envFloat("ALERT_INFO_MIN_ODDS", 3),
		infoMinMultiplier:      envFloat("ALERT_INFO_MIN_MULTIPLIER", 100),
		warnMinNotionalUSD:     envFloat("ALERT_WARNING_MIN_NOTIONAL_USD", 25000),
		warnMinOdds:            envFloat("ALERT_WARNING_MIN_ODDS", 5),
		warnMinMultiplier:      envFloat("ALERT_WARNING_MIN_MULTIPLIER", 1000),
		critMinNotionalUSD:     envFloat("ALERT_CRITICAL_MIN_NOTIONAL_USD", 100000),
		critMinOdds:            envFloat("ALERT_CRITICAL_MIN_ODDS", 8),
		critMinMultiplier:      envFloat("ALERT_CRITICAL_MIN_MULTIPLIER", 10000),
	}
}

func envFloat(k string, dflt float64) float64 {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return dflt
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return dflt
	}
	return f
}

func envDuration(k string, dflt time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return dflt
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return dflt
	}
	return d
}
