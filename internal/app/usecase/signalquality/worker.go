package signalquality

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

// Sender is the typed Telegram seam — *telegram.Router satisfies
// it. The worker labels every send SurfaceSignalQualityReport so
// the router maps it onto the admin chat.
type Sender interface {
	Send(ctx context.Context, msg telegram.Message) (telegram.SendResult, error)
}

// Config tunes the periodic signal-quality cycle. The default
// schedule is once per day at 08:00 UTC; operators can pick a
// different time-of-day via TELEGRAM_ADMIN_SIGNAL_QUALITY_*.
//
// Hard rule: the worker NEVER constructs without a Sender (the
// caller decides whether admin routing is wired). Without a
// Sender the worker stays idle but the Tick can still be invoked
// for dry-run audits.
type Config struct {
	Enabled       bool
	Interval      time.Duration // default 24h
	StartupGrace  time.Duration // delay before the first send
	Period        Period        // banner label; defaults to PeriodDaily
	MinSampleSize int           // banner threshold; defaults to 100
	Clock         func() time.Time
}

func (c Config) applyDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = 24 * time.Hour
	}
	if c.StartupGrace < 0 {
		c.StartupGrace = 0
	}
	if c.Period == "" {
		c.Period = PeriodDaily
	}
	if c.MinSampleSize <= 0 {
		c.MinSampleSize = 100
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
	return c
}

// Reader is the seam over polymarket_alerts. Production wiring is
// *Store (backed by pgxpool); tests pass a fake.
type Reader interface {
	ReadSnapshot(ctx context.Context, asOf time.Time) (Snapshot, error)
}

// Worker is the periodic admin-channel sender.
type Worker struct {
	cfg    Config
	reader Reader
	sender Sender
	log    *zerolog.Logger
}

func New(cfg Config, reader Reader, sender Sender, log *zerolog.Logger) *Worker {
	return &Worker{cfg: cfg.applyDefaults(), reader: reader, sender: sender, log: log}
}

// Run blocks until ctx cancels. Honors StartupGrace then ticks at
// Interval. When cfg.Enabled is false or the sender is nil the
// worker idles — useful for the dev-mode wiring where no admin
// chat is configured.
func (w *Worker) Run(ctx context.Context) error {
	if !w.cfg.Enabled || w.sender == nil {
		<-ctx.Done()
		return nil
	}
	if w.cfg.StartupGrace > 0 {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(w.cfg.StartupGrace):
		}
	}
	w.Tick(ctx)
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.Tick(ctx)
		}
	}
}

// Tick runs one cycle: read snapshot → format → send. Exposed for
// tests + the future CLI dry-run.
func (w *Worker) Tick(ctx context.Context) {
	snap, err := w.reader.ReadSnapshot(ctx, w.cfg.Clock())
	if err != nil {
		if w.log != nil {
			w.log.Warn().Err(err).Msg("signalquality: read snapshot failed")
		}
		return
	}
	snap.Period = w.cfg.Period
	if snap.MinSampleSize <= 0 {
		snap.MinSampleSize = w.cfg.MinSampleSize
	}
	body := Format(snap)
	if strings.TrimSpace(body) == "" {
		return
	}
	if _, err := w.sender.Send(ctx, telegram.Message{
		Surface: telegram.SurfaceSignalQualityReport,
		HTML:    body,
	}); err != nil && w.log != nil {
		w.log.Warn().Err(err).Msg("signalquality: telegram send failed")
	}
}

// --- Postgres-backed Reader ----------------------------------------------

// Store reads aggregate counts from polymarket_alerts. The query
// surface is small and deterministic; no AI, no event-page calls.
//
// v11.4: queries are bounded by SIGNAL_QUALITY_*_LOOKBACK and
// SIGNAL_QUALITY_MAX_ALERTS. The Store reads only rows whose
// sent_at is within Lookback of asOf and stops after MaxAlerts is
// reached. When MaxAlerts is hit the renderer surfaces a
// truncated=true banner so the operator knows the body is
// directional, not exact.
type Store struct {
	pool      *pgxpool.Pool
	lookback  time.Duration
	maxAlerts int
}

// NewStore wraps a connection pool with the default v11.4 limits
// (7-day lookback, 5000-row cap). Use NewStoreWithLimits to
// customise.
func NewStore(pool *pgxpool.Pool) *Store {
	return NewStoreWithLimits(pool, 7*24*time.Hour, 5000)
}

// NewStoreWithLimits is the production constructor — wired with
// the operator-configured lookback + cap.
func NewStoreWithLimits(pool *pgxpool.Pool, lookback time.Duration, maxAlerts int) *Store {
	if lookback <= 0 {
		lookback = 7 * 24 * time.Hour
	}
	if maxAlerts <= 0 {
		maxAlerts = 5000
	}
	return &Store{pool: pool, lookback: lookback, maxAlerts: maxAlerts}
}

