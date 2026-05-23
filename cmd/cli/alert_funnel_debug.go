// alert_funnel_debug.go — v11.11 funnel diagnosis report.
//
// Answers the operator question "why so few alerts and why always the
// same events?" by walking the funnel stage by stage from a read-only
// Postgres connection. Never writes, never calls Telegram, never
// calls AI.
//
// Stages (top → bottom of the funnel):
//
//	1. active markets / categories
//	2. trades in window per market + per event
//	3. detect.Loop work-queue lag (from Prometheus, optional)
//	4. lifecycle / age gate (markets blocked by LIFECYCLE_ALERT_FROM_PCT
//	   or MARKET_MIN_AGE)
//	5. alerts persisted (per severity, per event_slug, per condition)
//	6. strategy shadow decisions per strategy
//	7. dedup_key cardinality (collisions hint at over-broad keys)
//	8. top input-rich silent markets (high trade volume but zero alerts)
//
// Run:
//   go run ./cmd/cli alert-funnel-debug --dsn=... --since 24h [--json]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func runAlertFunnelDebug(args []string) {
	fs := flag.NewFlagSet("alert-funnel-debug", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("POSTGRES_DSN"), "Postgres DSN (defaults to $POSTGRES_DSN)")
	since := fs.Duration("since", 24*time.Hour, "lookback window")
	jsonOut := fs.Bool("json", false, "emit JSON instead of the operator-facing table")
	topN := fs.Int("top", 20, "rows for top-N tables")
	_ = fs.Parse(args)
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "alert-funnel-debug: --dsn or $POSTGRES_DSN required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "alert-funnel-debug: connect: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	rep, err := gatherFunnelReport(ctx, pool, *since, *topN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "alert-funnel-debug: gather: %v\n", err)
		os.Exit(1)
	}
	if *jsonOut {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
		return
	}
	printFunnelReport(os.Stdout, rep)
}

type FunnelReport struct {
	GeneratedAt    time.Time          `json:"generated_at"`
	LookbackHours  float64            `json:"lookback_hours"`
	MarketUniverse FunnelMarketStage  `json:"market_universe"`
	TradeInput     FunnelTradeStage   `json:"trade_input"`
	Lifecycle      FunnelLifecycle    `json:"lifecycle_buckets"`
	Alerts         FunnelAlertStage   `json:"alerts"`
	Shadow         FunnelShadowStage  `json:"strategy_shadow"`
	Dedup          FunnelDedupStage   `json:"dedup_keys"`
	Silent         []FunnelSilentRow  `json:"top_silent_markets"`
	TopEvents      []FunnelTopEvent   `json:"top_alerted_events"`
	Telegram       FunnelTelegramRoute `json:"telegram_route"`
}

type FunnelMarketStage struct {
	ActiveMarkets int            `json:"active_markets"`
	ActiveEvents  int            `json:"active_events"`
	PerCategory   []CategoryRow  `json:"per_category"`
}
type CategoryRow struct {
	Slug    string `json:"slug"`
	Markets int    `json:"markets"`
	Events  int    `json:"events"`
}
type FunnelTradeStage struct {
	Trades         int `json:"trades"`
	DistinctEvents int `json:"distinct_events"`
	DistinctMarkets int `json:"distinct_markets"`
	DistinctWallets int `json:"distinct_wallets"`
}
type FunnelLifecycle struct {
	Lt50          int `json:"under_50pct"`
	From50To75    int `json:"50_to_75pct"`
	From75To90    int `json:"75_to_90pct_eligible"`
	Ge90          int `json:"ge_90pct_hot"`
	UnknownLifec  int `json:"unknown_lifecycle"`
}
type FunnelAlertStage struct {
	Total              int            `json:"total"`
	DistinctEvents     int            `json:"distinct_events"`
	DistinctMarkets    int            `json:"distinct_markets"`
	BySeverity         map[string]int `json:"by_severity"`
	TopEventShare      float64        `json:"top_event_share_pct"`
	Top3EventShare     float64        `json:"top3_event_share_pct"`
}
type FunnelShadowStage struct {
	Total       int            `json:"total"`
	PerStrategy map[string]int `json:"per_strategy"`
	PerKind     map[string]int `json:"per_kind"`
}
type FunnelDedupStage struct {
	DistinctDedupKeys  int `json:"distinct_dedup_keys"`
	DedupCollisions    int `json:"dedup_collisions"`
}
type FunnelSilentRow struct {
	EventSlug    string `json:"event_slug"`
	ConditionID  string `json:"condition_id"`
	Trades       int    `json:"trades"`
	LifecyclePct *float64 `json:"lifecycle_pct,omitempty"`
}
type FunnelTopEvent struct {
	EventSlug  string `json:"event_slug"`
	Alerts     int    `json:"alerts"`
}
type FunnelTelegramRoute struct {
	Routes map[string]int `json:"routes"`
}

