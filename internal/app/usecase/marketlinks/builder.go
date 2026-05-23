// Package marketlinks builds the market ↔ market graph used by
// the thesis-accumulation detector.
//
// The package is intentionally pure-orchestration: an EventsLister
// hands us batches of (event_slug, market_set, link hints) and a
// LinkSink upserts the produced edges. The production wiring puts
// Gamma-event reads behind EventsLister and the
// polymarket_market_links repository behind LinkSink. Tests inject
// fakes for both seams.
//
// Failure semantics: per-event errors are logged + metric'd; the
// cycle continues. A panic in handleEvent is recovered so one
// bad event never aborts the batch. Builder NEVER touches Telegram,
// AI, or the realtime path.
package marketlinks

import (
	"context"
	"sync"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// LinkHint is the per-event input the builder turns into edges.
// SourceConditionID = "anchor" market; Targets = same-event markets
// that should link back to it. Direction is "aligned" for same-side
// outcomes within the event, "opposed" for explicit mirror outcomes,
// "unknown" otherwise.
type LinkHint struct {
	EventSlug         string
	SeriesID          string
	SourceConditionID string
	Targets           []LinkTarget
}

type LinkTarget struct {
	ConditionID string
	LinkType    string // "same_event" | "same_series" | "same_tag" | "manual"
	Direction   string // "aligned" | "opposed" | "unknown"
	Confidence  float64
}

// EventsLister returns batches of LinkHint. Production reads from
// the Gamma events repository; tests can stub.
type EventsLister interface {
	ListLinkHints(ctx context.Context, batchSize int) ([]LinkHint, error)
}

// LinkSink upserts one edge. Production wraps the sqlc query
// UpsertMarketLink.
type LinkSink interface {
	UpsertMarketLink(ctx context.Context, edge Edge) error
}

// Edge is the persisted link row. EventSlug + SeriesID are
// denormalised for index locality.
type Edge struct {
	SrcConditionID string
	DstConditionID string
	LinkType       string
	Direction      string
	Confidence     float64
	EventSlug      string
	SeriesID       string
	LinkVersion    int
}

// Logger is a tiny interface so the builder doesn't import zerolog
// directly — keeps unit tests dependency-free.
type Logger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, err error, kv ...any)
}

// noopLogger is the default; production wires zerolog.
type noopLogger struct{}

func (noopLogger) Info(string, ...any)         {}
func (noopLogger) Warn(string, ...any)         {}
func (noopLogger) Error(string, error, ...any) {}

// Builder is the v11.5 marketlinks worker.
type Builder struct {
	cfg    Config
	events EventsLister
	sink   LinkSink
	met    *metrics.Metrics
	log    Logger
	clock  func() time.Time
	mu     sync.Mutex
}

// Config controls the cycle.
type Config struct {
	Enabled        bool
	Interval       time.Duration
	BatchSize      int
	LinkVersion    int
	IncludeOpposed bool
	MinConfidence  float64
}

// New constructs a Builder. nil dependencies are tolerated by
// every code path so app boot can wire production deps lazily.
func New(cfg Config, events EventsLister, sink LinkSink, met *metrics.Metrics, log Logger) *Builder {
	if log == nil {
		log = noopLogger{}
	}
	return &Builder{
		cfg:    cfg,
		events: events,
		sink:   sink,
		met:    met,
		log:    log,
		clock:  time.Now,
	}
}

// WithClock overrides the wall clock; tests use this for
// determinism.
func (b *Builder) WithClock(fn func() time.Time) *Builder {
	if fn != nil {
		b.clock = fn
	}
	return b
}

// Run drives the ticker until ctx cancels. No immediate tick —
// the builder is the lowest-priority v11.5 worker; the first
// cycle waits one Interval to let app boot settle.
func (b *Builder) Run(ctx context.Context) {
	if !b.cfg.Enabled {
		return
	}
	t := time.NewTicker(b.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			b.Tick(ctx)
		}
	}
}

// Tick exposes one cycle for tests.
func (b *Builder) Tick(ctx context.Context) {
	start := b.clock()
	defer func() {
		if r := recover(); r != nil {
			b.observeRun("panic")
			b.log.Error("marketlinks: panic", nil, "panic", r)
		}
		b.observeLatency(time.Since(start))
	}()

	if b.events == nil || b.sink == nil {
		b.observeRun("skipped")
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	hints, err := b.events.ListLinkHints(ctx, b.cfg.BatchSize)
	if err != nil {
		b.observeRun("failed")
		b.log.Error("marketlinks: list hints failed", err)
		return
	}
	if len(hints) == 0 {
		b.observeRun("empty")
		return
	}

	var persisted, skipped, errored int
	for _, h := range hints {
		for _, tgt := range h.Targets {
			if tgt.ConditionID == "" || h.SourceConditionID == "" || tgt.ConditionID == h.SourceConditionID {
				skipped++
				continue
			}
			if tgt.Confidence < b.cfg.MinConfidence {
				skipped++
				continue
			}
			if tgt.Direction == "opposed" && !b.cfg.IncludeOpposed {
				skipped++
				continue
			}
			edge := Edge{
				SrcConditionID: h.SourceConditionID,
				DstConditionID: tgt.ConditionID,
				LinkType:       tgt.LinkType,
				Direction:      normaliseDirection(tgt.Direction),
				Confidence:     tgt.Confidence,
				EventSlug:      h.EventSlug,
				SeriesID:       h.SeriesID,
				LinkVersion:    b.cfg.LinkVersion,
			}
			if err := b.sink.UpsertMarketLink(ctx, edge); err != nil {
				errored++
				b.log.Warn("marketlinks: upsert failed",
					"src", edge.SrcConditionID, "dst", edge.DstConditionID, "err", err)
				continue
			}
			persisted++
		}
	}
	b.observeItems(persisted, skipped, errored)
	b.observeRun("ok")
}

func normaliseDirection(d string) string {
	switch d {
	case "aligned", "opposed":
		return d
	default:
		return "unknown"
	}
}

func (b *Builder) observeRun(status string) {
	if b.met == nil || b.met.StrategyWorkerRuns == nil {
		return
	}
	b.met.StrategyWorkerRuns.WithLabelValues("marketlinks", status).Inc()
}

func (b *Builder) observeItems(persisted, skipped, errored int) {
	if b.met == nil || b.met.StrategyWorkerItems == nil {
		return
	}
	c := b.met.StrategyWorkerItems
	if persisted > 0 {
		c.WithLabelValues("marketlinks", "persisted").Add(float64(persisted))
	}
	if skipped > 0 {
		c.WithLabelValues("marketlinks", "skipped").Add(float64(skipped))
	}
	if errored > 0 {
		c.WithLabelValues("marketlinks", "errored").Add(float64(errored))
	}
}

func (b *Builder) observeLatency(d time.Duration) {
	if b.met == nil || b.met.StrategyWorkerLatency == nil {
		return
	}
	b.met.StrategyWorkerLatency.WithLabelValues("marketlinks").Observe(d.Seconds())
}