// ReadSnapshot pulls a single Snapshot. v11.4: bounded by the
// configured lookback + maxAlerts. The aggregate counts come from
// a single CTE that caps the scanned row set at MaxAlerts using a
// recent-first ORDER BY + LIMIT, then GROUPs the capped set. If
// the cap is hit, Snapshot.Truncated = true.
func (s *Store) ReadSnapshot(ctx context.Context, asOf time.Time) (Snapshot, error) {
	if s == nil || s.pool == nil {
		return Snapshot{}, errors.New("signalquality: nil pool")
	}
	out := Snapshot{
		ReportDate:        asOf,
		LookbackHours:     int(s.lookback.Hours()),
		MaxAlertsScanCap:  s.maxAlerts,
	}
	since := asOf.Add(-s.lookback)

	// Eligible count: cheap COUNT() bounded by the lookback window.
	// We never run COUNT(*) over the full alerts table.
	var eligibleCount int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM polymarket_alerts
		WHERE status = 'sent'
		  AND sent_at >= $1
	`, since).Scan(&eligibleCount); err != nil {
		return Snapshot{}, err
	}
	out.EligibleCount = eligibleCount
	if eligibleCount > s.maxAlerts {
		out.Truncated = true
	}

	// Bounded aggregate scan: cap the row set with a recent-first
	// LIMIT, then aggregate. When eligible_count <= max_alerts the
	// CTE returns exactly the right set; when it exceeds the cap
	// the operator sees a directional view + Truncated marker.
	row := s.pool.QueryRow(ctx, `
		WITH bounded AS (
			SELECT outcome_status, status
			FROM polymarket_alerts
			WHERE status = 'sent'
			  AND sent_at >= $1
			ORDER BY sent_at DESC
			LIMIT $2
		)
		SELECT
			COUNT(*),
			COUNT(*) FILTER (WHERE outcome_status = 'resolved_correct'),
			COUNT(*) FILTER (WHERE outcome_status = 'resolved_wrong'),
			COUNT(*) FILTER (WHERE outcome_status NOT IN ('resolved_correct','resolved_wrong'))
		FROM bounded
	`, since, s.maxAlerts)
	if err := row.Scan(&out.TotalSent, &out.ResolvedCorrect, &out.ResolvedWrong, &out.Unresolved); err != nil {
		return Snapshot{}, err
	}
	out.ScannedCount = out.TotalSent

	kindRows, err := s.pool.Query(ctx, `
		WITH bounded AS (
			SELECT kind, outcome_status
			FROM polymarket_alerts
			WHERE status = 'sent'
			  AND sent_at >= $1
			ORDER BY sent_at DESC
			LIMIT $2
		)
		SELECT
			kind,
			COUNT(*) FILTER (WHERE outcome_status = 'resolved_correct'),
			COUNT(*) FILTER (WHERE outcome_status = 'resolved_wrong'),
			COUNT(*) FILTER (WHERE outcome_status NOT IN ('resolved_correct','resolved_wrong'))
		FROM bounded
		GROUP BY kind
		ORDER BY kind
	`, since, s.maxAlerts)
	if err != nil {
		return Snapshot{}, err
	}
	for kindRows.Next() {
		var k KindBreakdown
		if err := kindRows.Scan(&k.Kind, &k.Success, &k.Failure, &k.Unresolved); err != nil {
			kindRows.Close()
			return Snapshot{}, err
		}
		out.Kinds = append(out.Kinds, k)
	}
	kindRows.Close()

	sevRows, err := s.pool.Query(ctx, `
		WITH bounded AS (
			SELECT severity, outcome_status
			FROM polymarket_alerts
			WHERE status = 'sent'
			  AND sent_at >= $1
			ORDER BY sent_at DESC
			LIMIT $2
		)
		SELECT
			severity,
			COUNT(*) FILTER (WHERE outcome_status = 'resolved_correct'),
			COUNT(*) FILTER (WHERE outcome_status = 'resolved_wrong'),
			COUNT(*) FILTER (WHERE outcome_status NOT IN ('resolved_correct','resolved_wrong'))
		FROM bounded
		GROUP BY severity
		ORDER BY
			CASE severity
				WHEN 'info' THEN 1
				WHEN 'warning' THEN 2
				WHEN 'critical' THEN 3
				WHEN 'hard' THEN 4
				ELSE 5
			END
	`, since, s.maxAlerts)
	if err != nil {
		return Snapshot{}, err
	}
	for sevRows.Next() {
		var sv SeverityBreakdown
		if err := sevRows.Scan(&sv.Severity, &sv.Success, &sv.Failure, &sv.Unresolved); err != nil {
			sevRows.Close()
			return Snapshot{}, err
		}
		out.Severities = append(out.Severities, sv)
	}
	sevRows.Close()
	return out, nil
}
