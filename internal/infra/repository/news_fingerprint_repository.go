// news_fingerprint_repository.go — v10.7 news-driven AI gating.
package repository

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
)

// NewsFingerprintRepository wraps the polymarket_event_news_fingerprints
// table. The row keeps both the "is the news set unchanged?" signal
// (load-bearing for AI gating) AND the most recent semantic-output
// fingerprint for the cooldown check.
type NewsFingerprintRepository struct {
	q *sqlc.Queries
}

func NewNewsFingerprintRepository(pool *pgxpool.Pool) *NewsFingerprintRepository {
	return &NewsFingerprintRepository{q: sqlc.New(pool)}
}

// NewsFingerprint is the read shape consumed by the gating layer.
type NewsFingerprint struct {
	EventSlug               string
	Fingerprint             string
	AnnotationCount         int32
	LatestAnnotationAt      time.Time
	FirstSeenAt             time.Time
	LastSeenAt              time.Time
	ChangedAt               time.Time
	LastSeenUnchangedAt     time.Time
	LastAICalledAt          time.Time
	LastSemanticFingerprint string
	LastSemanticAt          time.Time
	LastSemanticCode        string
	RepeatedCount           int32
}

// Get returns the current fingerprint for an event_slug. Missing row
// → (zero, false, nil). Errors are propagated only when the query
// fails for a reason other than no-row.
func (r *NewsFingerprintRepository) Get(ctx context.Context, eventSlug string) (NewsFingerprint, bool, error) {
	row, err := r.q.GetEventNewsFingerprint(ctx, eventSlug)
	if err != nil {
		return NewsFingerprint{}, false, nil // fail-open
	}
	out := NewsFingerprint{
		EventSlug:           row.EventSlug,
		Fingerprint:         row.NewsFingerprint,
		AnnotationCount:     row.AnnotationCount,
		LatestAnnotationAt:  tsTime(row.LatestAnnotationAt),
		FirstSeenAt:         row.FirstSeenAt.Time,
		LastSeenAt:          row.LastSeenAt.Time,
		ChangedAt:           row.ChangedAt.Time,
		LastSeenUnchangedAt: tsTime(row.LastSeenUnchangedAt),
		LastAICalledAt:      tsTime(row.LastAiCalledAt),
		LastSemanticAt:      tsTime(row.LastSemanticAt),
		RepeatedCount:       row.RepeatedCount,
	}
	if row.LastSemanticFingerprint != nil {
		out.LastSemanticFingerprint = *row.LastSemanticFingerprint
	}
	if row.LastSemanticCode != nil {
		out.LastSemanticCode = *row.LastSemanticCode
	}
	return out, true, nil
}

// Upsert writes the latest fingerprint observation. The SQL flips
// changed_at only when the fingerprint string actually moved.
func (r *NewsFingerprintRepository) Upsert(ctx context.Context, in NewsFingerprint) error {
	return r.q.UpsertEventNewsFingerprint(ctx, sqlc.UpsertEventNewsFingerprintParams{
		EventSlug:          in.EventSlug,
		NewsFingerprint:    in.Fingerprint,
		AnnotationCount:    in.AnnotationCount,
		LatestAnnotationAt: tsFromTime(in.LatestAnnotationAt),
	})
}

// TouchAICalled stamps last_ai_called_at = NOW() so the gating layer
// can see when the AI last ran for this event.
func (r *NewsFingerprintRepository) TouchAICalled(ctx context.Context, eventSlug string) error {
	return r.q.TouchEventNewsAICalled(ctx, eventSlug)
}

// UpsertSemantic records the most recent semantic-fingerprint + code
// (e.g. AI_NO_NOTICEABLE_EDGE) for cooldown lookup.
func (r *NewsFingerprintRepository) UpsertSemantic(ctx context.Context, eventSlug, semanticFingerprint, semanticCode string) error {
	return r.q.UpsertSemanticFingerprint(ctx, sqlc.UpsertSemanticFingerprintParams{
		EventSlug:           eventSlug,
		SemanticFingerprint: nullableStr(semanticFingerprint),
		SemanticCode:        nullableStr(semanticCode),
	})
}
