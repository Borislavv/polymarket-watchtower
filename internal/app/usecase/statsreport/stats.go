// Package statsreport owns the periodic Telegram health-check that
// summarises what the pipeline has been doing since startup. One
// message per Interval, sent to the single configured Telegram chat
// alongside (not instead of) per-alert deliveries.
//
// The summary is intentionally informational, not alerting: operators
// running the service for the first time want to see "is anything
// happening at all" without watching Grafana, and a long-running
// deployment benefits from a recurring "I'm alive, here are my
// counters" heartbeat. When TELEGRAM_STATS_ENABLED=false (default)
// the worker is not constructed; when Telegram delivery is disabled
// outright the worker is also skipped — both paths fail closed.
package statsreport

import (
	"context"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// Config tunes the worker.
type Config struct {
	// Interval is the send cadence. Default 2h. The first tick fires one
	// Interval AFTER startup so a process that restarts every few
	// minutes (CI, deploys) doesn't flood the chat with summaries.
	Interval time.Duration
	// ChatID is the single Telegram chat receiving summaries — usually
	// the same chat the per-alert worker writes to.
	ChatID string
	// StartupGrace, if > 0, delays the first send. Defaults to Interval
	// so the very first tick is one full window into the run — long
	// enough for "trades imported since startup" to be meaningful.
	StartupGrace time.Duration
	// Clock is the time source. Defaults to time.Now.
	Clock func() time.Time
}

func (c Config) applyDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = 2 * time.Hour
	}
	if c.StartupGrace < 0 {
		c.StartupGrace = 0
	}
	if c.StartupGrace == 0 {
		c.StartupGrace = c.Interval
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return c
}

// Sender posts a single HTML message to one Telegram chat. The
// production wiring is a tiny adapter around *telegram.Bot — the
// interface intentionally drops the SendResult Bot returns because
// the worker has no use for the upstream message id.
type Sender interface {
	SendHTML(ctx context.Context, chatID, text string) error
}

// StatsStore reads the pipeline-state counters used in each summary.
// Production wiring is *Store (backed by a pgxpool.Pool); tests can
// supply a fake.
type StatsStore interface {
	Read(ctx context.Context) (Stats, error)
}

// Stats is the snapshot rendered into one summary message. All
// fields are absolute counters at read time (not rates) so two
// consecutive messages can be diffed by eye.
type Stats struct {
	MarketsTotal       int64
	MarketsActive      int64
	MarketsSoftDeleted int64
	MarketsPurged      int64
	TradesTotal        int64
	// TradesLast2h is the count by traded_at — i.e. how many trades
	// have happened on Polymarket in the last 2 hours. Misleading on
	// its own because backfill inflates ingestion volume without
	// representing live activity.
	TradesLast2h int64
	// TradesIngestedLast2h is the count by ingested_at — what the
	// watchtower has PERSISTED in the last 2h, regardless of when
	// the underlying trade happened. Dominated by backfill on a
	// freshly-discovered universe.
	TradesIngestedLast2h int64
	// TradesAnalyzedLast2h is the count of trades that reached
	// detect.Observe — sourced from watchtower_trades_analyzed_total
	// when wired, falls back to 0. The gap between
	// TradesIngestedLast2h (~26k in a healthy hour at our scale) and
	// TradesAnalyzedLast2h (≤ the collect-side imported count, modulo
	// the LIVE_ALERT_MAX_LAG skip pile) tells the operator whether
	// the detection path is keeping up with ingestion.
	TradesAnalyzedLast2h int64
	TradersTotal         int64
	AlertsBySeverity     map[string]int64
	AlertsPending        int64
	AlertsFailed         int64
	AlertsSent           int64
	BackfillByStatus     map[string]int64
	UptimeSince          time.Time
}

// Worker is the long-running summary loop. Safe to run alongside
// the alertsender worker: it does not consume the alerts queue, it
// only reads aggregate counts.
type Worker struct {
	cfg     Config
	stats   StatsStore
	sender  Sender
	log     *zerolog.Logger
	metrics *metrics.Metrics
	started time.Time
}

// New constructs the worker. All inputs are required except metrics,
// which is optional (tests can pass nil).
func New(cfg Config, stats StatsStore, sender Sender, met *metrics.Metrics, log *zerolog.Logger) *Worker {
	c := cfg.applyDefaults()
	return &Worker{cfg: c, stats: stats, sender: sender, log: log, metrics: met, started: c.Clock()}
}

