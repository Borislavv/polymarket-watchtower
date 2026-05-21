// realtime_repository.go — v10.4 WebSocket realtime persistence.
// Wraps the sqlc-generated helpers for the four v10.4 tables.
package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// RealtimeRepository is the persistence seam for the realtime
// worker. nil-safe construction is the caller's responsibility.
type RealtimeRepository struct {
	q *sqlc.Queries
}

func NewRealtimeRepository(pool *pgxpool.Pool) *RealtimeRepository {
	return &RealtimeRepository{q: sqlc.New(pool)}
}

// WSEventRow mirrors the polymarket_ws_events insert shape.
type WSEventRow struct {
	ReceivedAt        time.Time
	ExchangeTimestamp *time.Time
	EventType         string
	EventSlug         string
	ConditionID       string
	MarketSlug        string
	CLOBTokenID       string
	Outcome           string
	Price             *float64
	Size              *float64
	Side              string
	SideSource        string
	SideConfidence    float64
	BestBid           *float64
	BestAsk           *float64
	Mid               *float64
	TxHash            string
	TradeID           string
	Wallet            string
	Sequence          string
	RawJSON           []byte
	RawHash           string
}

// InsertWSEvent persists one normalized WS event. Fail-open at the
// caller layer — a write error here MUST NOT block the read loop.
func (r *RealtimeRepository) InsertWSEvent(ctx context.Context, in WSEventRow) error {
	return r.q.InsertWSEvent(ctx, sqlc.InsertWSEventParams{
		ReceivedAt:        tsFromTime(in.ReceivedAt),
		ExchangeTimestamp: tsFromTimePtr(in.ExchangeTimestamp),
		EventType:         in.EventType,
		EventSlug:         nullableStr(in.EventSlug),
		ConditionID:       nullableStr(in.ConditionID),
		MarketSlug:        nullableStr(in.MarketSlug),
		ClobTokenID:       nullableStr(in.CLOBTokenID),
		Outcome:           nullableStr(in.Outcome),
		Price:             in.Price,
		Size:              in.Size,
		Side:              nullableStr(in.Side),
		SideSource:        in.SideSource,
		SideConfidence:    in.SideConfidence,
		BestBid:           in.BestBid,
		BestAsk:           in.BestAsk,
		Mid:               in.Mid,
		TxHash:            nullableStr(in.TxHash),
		TradeID:           nullableStr(in.TradeID),
		Wallet:            nullableStr(in.Wallet),
		Sequence:          nullableStr(in.Sequence),
		RawJson:           in.RawJSON,
		RawHash:           nullableStr(in.RawHash),
	})
}

// LiveMarketStateRow mirrors polymarket_live_market_state.
type LiveMarketStateRow struct {
	ConditionID   string
	EventSlug     string
	MarketSlug    string
	BestBid       *float64
	BestAsk       *float64
	Mid           *float64
	LastPrice     *float64
	LastTradeAt   *time.Time
	LastWSEventAt *time.Time
	WSConnected   bool
}

// UpsertLiveMarketState writes the latest top-of-book / mid / last-
// price snapshot. Always idempotent.
func (r *RealtimeRepository) UpsertLiveMarketState(ctx context.Context, in LiveMarketStateRow) error {
	return r.q.UpsertLiveMarketState(ctx, sqlc.UpsertLiveMarketStateParams{
		ConditionID:   in.ConditionID,
		EventSlug:     nullableStr(in.EventSlug),
		MarketSlug:    nullableStr(in.MarketSlug),
		BestBid:       in.BestBid,
		BestAsk:       in.BestAsk,
		Mid:           in.Mid,
		LastPrice:     in.LastPrice,
		LastTradeAt:   tsFromTimePtr(in.LastTradeAt),
		LastWsEventAt: tsFromTimePtr(in.LastWSEventAt),
		WsConnected:   in.WSConnected,
	})
}

// GetLiveMarketState returns the latest persisted snapshot for one
// condition_id. (nil, ErrPredictionNotFound)-style pattern would be
// nicer but the worker treats "no row" as "no prior state".
func (r *RealtimeRepository) GetLiveMarketState(ctx context.Context, conditionID string) (LiveMarketStateRow, bool, error) {
	row, err := r.q.GetLiveMarketState(ctx, conditionID)
	if err != nil {
		return LiveMarketStateRow{}, false, nil // fail-open on read
	}
	out := LiveMarketStateRow{
		ConditionID: row.ConditionID,
		WSConnected: row.WsConnected,
		BestBid:     row.BestBid,
		BestAsk:     row.BestAsk,
		Mid:         row.Mid,
		LastPrice:   row.LastPrice,
	}
	if row.EventSlug != nil {
		out.EventSlug = *row.EventSlug
	}
	if row.MarketSlug != nil {
		out.MarketSlug = *row.MarketSlug
	}
	if row.LastTradeAt.Valid {
		t := row.LastTradeAt.Time
		out.LastTradeAt = &t
	}
	if row.LastWsEventAt.Valid {
		t := row.LastWsEventAt.Time
		out.LastWSEventAt = &t
	}
	return out, true, nil
}

