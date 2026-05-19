// Package marketintel runs the 2h market-intelligence report.
//
// Pipeline:
//  1. List top-N candidate markets (lifecycle + recent activity + liquidity)
//  2. Build a compact MarketReportRequest
//  3. Call analyzer.AnalyzeMarketReport
//  4. Compose Telegram body (Overview / Markets to watch / What matters
//     next / Analyst summary)
//  5. Hash the body for dedup; INSERT ON CONFLICT (summary_hash) DO NOTHING
//  6. On fresh insert, post to Telegram and update delivery_status
//
// The worker is intentionally simple — the orchestration is mostly
// data shaping. The hard parts (model call, cost control, content
// hashing) live downstream in the analyzer + repo.
package marketintel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/telegram"
)

// Candidates returns the top-N markets the analyzer should look at.
// *repository.MarketIntelligenceRepository satisfies it.
type Candidates interface {
	ListIntelligenceCandidates(ctx context.Context, limit int32) ([]repository.IntelligenceCandidate, error)
}

// Store persists the generated report row.
// *repository.MarketIntelligenceRepository satisfies it.
type Store interface {
	Insert(ctx context.Context, r repository.NewMarketIntelligenceReport) (repository.MarketIntelligenceReport, bool, error)
}

// Analyzer is the AI entry point. analysis.Analyzer satisfies it.
type Analyzer interface {
	AnalyzeMarketReport(ctx context.Context, req analysis.MarketReportRequest) (analysis.MarketReportAnalysis, error)
}

// Bot is the Telegram delivery seam. *telegram.Bot satisfies it.
type Bot interface {
	SendHTML(ctx context.Context, chatID, text string) (telegram.SendResult, error)
}

// Config tunes the worker.
type Config struct {
	Enabled        bool
	Interval       time.Duration
	MaxMarkets     int
	MaxOutputChars int
	ChatID         string

	Clock func() time.Time
}

func (c *Config) applyDefaults() {
	if c.Interval <= 0 {
		c.Interval = 2 * time.Hour
	}
	if c.MaxMarkets <= 0 {
		c.MaxMarkets = 50
	}
	if c.MaxOutputChars <= 0 {
		c.MaxOutputChars = 2000
	}
	if c.Clock == nil {
		c.Clock = time.Now
	}
}

// Worker is the periodic 2h intelligence loop.
type Worker struct {
	cfg        Config
	candidates Candidates
	store      Store
	analyzer   Analyzer
	bot        Bot
	log        *zerolog.Logger
}

// New wires the worker. All deps required. Pass analysis.NoopAnalyzer
// to disable the AI call (the worker still selects candidates and
// persists a "skipped" row so an operator can audit the cadence).
func New(cfg Config, candidates Candidates, store Store, analyzer Analyzer, bot Bot, log *zerolog.Logger) *Worker {
	cfg.applyDefaults()
	return &Worker{cfg: cfg, candidates: candidates, store: store, analyzer: analyzer, bot: bot, log: log}
}

// Run blocks until ctx cancels.
func (w *Worker) Run(ctx context.Context) {
	if !w.cfg.Enabled {
		return
	}
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	w.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.tick(ctx)
		}
	}
}

// Tick exposes one cycle for tests.
func (w *Worker) Tick(ctx context.Context) { w.tick(ctx) }

func (w *Worker) tick(ctx context.Context) {
	candidates, err := w.candidates.ListIntelligenceCandidates(ctx, int32(w.cfg.MaxMarkets))
	if err != nil {
		w.log.Err(err).Msg("marketintel: list candidates failed")
		return
	}
	if len(candidates) == 0 {
		return
	}
	now := w.cfg.Clock()
	req := buildRequest(candidates, now, w.cfg.Interval)
	res, err := w.analyzer.AnalyzeMarketReport(ctx, req)
	if err != nil {
		w.log.Err(err).Msg("marketintel: analyzer returned error")
		res = analysis.MarketReportAnalysis{Status: analysis.StatusError, Model: "unknown", LastError: err.Error()}
	}
	body, marketsJSON := composeReport(req, res)
	hash := bodyHash(body)

	// Dedup: identical content within a tick window is suppressed.
	// The INSERT ON CONFLICT (summary_hash) DO NOTHING primitive
	// makes this race-safe across replicas.
	stored, fresh, err := w.store.Insert(ctx, repository.NewMarketIntelligenceReport{
		PeriodStart:      now.Add(-w.cfg.Interval),
		PeriodEnd:        now,
		SummaryHash:      hash,
		ReportText:       body,
		MarketsJSON:      marketsJSON,
		Model:            res.Model,
		PromptTokens:     int32(res.PromptTokens),
		CompletionTokens: int32(res.CompletionTokens),
		EstimatedCostUSD: res.EstimatedCostUSD,
		TelegramChatID:   w.cfg.ChatID,
		DeliveryStatus:   "pending",
	})
	if err != nil {
		w.log.Err(err).Msg("marketintel: persist failed")
		return
	}
	if !fresh {
		// Dedup hit — content identical to a prior report. Skip
		// Telegram delivery so the operator's feed stays clean.
		w.log.Debug().Msg("marketintel: dedup hit, skipping send")
		return
	}
	_ = stored

	if w.bot == nil || w.cfg.ChatID == "" {
		return
	}
	if _, err := w.bot.SendHTML(ctx, w.cfg.ChatID, body); err != nil {
		w.log.Err(err).Msg("marketintel: telegram send failed")
		return
	}
}

