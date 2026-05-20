// Package eventflow is the deterministic event-level flow
// aggregation used by every AI prompt that needs real Watchtower
// context (daily intel, catalyst importer, prediction prompts).
//
// One Repository call per event returns an EventFlowSummary that
// rolls up:
//
//   - alert counts by severity and kind;
//   - strongest outcome / side / condition_id by signed notional;
//   - same-side / opposite-side notional + net directional balance;
//   - largest trade in the window;
//   - top-N alerts + top-N trades for the prompt block.
//
// Failure semantics are silent: a DB error returns an EventFlowSummary
// with WindowStart/End set and the rest zero so the renderer emits an
// explicit "no meaningful stored flow" sentence. The alert / daily /
// catalyst paths NEVER block on this loader.
//
// Polymarket-authored fields (market question, wallet) are DATA. The
// renderer truncates wallets and HTML-safe-text trades; nothing here
// is fed to the model as instructions.
package eventflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// Config tunes the loader.
type Config struct {
	Enabled          bool
	Lookback         time.Duration
	MaxAlerts        int
	MaxTrades        int
	MinLargeTradeUSD float64
	TopItems         int
}

func (c *Config) applyDefaults() {
	if c.Lookback <= 0 {
		c.Lookback = 24 * time.Hour
	}
	if c.MaxAlerts <= 0 {
		c.MaxAlerts = 25
	}
	if c.MaxTrades <= 0 {
		c.MaxTrades = 150
	}
	if c.MinLargeTradeUSD <= 0 {
		c.MinLargeTradeUSD = 10_000
	}
	if c.TopItems <= 0 {
		c.TopItems = 10
	}
}

// EventFlowAlert is one summarised alert row for the prompt.
type EventFlowAlert struct {
	ID          int64
	Kind        string
	Severity    string
	Reason      string
	ConditionID string
	MarketSlug  string
	Question    string
	CreatedAt   time.Time
}

// EventFlowTrade is one summarised trade row for the prompt.
type EventFlowTrade struct {
	ID           int64
	ConditionID  string
	OutcomeToken string
	Side         string
	Price        float64
	NotionalUSD  float64
	Wallet       string
	TradedAt     time.Time
}

// EventFlowSummary is the rich aggregate the AI prompt sees.
type EventFlowSummary struct {
	EventSlug   string
	Lookback    time.Duration
	WindowStart time.Time
	WindowEnd   time.Time

	RecentAlerts   int
	InfoAlerts     int
	WarningAlerts  int
	CriticalAlerts int
	HardAlerts     int

	AlertKinds      map[string]int
	AlertSeverities map[string]int

	StrongestOutcome     string
	StrongestSide        string
	StrongestConditionID string

	SameSideNotionalUSD       float64
	OppositeSideNotionalUSD   float64
	NetDirectionalNotionalUSD float64
	DirectionalImbalance      float64

	LargestTradeUSD     float64
	LargestTradeWallet  string
	LargestTradeOutcome string
	LargestTradeSide    string
	LargestTradeAt      time.Time

	AccumulationAlerts   int
	ClusterAlerts        int
	OwnershipAlerts      int
	StableFavoriteAlerts int
	NewWalletAlerts      int
	WhaleFlowAlerts      int

	AccumulationNote   string
	OwnershipNote      string
	ClusterNote        string
	StableFavoriteNote string
	WhaleNote          string

	TopAlerts []EventFlowAlert
	TopTrades []EventFlowTrade
}

// Empty reports whether the summary has no rows. Renderers branch on
// this rather than checking individual fields.
func (s EventFlowSummary) Empty() bool {
	return s.RecentAlerts == 0 && len(s.TopTrades) == 0 && s.LargestTradeUSD == 0
}

// Repository is the seam. *EventFlowRepository (below) satisfies it.
type Repository interface {
	LoadEventFlowSummary(ctx context.Context, eventSlug string, lookback time.Duration) (EventFlowSummary, error)
}

// EventFlowRepository wraps the sqlc-generated aggregation queries
// in a single fail-open loader.
type EventFlowRepository struct {
	q       *sqlc.Queries
	cfg     Config
	metrics *metrics.Metrics
	log     *zerolog.Logger
}

// New wires the repository. The sqlc queries object is pulled from
// the same pgx pool the rest of the repository layer uses; we don't
// take a pgxpool here to keep the boundary tight.
func New(q *sqlc.Queries, cfg Config, met *metrics.Metrics, log *zerolog.Logger) *EventFlowRepository {
	cfg.applyDefaults()
	return &EventFlowRepository{q: q, cfg: cfg, metrics: met, log: log}
}