// SetLiveMarketWSConnected bulk-flips ws_connected for many markets
// when the WS client connects / disconnects.
func (r *RealtimeRepository) SetLiveMarketWSConnected(ctx context.Context, conditionIDs []string, connected bool) error {
	if len(conditionIDs) == 0 {
		return nil
	}
	return r.q.SetLiveMarketWSConnected(ctx, sqlc.SetLiveMarketWSConnectedParams{
		WsConnected:  connected,
		ConditionIds: conditionIDs,
	})
}

// EnqueueRealtimeWorkRow is the polymarket_realtime_work_queue
// insert shape. dedupe_key is the operator-visible idempotency
// primitive — typically `condition_id + reason + minute_bucket`.
type EnqueueRealtimeWorkRow struct {
	ConditionID string
	EventSlug   string
	Reason      string
	Priority    int16
	DedupeKey   string
	AvailableAt time.Time
}

// EnqueueRealtimeWork inserts one queue row. ON CONFLICT(dedupe_key)
// DO NOTHING means burst-of-events for the same market collapses to
// one queue row — exactly what the v10.4 spec asks for.
func (r *RealtimeRepository) EnqueueRealtimeWork(ctx context.Context, in EnqueueRealtimeWorkRow) error {
	return r.q.EnqueueRealtimeWork(ctx, sqlc.EnqueueRealtimeWorkParams{
		ConditionID: nullableStr(in.ConditionID),
		EventSlug:   nullableStr(in.EventSlug),
		Reason:      in.Reason,
		Priority:    in.Priority,
		DedupeKey:   in.DedupeKey,
		AvailableAt: tsFromTime(in.AvailableAt),
	})
}

// RealtimeWorkClaim mirrors one claimed row.
type RealtimeWorkClaim struct {
	ID          int64
	ConditionID string
	EventSlug   string
	Reason      string
	Priority    int16
	DedupeKey   string
	Attempts    int32
}

// ClaimRealtimeWorkBatch atomically claims up to `limit` due rows.
func (r *RealtimeRepository) ClaimRealtimeWorkBatch(ctx context.Context, limit int32) ([]RealtimeWorkClaim, error) {
	rows, err := r.q.ClaimRealtimeWorkBatch(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]RealtimeWorkClaim, 0, len(rows))
	for _, r := range rows {
		claim := RealtimeWorkClaim{
			ID:        r.ID,
			Reason:    r.Reason,
			Priority:  r.Priority,
			DedupeKey: r.DedupeKey,
			Attempts:  r.Attempts,
		}
		if r.ConditionID != nil {
			claim.ConditionID = *r.ConditionID
		}
		if r.EventSlug != nil {
			claim.EventSlug = *r.EventSlug
		}
		out = append(out, claim)
	}
	return out, nil
}

// MarkRealtimeWorkFailed re-queues the row with last_error stamped
// and available_at bumped 1m into the future.
func (r *RealtimeRepository) MarkRealtimeWorkFailed(ctx context.Context, id int64, lastError string) error {
	return r.q.MarkRealtimeWorkFailed(ctx, sqlc.MarkRealtimeWorkFailedParams{
		ID:        id,
		LastError: nullableStr(lastError),
	})
}

// DeleteOldRealtimeWork is the periodic cleanup the worker runs.
func (r *RealtimeRepository) DeleteOldRealtimeWork(ctx context.Context, olderThan time.Time) error {
	return r.q.DeleteOldRealtimeWork(ctx, tsFromTime(olderThan))
}

// InsertGapRecovery opens an audit row; FinishGapRecovery closes it.
func (r *RealtimeRepository) InsertGapRecovery(ctx context.Context, conditionID string, lookbackStart, lookbackEnd time.Time) (int64, error) {
	return r.q.InsertGapRecovery(ctx, sqlc.InsertGapRecoveryParams{
		ConditionID:   conditionID,
		LookbackStart: tsFromTime(lookbackStart),
		LookbackEnd:   tsFromTime(lookbackEnd),
	})
}

// FinishGapRecovery closes an audit row.
func (r *RealtimeRepository) FinishGapRecovery(ctx context.Context, id int64, status string, recovered int32, lastError string) error {
	return r.q.FinishGapRecovery(ctx, sqlc.FinishGapRecoveryParams{
		ID:              id,
		Status:          status,
		RecoveredTrades: recovered,
		LastError:       nullableStr(lastError),
	})
}

// --- small helpers ---------------------------------------------------------
//
// tsFromTimePtr returns a NULL pgtype.Timestamptz when the input
// pointer is nil; otherwise the canonical conversion. Keeps the
// nullable-time call sites from sprinkling nil checks everywhere.

func tsFromTimePtr(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return tsFromTime(*t)
}

func nullableStr(s string) *string {
	if s == "" {
		return nil
	}
	v := s
	return &v
}