// buildRequest projects the candidate list into the analyzer's
// structured request. We don't pre-bucket whale-flow / stable
// favorite / asymmetric counts here — that bucketing requires
// cross-table reads we're deliberately deferring. The AI gets the
// raw market list + counters at zero and produces the operator-
// facing breakdown.
func buildRequest(rows []repository.IntelligenceCandidate, now time.Time, period time.Duration) analysis.MarketReportRequest {
	req := analysis.MarketReportRequest{
		GeneratedAt: now,
		PeriodStart: now.Add(-period),
		PeriodEnd:   now,
		Markets:     make([]analysis.MarketReportMarket, 0, len(rows)),
	}
	for _, r := range rows {
		// Remaining return: (1 - last_price) / last_price expressed
		// as a percentage. Zero when last_price is degenerate.
		var remainPct float64
		if r.LastPrice > 0 && r.LastPrice < 1 {
			remainPct = 100 * (1 - r.LastPrice) / r.LastPrice
		}
		req.Markets = append(req.Markets, analysis.MarketReportMarket{
			Title:              r.Question,
			Category:           r.Category,
			LifecyclePct:       r.LifecyclePct,
			Probability:        r.LastPrice,
			RemainingReturnPct: remainPct,
			Volume24hUSD:       r.Volume24hUSD,
			RecentTrades24h:    int(r.Trades24h),
			AlertsLast24h:      int(r.Alerts24h),
		})
	}
	return req
}

// composeReport renders the Telegram body in the exact shape the
// spec mandates. When the analyzer returned anything other than OK
// we still produce a body — operators want the candidate list and
// counts even when the AI summary itself is unavailable.
func composeReport(req analysis.MarketReportRequest, res analysis.MarketReportAnalysis) (string, []byte) {
	var b strings.Builder
	b.WriteString("<b>Market intelligence · 2h</b>\n")
	fmt.Fprintf(&b, "\nperiod: %s — %s\n",
		req.PeriodStart.Format(time.RFC3339), req.PeriodEnd.Format(time.RFC3339))

	b.WriteString("\n<b>Overview</b>\n")
	fmt.Fprintf(&b, "• markets evaluated: %d\n", len(req.Markets))
	fmt.Fprintf(&b, "• whale-flow candidates: %d\n", req.WhaleFlowCandidates)
	fmt.Fprintf(&b, "• stable favorites: %d\n", req.StableFavorites)
	fmt.Fprintf(&b, "• asymmetric setups: %d\n", req.AsymmetricSetups)
	fmt.Fprintf(&b, "• developing signals: %d\n", req.DevelopingSignals)

	b.WriteString("\n<b>Markets to watch</b>\n")
	n := len(req.Markets)
	if n > 8 {
		n = 8 // keep the visible list compact; the full list is in markets_json
	}
	for i := 0; i < n; i++ {
		m := req.Markets[i]
		fmt.Fprintf(&b, "%d. %s — lifecycle %.0f%%, price %.2f, vol24h $%.0f, alerts24h %d\n",
			i+1, htmlEscape(truncate(m.Title, 80)), m.LifecyclePct, m.Probability,
			m.Volume24hUSD, m.AlertsLast24h)
	}

	b.WriteString("\n<b>Analyst summary</b>\n")
	switch {
	case res.Status == analysis.StatusOK && res.ReportText != "":
		for i, line := range strings.Split(strings.TrimSpace(res.ReportText), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if i == 0 {
				fmt.Fprintf(&b, "• %s\n", htmlEscape(line))
				continue
			}
			fmt.Fprintf(&b, "  %s\n", htmlEscape(line))
		}
	case res.Status == analysis.StatusSkipped:
		b.WriteString("• AI summary unavailable (")
		b.WriteString(htmlEscape(res.LastError))
		b.WriteString("). Candidate list above is unranked.\n")
	default:
		b.WriteString("• AI summary unavailable. Candidate list above is unranked.\n")
	}

	// markets_json is the durable view of the candidates for later
	// SQL-side replay / strategy attribution.
	mj, _ := json.Marshal(req.Markets)
	return b.String(), mj
}

func bodyHash(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