// LoadEventFlowSummary issues four parallel-friendly queries and
// folds them into one EventFlowSummary. Lookback=0 falls back to
// the configured default; the supplied value otherwise overrides.
func (r *EventFlowRepository) LoadEventFlowSummary(ctx context.Context, eventSlug string, lookback time.Duration) (EventFlowSummary, error) {
	if !r.cfg.Enabled {
		return EventFlowSummary{EventSlug: eventSlug}, nil
	}
	if eventSlug == "" {
		return EventFlowSummary{}, nil
	}
	if lookback <= 0 {
		lookback = r.cfg.Lookback
	}
	end := time.Now().UTC()
	start := end.Add(-lookback)
	out := EventFlowSummary{
		EventSlug:       eventSlug,
		Lookback:        lookback,
		WindowStart:     start,
		WindowEnd:       end,
		AlertKinds:      map[string]int{},
		AlertSeverities: map[string]int{},
	}
	slugPtr := &eventSlug
	since := pgtype.Timestamptz{Time: start, Valid: true}

	// 1. Alert kind/severity counts.
	if rows, err := r.q.AggregateEventAlertsByKindAndSeverity(ctx,
		sqlc.AggregateEventAlertsByKindAndSeverityParams{
			EventSlug: slugPtr, Since: since,
		}); err == nil {
		for _, row := range rows {
			n := int(row.Count)
			out.RecentAlerts += n
			out.AlertKinds[row.Kind] += n
			out.AlertSeverities[row.Severity] += n
			switch row.Severity {
			case "info":
				out.InfoAlerts += n
			case "warning":
				out.WarningAlerts += n
			case "critical":
				out.CriticalAlerts += n
			case "hard":
				out.HardAlerts += n
			}
		}
		out.AccumulationAlerts = out.AlertKinds["accumulation"]
		out.ClusterAlerts = out.AlertKinds["category_watch"]
		out.OwnershipAlerts = out.AlertKinds["ownership_concentration"]
		out.StableFavoriteAlerts = out.AlertKinds["stable_favorite"]
		out.WhaleFlowAlerts = out.AlertKinds["trade_anomaly"]
		// new_wallet alerts are flagged as reason codes on top of
		// other kinds; we surface the counter only via the top-alerts
		// reason list below.
		_ = out.NewWalletAlerts
	} else {
		r.observe("alerts_failed")
		r.warn(err, eventSlug, "alerts aggregate failed")
	}

	// 2. Top alerts.
	if rows, err := r.q.ListEventTopAlerts(ctx, sqlc.ListEventTopAlertsParams{
		EventSlug:  slugPtr,
		Since:      since,
		LimitCount: int32(r.cfg.MaxAlerts),
	}); err == nil {
		max := r.cfg.TopItems
		if max > len(rows) {
			max = len(rows)
		}
		out.TopAlerts = make([]EventFlowAlert, 0, max)
		for i, row := range rows {
			if i >= max {
				break
			}
			out.TopAlerts = append(out.TopAlerts, EventFlowAlert{
				ID:          row.ID,
				Kind:        row.Kind,
				Severity:    row.Severity,
				Reason:      row.Reason,
				ConditionID: row.ConditionID,
				MarketSlug:  row.MarketSlug,
				Question:    row.Question,
				CreatedAt:   row.CreatedAt.Time,
			})
			if strings.Contains(row.Reason, "NEW_WALLET") {
				out.NewWalletAlerts++
			}
		}
	} else {
		r.warn(err, eventSlug, "top alerts failed")
	}

	// 3. Per-(condition, outcome, side) trade sums.
	if rows, err := r.q.SumEventTradesByConditionAndSide(ctx,
		sqlc.SumEventTradesByConditionAndSideParams{
			EventSlug: slugPtr, Since: since,
		}); err == nil {
		// Aggregate per (condition, outcome, side); pick strongest by
		// absolute notional. Track BUY vs SELL by outcome to compute
		// directional imbalance: BUY of outcome A and SELL of outcome
		// B are economically symmetric, but we keep them separate so
		// the AI sees the raw structure.
		type cell struct {
			conditionID  string
			outcomeToken string
			side         string
			notionalUSD  float64
			tradeCount   int64
		}
		var strongest cell
		for _, row := range rows {
			c := cell{
				conditionID: row.ConditionID, outcomeToken: row.OutcomeToken,
				side: row.Side, notionalUSD: row.NotionalUsd, tradeCount: row.TradeCount,
			}
			if c.notionalUSD > strongest.notionalUSD {
				strongest = c
			}
		}
		if strongest.notionalUSD > 0 {
			out.StrongestOutcome = strongest.outcomeToken
			out.StrongestSide = strongest.side
			out.StrongestConditionID = strongest.conditionID
			// Sum same-side vs opposite-side for the SAME (condition,
			// outcome). The "side" axis is BUY vs SELL on this outcome
			// — opposite-side flow is real resistance from the same
			// outcome's other side. Cross-condition flow within an
			// event is intentionally not netted here; an event with
			// two markets ("will X win" vs "will Y win") has its own
			// directional structure the AI must reason about.
			same, opp := 0.0, 0.0
			for _, row := range rows {
				if row.ConditionID != strongest.conditionID || row.OutcomeToken != strongest.outcomeToken {
					continue
				}
				if row.Side == strongest.side {
					same += row.NotionalUsd
				} else {
					opp += row.NotionalUsd
				}
			}
			out.SameSideNotionalUSD = same
			out.OppositeSideNotionalUSD = opp
			out.NetDirectionalNotionalUSD = same - opp
			tot := same + opp
			if tot > 0 {
				out.DirectionalImbalance = (same - opp) / tot
			}
		}
	} else {
		r.warn(err, eventSlug, "trade sums failed")
	}

	// 4. Top trades + largest trade.
	if rows, err := r.q.ListEventTopTrades(ctx, sqlc.ListEventTopTradesParams{
		EventSlug:  slugPtr,
		Since:      since,
		MinUsd:     r.cfg.MinLargeTradeUSD,
		LimitCount: int32(r.cfg.MaxTrades),
	}); err == nil {
		cap := r.cfg.TopItems
		if cap > len(rows) {
			cap = len(rows)
		}
		out.TopTrades = make([]EventFlowTrade, 0, cap)
		for i, row := range rows {
			tr := EventFlowTrade{
				ID:           row.ID,
				ConditionID:  row.ConditionID,
				OutcomeToken: row.OutcomeToken,
				Side:         row.Side,
				Price:        row.Price,
				NotionalUSD:  row.NotionalUsd,
				Wallet:       row.Wallet,
				TradedAt:     row.TradedAt.Time,
			}
			if i < cap {
				out.TopTrades = append(out.TopTrades, tr)
			}
			if tr.NotionalUSD > out.LargestTradeUSD {
				out.LargestTradeUSD = tr.NotionalUSD
				out.LargestTradeWallet = tr.Wallet
				out.LargestTradeOutcome = tr.OutcomeToken
				out.LargestTradeSide = tr.Side
				out.LargestTradeAt = tr.TradedAt
			}
		}
	} else {
		r.warn(err, eventSlug, "top trades failed")
	}

	out.AccumulationNote = renderAccumulationNote(out)
	out.OwnershipNote = renderOwnershipNote(out)
	out.ClusterNote = renderClusterNote(out)
	out.StableFavoriteNote = renderStableFavoriteNote(out)
	out.WhaleNote = renderWhaleNote(out)

	if out.Empty() {
		r.observe("empty")
		if r.metrics != nil && r.metrics.EventFlowSummaryEmpty != nil {
			r.metrics.EventFlowSummaryEmpty.Inc()
		}
	} else {
		r.observe("ok")
	}
	return out, nil
}