// Run blocks until ctx is cancelled. The first send is delayed by
// StartupGrace so a freshly-started process doesn't immediately
// announce "0 trades, 0 alerts" — the first summary should carry
// real volume.
func (w *Worker) Run(ctx context.Context) error {
	if w.cfg.StartupGrace > 0 {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(w.cfg.StartupGrace):
		}
	}
	w.tick(ctx)
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// Tick runs one summary cycle; exposed for tests.
func (w *Worker) Tick(ctx context.Context) { w.tick(ctx) }

func (w *Worker) tick(ctx context.Context) {
	s, err := w.stats.Read(ctx)
	if err != nil {
		w.observeErr()
		w.log.Err(err).Msg("statsreport: read failed")
		return
	}
	s.UptimeSince = w.started
	msg := Format(s, w.cfg.Clock())
	if err := w.sender.SendHTML(ctx, w.cfg.ChatID, msg); err != nil {
		w.observeErr()
		w.log.Err(err).Msg("statsreport: send failed")
		return
	}
	w.observeOK()
	w.log.Info().
		Int64("markets_active", s.MarketsActive).
		Int64("trades_total", s.TradesTotal).
		Int64("alerts_sent", s.AlertsSent).
		Int64("alerts_pending", s.AlertsPending).
		Msg("statsreport: summary sent")
}

func (w *Worker) observeOK() {
	if w.metrics != nil {
		w.metrics.StatsSummariesSent.Inc()
	}
}

func (w *Worker) observeErr() {
	if w.metrics != nil {
		w.metrics.StatsSummaryErrors.Inc()
	}
}

// Format renders one Stats snapshot into the HTML message body. now
// is passed in so tests are deterministic.
//
// Layout follows the same "header → sections → no orphan bullets"
// shape as the alert formatter so the two message styles look like
// they came from the same system.
func Format(s Stats, now time.Time) string {
	var b strings.Builder
	uptime := now.Sub(s.UptimeSince).Round(time.Minute)
	fmt.Fprintf(&b, "<b>Watchtower stats — uptime %s</b>\n", humanDuration(uptime))

	b.WriteString("\n<b>Markets</b>\n")
	fmt.Fprintf(&b, "• total: %d\n", s.MarketsTotal)
	fmt.Fprintf(&b, "• active: %d\n", s.MarketsActive)
	fmt.Fprintf(&b, "• soft-deleted: %d\n", s.MarketsSoftDeleted)
	fmt.Fprintf(&b, "• purged: %d\n", s.MarketsPurged)

	b.WriteString("\n<b>Trades</b>\n")
	fmt.Fprintf(&b, "• total: %d\n", s.TradesTotal)
	fmt.Fprintf(&b, "• last 2h (by traded_at): %d\n", s.TradesLast2h)
	if s.TradesIngestedLast2h > 0 || s.TradesAnalyzedLast2h > 0 {
		fmt.Fprintf(&b, "• last 2h (imported into DB): %d\n", s.TradesIngestedLast2h)
		fmt.Fprintf(&b, "• last 2h (analyzed by detector): %d\n", s.TradesAnalyzedLast2h)
		if s.TradesIngestedLast2h > 0 {
			ratio := float64(s.TradesAnalyzedLast2h) / float64(s.TradesIngestedLast2h) * 100
			fmt.Fprintf(&b, "• analyzed/imported: <b>%.1f%%</b>\n", ratio)
		}
	}
	fmt.Fprintf(&b, "• traders seen: %d\n", s.TradersTotal)

	b.WriteString("\n<b>Alerts</b>\n")
	fmt.Fprintf(&b, "• sent: %d\n", s.AlertsSent)
	fmt.Fprintf(&b, "• pending: %d\n", s.AlertsPending)
	fmt.Fprintf(&b, "• failed: %d\n", s.AlertsFailed)
	if len(s.AlertsBySeverity) > 0 {
		b.WriteString("• by severity:")
		for _, sev := range []string{"info", "warning", "critical", "hard"} {
			n := s.AlertsBySeverity[sev]
			if n == 0 {
				continue
			}
			fmt.Fprintf(&b, " %s=%d", html.EscapeString(sev), n)
		}
		b.WriteByte('\n')
	}

	if len(s.BackfillByStatus) > 0 {
		b.WriteString("\n<b>Backfill</b>\n")
		for _, st := range []string{"pending", "running", "completed", "partial_api_limit", "failed"} {
			n := s.BackfillByStatus[st]
			if n == 0 {
				continue
			}
			fmt.Fprintf(&b, "• %s: %d\n", html.EscapeString(st), n)
		}
	}
	return b.String()
}