func gatherFunnelReport(ctx context.Context, pool *pgxpool.Pool, since time.Duration, topN int) (*FunnelReport, error) {
	rep := &FunnelReport{
		GeneratedAt:   time.Now().UTC(),
		LookbackHours: since.Hours(),
	}
	sinceArg := fmt.Sprintf("%d seconds", int(since.Seconds()))

	// stage 1 — market universe
	if err := pool.QueryRow(ctx, `
SELECT COUNT(*)::int, COUNT(DISTINCT event_slug)::int
FROM polymarket_markets
WHERE active=true AND deleted_at IS NULL AND purged_at IS NULL`).
		Scan(&rep.MarketUniverse.ActiveMarkets, &rep.MarketUniverse.ActiveEvents); err != nil {
		return nil, err
	}
	catRows, err := pool.Query(ctx, `
SELECT c.slug, COUNT(DISTINCT pm.id)::int, COUNT(DISTINCT pm.event_slug)::int
FROM polymarket_markets pm
JOIN polymarket_market_categories mc ON mc.market_id = pm.id
JOIN polymarket_categories c ON c.id = mc.category_id
WHERE pm.active=true AND pm.deleted_at IS NULL AND pm.purged_at IS NULL
GROUP BY c.slug ORDER BY 2 DESC LIMIT 10`)
	if err == nil {
		for catRows.Next() {
			var r CategoryRow
			if err := catRows.Scan(&r.Slug, &r.Markets, &r.Events); err == nil {
				rep.MarketUniverse.PerCategory = append(rep.MarketUniverse.PerCategory, r)
			}
		}
		catRows.Close()
	}

	// stage 2 — trades in window
	_ = pool.QueryRow(ctx, `
SELECT COUNT(*)::int, COUNT(DISTINCT pm.event_slug)::int,
       COUNT(DISTINCT pm.id)::int, COUNT(DISTINCT t.trader_id)::int
FROM polymarket_trades t
JOIN polymarket_markets pm ON pm.id = t.market_id
WHERE t.traded_at >= NOW() - $1::interval`, sinceArg).Scan(
		&rep.TradeInput.Trades, &rep.TradeInput.DistinctEvents,
		&rep.TradeInput.DistinctMarkets, &rep.TradeInput.DistinctWallets)

	// stage 3 — lifecycle buckets for markets with trades in window
	rows, err := pool.Query(ctx, `
WITH ir AS (
  SELECT pm.id,
    CASE WHEN pm.start_date IS NULL OR pm.end_date IS NULL THEN NULL
         ELSE 100.0 * EXTRACT(EPOCH FROM (NOW() - pm.start_date))
              / NULLIF(EXTRACT(EPOCH FROM (pm.end_date - pm.start_date)),0)
    END AS pct
  FROM polymarket_markets pm
  JOIN polymarket_trades t ON t.market_id = pm.id
  WHERE pm.active=true AND pm.deleted_at IS NULL AND pm.purged_at IS NULL
    AND t.traded_at >= NOW() - $1::interval
  GROUP BY pm.id, pm.start_date, pm.end_date
)
SELECT bucket, COUNT(*)::int FROM (
  SELECT CASE
    WHEN pct IS NULL THEN 'unknown'
    WHEN pct < 50 THEN 'lt50'
    WHEN pct < 75 THEN '50_75'
    WHEN pct < 90 THEN '75_90'
    ELSE 'ge90'
  END AS bucket FROM ir
) s GROUP BY 1`, sinceArg)
	if err == nil {
		for rows.Next() {
			var b string
			var c int
			if err := rows.Scan(&b, &c); err == nil {
				switch b {
				case "unknown":
					rep.Lifecycle.UnknownLifec = c
				case "lt50":
					rep.Lifecycle.Lt50 = c
				case "50_75":
					rep.Lifecycle.From50To75 = c
				case "75_90":
					rep.Lifecycle.From75To90 = c
				case "ge90":
					rep.Lifecycle.Ge90 = c
				}
			}
		}
		rows.Close()
	}

	// stage 4 — alerts
	rep.Alerts.BySeverity = map[string]int{}
	_ = pool.QueryRow(ctx, `
SELECT COUNT(*)::int, COUNT(DISTINCT pm.event_slug)::int, COUNT(DISTINCT pm.id)::int
FROM polymarket_alerts a JOIN polymarket_markets pm ON pm.id = a.market_id
WHERE a.created_at >= NOW() - $1::interval`, sinceArg).Scan(
		&rep.Alerts.Total, &rep.Alerts.DistinctEvents, &rep.Alerts.DistinctMarkets)
	sevRows, err := pool.Query(ctx, `
SELECT severity::text, COUNT(*)::int FROM polymarket_alerts
WHERE created_at >= NOW() - $1::interval GROUP BY 1`, sinceArg)
	if err == nil {
		for sevRows.Next() {
			var sev string
			var n int
			if err := sevRows.Scan(&sev, &n); err == nil {
				rep.Alerts.BySeverity[sev] = n
			}
		}
		sevRows.Close()
	}

	// top events
	topRows, err := pool.Query(ctx, `
SELECT COALESCE(pm.event_slug, '')::text AS event_slug, COUNT(*)::int
FROM polymarket_alerts a JOIN polymarket_markets pm ON pm.id = a.market_id
WHERE a.created_at >= NOW() - $1::interval
GROUP BY pm.event_slug ORDER BY 2 DESC LIMIT $2`, sinceArg, topN)
	if err == nil {
		var topAcc int
		idx := 0
		for topRows.Next() {
			var ev string
			var n int
			if err := topRows.Scan(&ev, &n); err == nil {
				rep.TopEvents = append(rep.TopEvents, FunnelTopEvent{EventSlug: ev, Alerts: n})
				if idx == 0 && rep.Alerts.Total > 0 {
					rep.Alerts.TopEventShare = 100.0 * float64(n) / float64(rep.Alerts.Total)
				}
				if idx < 3 {
					topAcc += n
				}
				idx++
			}
		}
		topRows.Close()
		if rep.Alerts.Total > 0 {
			rep.Alerts.Top3EventShare = 100.0 * float64(topAcc) / float64(rep.Alerts.Total)
		}
	}

	// stage 5 — strategy shadow
	rep.Shadow.PerStrategy = map[string]int{}
	rep.Shadow.PerKind = map[string]int{}
	sRows, err := pool.Query(ctx, `
SELECT strategy_name, COALESCE(decision_kind, '') AS kind, COUNT(*)::int
FROM polymarket_strategy_shadow_decisions
WHERE fired_at >= NOW() - $1::interval
GROUP BY 1, 2`, sinceArg)
	if err == nil {
		for sRows.Next() {
			var name, kind string
			var n int
			if err := sRows.Scan(&name, &kind, &n); err == nil {
				rep.Shadow.PerStrategy[name] += n
				rep.Shadow.PerKind[kind] += n
				rep.Shadow.Total += n
			}
		}
		sRows.Close()
	}

	// stage 6 — dedup keys
	_ = pool.QueryRow(ctx, `
SELECT COUNT(DISTINCT dedup_key)::int FROM polymarket_alerts
WHERE created_at >= NOW() - $1::interval`, sinceArg).Scan(&rep.Dedup.DistinctDedupKeys)
	_ = pool.QueryRow(ctx, `
SELECT COALESCE(SUM(c-1),0)::int FROM (
  SELECT COUNT(*) AS c FROM polymarket_alerts
  WHERE created_at >= NOW() - $1::interval
  GROUP BY dedup_key HAVING COUNT(*)>1
) s`, sinceArg).Scan(&rep.Dedup.DedupCollisions)

	// stage 7 — top input-rich silent markets
	silentRows, err := pool.Query(ctx, `
WITH ir AS (
  SELECT pm.id, pm.event_slug, pm.condition_id,
    COUNT(t.id) AS trades,
    CASE WHEN pm.start_date IS NULL OR pm.end_date IS NULL THEN NULL
         ELSE 100.0 * EXTRACT(EPOCH FROM (NOW() - pm.start_date))
              / NULLIF(EXTRACT(EPOCH FROM (pm.end_date - pm.start_date)),0)
    END AS pct
  FROM polymarket_markets pm
  JOIN polymarket_trades t ON t.market_id = pm.id
  WHERE pm.active=true AND pm.deleted_at IS NULL AND pm.purged_at IS NULL
    AND t.traded_at >= NOW() - $1::interval
  GROUP BY pm.id, pm.event_slug, pm.condition_id, pm.start_date, pm.end_date
  HAVING COUNT(t.id) >= 50
),
alerted AS (
  SELECT DISTINCT a.market_id FROM polymarket_alerts a
  WHERE a.created_at >= NOW() - $1::interval
)
SELECT ir.event_slug, ir.condition_id, ir.trades::int, ir.pct
FROM ir WHERE ir.id NOT IN (SELECT market_id FROM alerted)
ORDER BY ir.trades DESC LIMIT $2`, sinceArg, topN)
	if err == nil {
		for silentRows.Next() {
			var row FunnelSilentRow
			var pct *float64
			if err := silentRows.Scan(&row.EventSlug, &row.ConditionID, &row.Trades, &pct); err == nil {
				row.LifecyclePct = pct
				rep.Silent = append(rep.Silent, row)
			}
		}
		silentRows.Close()
	}

	// stage 8 — telegram routes from metric counters in the DB (if logged).
	// We can read recent route counters via the alertsender's metric table
	// only when one exists; absent that, this stays empty and operator
	// reads Prometheus /metrics directly.
	rep.Telegram.Routes = map[string]int{}

	return rep, nil
}