func (r *EventFlowRepository) observe(status string) {
	if r.metrics == nil || r.metrics.EventFlowSummaryLoad == nil {
		return
	}
	r.metrics.EventFlowSummaryLoad.WithLabelValues(status).Inc()
}

func (r *EventFlowRepository) warn(err error, eventSlug, what string) {
	if r.log == nil {
		return
	}
	r.log.Warn().Err(err).Str("event_slug", eventSlug).Msg("eventflow: " + what)
}

// --- notes ---------------------------------------------------------------

func renderAccumulationNote(s EventFlowSummary) string {
	if s.AccumulationAlerts == 0 {
		return ""
	}
	return fmt.Sprintf("%d accumulation alerts in last %s", s.AccumulationAlerts, roundDur(s.Lookback))
}

func renderOwnershipNote(s EventFlowSummary) string {
	if s.OwnershipAlerts == 0 {
		return ""
	}
	return fmt.Sprintf("%d ownership-concentration alerts (flow-based approximation)", s.OwnershipAlerts)
}

func renderClusterNote(s EventFlowSummary) string {
	if s.ClusterAlerts == 0 {
		return ""
	}
	return fmt.Sprintf("%d category-watch cluster alerts", s.ClusterAlerts)
}

func renderStableFavoriteNote(s EventFlowSummary) string {
	if s.StableFavoriteAlerts == 0 {
		return ""
	}
	return fmt.Sprintf("%d stable-favorite alerts", s.StableFavoriteAlerts)
}

func renderWhaleNote(s EventFlowSummary) string {
	if s.WhaleFlowAlerts == 0 {
		return ""
	}
	return fmt.Sprintf("%d single-trade whale-flow alerts", s.WhaleFlowAlerts)
}

func roundDur(d time.Duration) string {
	if d >= time.Hour {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dm", int(d/time.Minute))
}
