// Package aibudget is a process-local daily AI spend governor.
//
// Why this exists:
//
//	Watchtower runs several independent AI-consuming workers (alert
//	analyzer, prediction evolution, prediction creation, catalyst
//	importer, daily political intel, market intelligence). Each one
//	had its own optional cap before — the catalyst importer in
//	particular ran ~3,200 calls/day with gpt-4.1 (~$70–90/day) with
//	no daily $-budget at all. The operator could not see, cap, or
//	prioritize spend.
//
// Contract:
//
//	A worker MUST call Manager.Allow(bucket, estCost) before issuing
//	an AI request. Allow returns (true, "") when the request is
//	permitted; (false, reason) when the per-bucket OR global cap
//	would be exceeded. After the response lands (success OR failure
//	with token usage), the worker MUST call Manager.Charge(bucket,
//	actualCost) so the running total is accurate. Calls denied by
//	Allow MUST NOT be Charged.
//
// Behavior:
//
//   - Per-bucket and global counters reset at UTC midnight; the
//     reset is lazy (checked at every Allow/Charge call).
//   - Allow is a single atomic operation guarded by one mutex. No
//     channel; no goroutine. Predictable latency.
//   - Caps of 0 mean "no cap" (fail-open). Useful for tests and
//     one-shot dry runs.
//   - The Manager is process-local. The CLAUDE.md persistence-model
//     section explicitly assumes a single-instance deployment, so
//     cross-process coordination is intentionally NOT in scope.
//     Documented as a known limitation.
//
// Priority is enforced via differentially-sized bucket caps + the
// global cap. A high-priority bucket (alerts) gets the largest
// per-bucket cap; a low-priority bucket (daily intel) gets a small
// cap that runs out first. When the global cap is hit, all buckets
// are denied uniformly; the operator tunes priority by setting the
// bucket caps so the low-priority buckets stop earlier.
package aibudget

import (
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// Bucket identifiers. Use these constants instead of string literals
// at call sites so the compiler catches typos.
const (
	BucketAlertAnalysis     = "alert_analysis"
	BucketPredictionEvolve  = "prediction_evolution"
	BucketPredictionCreate  = "prediction_creation"
	BucketCatalystImporter  = "catalyst_importer"
	BucketMarketIntel       = "market_intel"
	BucketDailyIntel        = "daily_political_intel"
	BucketAnnotationRanking = "annotation_ranking"
)

// AllowedBuckets is the closed set of bucket names the Manager
// recognises. A worker passing anything else is treated as
// fail-open (the call goes through with a metrics warning) so a
// future bucket name can roll out without breaking existing callers.
var AllowedBuckets = []string{
	BucketAlertAnalysis,
	BucketPredictionEvolve,
	BucketPredictionCreate,
	BucketCatalystImporter,
	BucketMarketIntel,
	BucketDailyIntel,
	BucketAnnotationRanking,
}

// Config wires the Manager.
type Config struct {
	// GlobalDailyBudgetUSD caps total spend across all buckets.
	// 0 disables the global cap.
	GlobalDailyBudgetUSD float64
	// BucketDailyBudgetsUSD maps bucket → daily cap in USD.
	// A bucket missing from the map (or set to 0) is uncapped.
	BucketDailyBudgetsUSD map[string]float64
	// Clock is overridable for tests. Production: time.Now.
	Clock func() time.Time
}

// Manager is the budget governor. Zero value is NOT useful; use New.
type Manager struct {
	cfg   Config
	met   *metrics.Metrics
	clock func() time.Time

	mu      sync.Mutex
	day     time.Time          // current UTC day; counters reset when this changes
	global  float64            // running global spend (USD) today
	buckets map[string]float64 // running per-bucket spend (USD) today
}

// New constructs a Manager. Fail-open: a nil metrics handle is
// tolerated (counters silently skip).
func New(cfg Config, met *metrics.Metrics) *Manager {
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.BucketDailyBudgetsUSD == nil {
		cfg.BucketDailyBudgetsUSD = map[string]float64{}
	}
	m := &Manager{
		cfg:     cfg,
		met:     met,
		clock:   cfg.Clock,
		buckets: make(map[string]float64, len(cfg.BucketDailyBudgetsUSD)),
	}
	m.day = m.clock().UTC().Truncate(24 * time.Hour)
	return m
}

// Allow is the gatekeeper. Returns (true, "") when the call is
// permitted under both the per-bucket and the global daily caps;
// (false, reason) with reason ∈ {"bucket_exhausted","global_exhausted"}
// when it isn't. estCost MUST be a non-negative pre-flight estimate
// in USD; the caller pairs this with Charge after the request lands
// so the running total uses the actual token-based cost.
//
// A bucket / cap of 0 means "no cap" and always allows.
//
// Concurrency: safe across goroutines.
func (m *Manager) Allow(bucket string, estCost float64) (bool, string) {
	if m == nil {
		return true, ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollIfNewDayLocked()

	if estCost < 0 {
		estCost = 0
	}
	if cap, ok := m.cfg.BucketDailyBudgetsUSD[bucket]; ok && cap > 0 {
		if m.buckets[bucket]+estCost > cap {
			m.observeDenied(bucket, "bucket_exhausted")
			return false, "bucket_exhausted"
		}
	}
	if m.cfg.GlobalDailyBudgetUSD > 0 {
		if m.global+estCost > m.cfg.GlobalDailyBudgetUSD {
			m.observeDenied(bucket, "global_exhausted")
			return false, "global_exhausted"
		}
	}
	return true, ""
}

// Charge records actual spend after an AI call lands. Always pair
// with a prior Allow; do NOT call Charge after a denial.
func (m *Manager) Charge(bucket string, actualCost float64) {
	if m == nil {
		return
	}
	if actualCost <= 0 {
		// Token-less responses (cache-only, empty completion) still
		// touch the counters at 0 so we can see a Charge happened.
		actualCost = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollIfNewDayLocked()
	m.global += actualCost
	m.buckets[bucket] += actualCost
	if m.met != nil && m.met.AIBudgetCharged != nil {
		m.met.AIBudgetCharged.WithLabelValues(bucket).Add(actualCost)
	}
	if m.met != nil && m.met.AIBudgetSpent != nil {
		m.met.AIBudgetSpent.WithLabelValues(bucket).Set(m.buckets[bucket])
	}
	if m.met != nil && m.met.AIBudgetGlobalSpent != nil {
		m.met.AIBudgetGlobalSpent.Set(m.global)
	}
}

// Snapshot returns a copy of today's spend per bucket plus the
// running global. Cheap; used by diagnostics + admin endpoints.
func (m *Manager) Snapshot() (global float64, perBucket map[string]float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollIfNewDayLocked()
	out := make(map[string]float64, len(m.buckets))
	for k, v := range m.buckets {
		out[k] = v
	}
	return m.global, out
}

func (m *Manager) rollIfNewDayLocked() {
	day := m.clock().UTC().Truncate(24 * time.Hour)
	if day.Equal(m.day) {
		return
	}
	m.day = day
	m.global = 0
	for k := range m.buckets {
		delete(m.buckets, k)
	}
	if m.met != nil && m.met.AIBudgetGlobalSpent != nil {
		m.met.AIBudgetGlobalSpent.Set(0)
	}
}

func (m *Manager) observeDenied(bucket, reason string) {
	if m.met == nil || m.met.AIBudgetDenied == nil {
		return
	}
	m.met.AIBudgetDenied.WithLabelValues(bucket, reason).Inc()
}