func printFunnelReport(out *os.File, r *FunnelReport) {
	fmt.Fprintf(out, "Alert funnel — generated %s, lookback %.1fh\n\n",
		r.GeneratedAt.Format(time.RFC3339), r.LookbackHours)

	fmt.Fprintf(out, "1) Market universe: %d active markets across %d events.\n",
		r.MarketUniverse.ActiveMarkets, r.MarketUniverse.ActiveEvents)
	if len(r.MarketUniverse.PerCategory) > 0 {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "   category\tmarkets\tevents")
		for _, c := range r.MarketUniverse.PerCategory {
			fmt.Fprintf(tw, "   %s\t%d\t%d\n", c.Slug, c.Markets, c.Events)
		}
		_ = tw.Flush()
	}

	fmt.Fprintf(out, "\n2) Trade input: %d trades across %d events / %d markets / %d wallets.\n",
		r.TradeInput.Trades, r.TradeInput.DistinctEvents,
		r.TradeInput.DistinctMarkets, r.TradeInput.DistinctWallets)

	l := r.Lifecycle
	fmt.Fprintf(out, "\n3) Lifecycle buckets for markets with trades (gate fires at LIFECYCLE_ALERT_FROM_PCT, default 75):\n")
	fmt.Fprintf(out, "   <50%%: %d   50-75%%: %d   75-90%% (eligible): %d   ≥90%% (HOT eligible): %d   unknown_lifecycle: %d\n",
		l.Lt50, l.From50To75, l.From75To90, l.Ge90, l.UnknownLifec)
	if l.Lt50+l.From50To75 > 0 && l.From75To90+l.Ge90 > 0 {
		blocked := l.Lt50 + l.From50To75
		total := l.Lt50 + l.From50To75 + l.From75To90 + l.Ge90
		fmt.Fprintf(out, "   → %d of %d active-trading markets (%.1f%%) blocked by lifecycle gate.\n",
			blocked, total, 100.0*float64(blocked)/float64(total))
	}

	a := r.Alerts
	fmt.Fprintf(out, "\n4) Alerts: %d total / %d events / %d markets.\n",
		a.Total, a.DistinctEvents, a.DistinctMarkets)
	if len(a.BySeverity) > 0 {
		fmt.Fprintf(out, "   severity: ")
		for k, v := range a.BySeverity {
			fmt.Fprintf(out, "%s=%d ", k, v)
		}
		fmt.Fprintln(out)
	}
	if a.Total > 0 {
		fmt.Fprintf(out, "   top event share: %.1f%%   top-3 events: %.1f%%\n",
			a.TopEventShare, a.Top3EventShare)
	}

	s := r.Shadow
	fmt.Fprintf(out, "\n5) Strategy shadow decisions: %d total\n", s.Total)
	if len(s.PerStrategy) > 0 {
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "   strategy\trows")
		for k, v := range s.PerStrategy {
			fmt.Fprintf(tw, "   %s\t%d\n", k, v)
		}
		_ = tw.Flush()
	}

	fmt.Fprintf(out, "\n6) Dedup keys: %d distinct, %d collisions (collisions=alerts sharing a key, ideally 0).\n",
		r.Dedup.DistinctDedupKeys, r.Dedup.DedupCollisions)

	if len(r.TopEvents) > 0 {
		fmt.Fprintf(out, "\n7) Top alerted events (lookback %.1fh):\n", r.LookbackHours)
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "   event_slug\talerts")
		for _, e := range r.TopEvents {
			fmt.Fprintf(tw, "   %s\t%d\n", e.EventSlug, e.Alerts)
		}
		_ = tw.Flush()
	}

	if len(r.Silent) > 0 {
		fmt.Fprintf(out, "\n8) Top input-rich silent markets (trades ≥ 50 but 0 alerts):\n")
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "   event_slug\tcondition_id\ttrades\tlifecycle_pct")
		for _, m := range r.Silent {
			pct := "—"
			if m.LifecyclePct != nil {
				pct = fmt.Sprintf("%.1f%%", *m.LifecyclePct)
			}
			fmt.Fprintf(tw, "   %s\t%s…\t%d\t%s\n",
				m.EventSlug, truncMid(m.ConditionID, 16), m.Trades, pct)
		}
		_ = tw.Flush()
		fmt.Fprintln(out, "   → silent markets with lifecycle_pct < 75 are blocked by LIFECYCLE_ALERT_FROM_PCT.")
		fmt.Fprintln(out, "   → silent markets with lifecycle_pct ≥ 75 failed the absolute/multiplier tier ladder.")
	}

	fmt.Fprintln(out, "\nUser flow / AI / writes: none (read-only).")
}

func truncMid(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
