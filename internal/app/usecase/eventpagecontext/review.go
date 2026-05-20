package eventpagecontext

import (
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// --- PART 8 — Event Page Review worker (deferred) -----------------------
//
// This file scaffolds the Event Page Review worker (PART 8 of the
// v9.5 spec) and the Related-Market Lag Detector (PART 9). Neither
// is wired into app.go yet; they require a new alert kind
// ("event_page_annotation") + a dedup table + a Telegram render path,
// and the underlying data (annotations + per-market pricing) only
// just landed.
//
// What is here today:
//   - ReviewConfig + LagDetectorConfig so operators see the env
//     surface and can plan tuning;
//   - LagCandidate type so future code can pass results without
//     ad-hoc tuples.
//
// What is NOT here today:
//   - the periodic Worker.Run loop;
//   - the new alert kind / dedup;
//   - the Telegram formatter;
//   - the lag-detector evaluation.
//
// The deferral is intentional. Per the spec: "Do not overengineer."
// Adding a worker that emits alerts before the annotation feed has
// any operator review would create noise without payoff. The
// scaffolding here makes the surface obvious without committing to
// behaviour we haven't validated against live data.

// ReviewConfig — env knobs for the future review worker.
type ReviewConfig struct {
	Enabled           bool
	Interval          time.Duration
	CategoryWhitelist []string
	AlertsEnabled     bool
	MinAbsPriceChange float64 // e.g. 0.10 = 10 percentage points
	Cooldown          time.Duration
}

// LagDetectorConfig — env knobs for the future related-market lag
// detector. The detector observes large annotation-driven moves on
// one outcome and flags peer markets in the same event that have
// NOT repriced.
type LagDetectorConfig struct {
	Enabled           bool
	MinAnnotationMove float64 // absolute, e.g. 0.15
	MaxRelatedMove    float64 // absolute cap on related-market move
}

// LagCandidate is one related-market lag flag. The detector emits
// this struct; the AI prompt path consumes it as additional context.
type LagCandidate struct {
	EventSlug      string
	TriggerMarket  repository.EventPageMarketRow
	LaggingMarket  repository.EventPageMarketRow
	AnnotationHash string
	TriggerMove    float64
	RelatedMove    float64
}