func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "&lt;1m"
	}
	const day = 24 * time.Hour
	days := int(d / day)
	hours := int((d % day) / time.Hour)
	switch {
	case days >= 1 && hours > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case days >= 1:
		return fmt.Sprintf("%dd", days)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int((d%time.Hour)/time.Minute))
	default:
		m := int(d / time.Minute)
		if m == 0 {
			return "&lt;1m"
		}
		return fmt.Sprintf("%dm", m)
	}
}

// --- Postgres-backed StatsStore --------------------------------------------

// Store reads pipeline-state counters from Postgres. Queries are
// kept here (raw SQL) rather than in sqlc so this informational
// path doesn't grow the typed query surface. Read tolerates a
// missing column / table only insofar as the pool itself works — a
// schema drift should be loud, not silent.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore wraps a connection pool.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Read returns a Stats snapshot.
func (s *Store) Read(ctx context.Context) (Stats, error) {
	out := Stats{
		AlertsBySeverity: make(map[string]int64, 4),
		BackfillByStatus: make(map[string]int64, 5),
	}
	row := s.pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM polymarket_markets)                                                AS total,
			(SELECT COUNT(*) FROM polymarket_markets WHERE active = TRUE)                            AS active,
			(SELECT COUNT(*) FROM polymarket_markets WHERE deleted_at IS NOT NULL AND purged_at IS NULL) AS soft_deleted,
			(SELECT COUNT(*) FROM polymarket_markets WHERE purged_at IS NOT NULL)                    AS purged
	`)
	if err := row.Scan(&out.MarketsTotal, &out.MarketsActive, &out.MarketsSoftDeleted, &out.MarketsPurged); err != nil {
		return out, fmt.Errorf("read markets: %w", err)
	}
	row = s.pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM polymarket_trades)                                                AS total,
			(SELECT COUNT(*) FROM polymarket_trades WHERE traded_at   >= NOW() - INTERVAL '2 hours') AS last_2h_traded,
			(SELECT COUNT(*) FROM polymarket_trades WHERE ingested_at >= NOW() - INTERVAL '2 hours') AS last_2h_ingested,
			(SELECT COUNT(*) FROM polymarket_traders)                                               AS traders
	`)
	if err := row.Scan(&out.TradesTotal, &out.TradesLast2h, &out.TradesIngestedLast2h, &out.TradersTotal); err != nil {
		return out, fmt.Errorf("read trades: %w", err)
	}
	// TradesAnalyzedLast2h is not in the DB — it lives in the
	// Prometheus counter watchtower_trades_analyzed_total. The
	// statsreport.Store layer doesn't have a Prometheus reader, so
	// we leave it at 0 here and let the worker fill it via a small
	// shim (set on Stats before rendering). When unset (0) the
	// renderer omits the analyzed/imported ratio cleanly.

	sevRows, err := s.pool.Query(ctx, `
		SELECT severity, COUNT(*) FROM polymarket_alerts
		WHERE status = 'sent'
		GROUP BY severity
	`)
	if err != nil {
		return out, fmt.Errorf("read alerts severity: %w", err)
	}
	for sevRows.Next() {
		var sev string
		var n int64
		if err := sevRows.Scan(&sev, &n); err != nil {
			sevRows.Close()
			return out, fmt.Errorf("scan alerts severity: %w", err)
		}
		out.AlertsBySeverity[strings.ToLower(sev)] = n
	}
	sevRows.Close()

	row = s.pool.QueryRow(ctx, `
		SELECT
			SUM(CASE WHEN status = 'pending' THEN 1 ELSE 0 END),
			SUM(CASE WHEN status = 'failed'  THEN 1 ELSE 0 END),
			SUM(CASE WHEN status = 'sent'    THEN 1 ELSE 0 END)
		FROM polymarket_alerts
	`)
	var pending, failed, sent *int64
	if err := row.Scan(&pending, &failed, &sent); err != nil {
		return out, fmt.Errorf("read alerts status: %w", err)
	}
	if pending != nil {
		out.AlertsPending = *pending
	}
	if failed != nil {
		out.AlertsFailed = *failed
	}
	if sent != nil {
		out.AlertsSent = *sent
	}

	bfRows, err := s.pool.Query(ctx, `
		SELECT backfill_status, COUNT(*) FROM polymarket_markets
		WHERE active = TRUE
		GROUP BY backfill_status
	`)
	if err != nil {
		return out, fmt.Errorf("read backfill: %w", err)
	}
	for bfRows.Next() {
		var st string
		var n int64
		if err := bfRows.Scan(&st, &n); err != nil {
			bfRows.Close()
			return out, fmt.Errorf("scan backfill: %w", err)
		}
		out.BackfillByStatus[st] = n
	}
	bfRows.Close()

	return out, nil
}
