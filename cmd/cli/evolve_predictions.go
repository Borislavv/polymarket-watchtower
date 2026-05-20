// evolve_predictions.go — one-shot Prediction Evolution dry-run.
//
// Connects to Postgres, lists predictions due for evolution, and
// per-prediction prints: id / event_slug / old state / new state /
// AI refreshed yes/no / repricing status / strongest flow side /
// matched alerts count / decay applied yes/no / Telegram yes/no.
//
// `--dry-run` (default true) skips ALL DB writes — the deterministic
// layers run end-to-end against live tables but the worker's
// Apply / Touch / Decay paths short-circuit. Use without
// `--dry-run` to run a real cycle (worker writes are persisted).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventflow"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/eventpagecontext"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/marketprediction/evolution"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/repricing"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/eventpage"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres/sqlc"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

func runEvolvePredictions(args []string) {
	fs := flag.NewFlagSet("evolve-predictions", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("POSTGRES_DSN"), "Postgres DSN")
	once := fs.Bool("once", true, "run exactly one cycle and exit")
	limit := fs.Int("limit", 10, "BatchSize override")
	dryRun := fs.Bool("dry-run", true, "skip DB writes")
	_ = fs.Parse(args)
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "evolve-predictions: --dsn or $POSTGRES_DSN required")
		os.Exit(2)
	}
	_ = *once // we always do one tick — kept for future scheduled mode

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	logger := zerolog.New(os.Stderr).With().Timestamp().Logger()

	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "evolve-predictions: open pool:", err)
		os.Exit(1)
	}
	defer pool.Close()

	met := metrics.New()
	predsRepo := repository.NewRepricingPredictionsRepository(pool)
	marketsRepo := repository.NewMarketRepository(pool)
	resolver := eventpage.NewBuildIDResolver(eventpage.BuildIDResolverConfig{
		HTMLBaseURL: "https://polymarket.com", TTL: 30 * time.Minute, Logger: &logger,
	})
	epClient, err := eventpage.NewClient(eventpage.ClientConfig{
		HTMLBaseURL: "https://polymarket.com", Resolver: resolver, Logger: &logger,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "evolve-predictions: event page client:", err)
		os.Exit(1)
	}
	// Slug resolver via marketsRepo.
	slugResolver := func(ctx context.Context, conditionID string) string {
		if conditionID == "" {
			return ""
		}
		m, err := marketsRepo.GetByConditionID(ctx, conditionID)
		if err != nil {
			return ""
		}
		return m.EventSlug
	}
	eventPageProvider := eventpagecontext.New(eventpagecontext.Config{
		Enabled:          true,
		RefreshInfo:      10 * time.Minute,
		RefreshImportant: 5 * time.Minute,
		PromptMaxItems:   25,
		PromptMaxChars:   5000,
		FetchTimeout:     8 * time.Second,
	}, epClient, repository.NewEventPageRepository(pool), slugResolver, met, &logger)
	flowRepo := eventflow.New(sqlc.New(pool), eventflow.Config{
		Enabled: true, Lookback: 24 * time.Hour, TopItems: 10,
	}, met, &logger)
	repricingComp := repricing.New(repricing.Config{Enabled: true}, sqlc.New(pool), predsRepo, met, &logger)
	catalystRepo := repository.NewEventCatalystRepository(pool)
	w := evolution.New(evolution.Config{
		Enabled:      true,
		BatchSize:    *limit,
		Concurrency:  2,
		Timeout:      60 * time.Second,
		AIEnabled:    false, // dry-run: never call AI
		DecayEnabled: !*dryRun,
		SendTelegram: false,
	}, predsRepo, eventPageProvider, catalystRepo, flowRepo, repricingComp, analysis.NoopPredictionEvolutionGenerator{}, nil, met, &logger)

	// Pull predictions and process one-by-one so we can print per-row.
	rows, err := predsRepo.ListPredictionsForEvolution(ctx, time.Now().Add(-1*time.Minute), int32(*limit))
	if err != nil {
		fmt.Fprintln(os.Stderr, "evolve-predictions: list:", err)
		os.Exit(1)
	}
	if len(rows) == 0 {
		fmt.Println("no predictions due for evolution")
		return
	}
	fmt.Printf("%-6s  %-32s  %-20s -> %-20s  %3s  %-16s  %-8s  %4s  %-6s  %-3s\n",
		"id", "event_slug", "old_state", "new_state", "AI", "repricing", "side", "match", "decay", "tg")
	for _, p := range rows {
		res := w.TickOne(ctx, p, *dryRun)
		ai := "no"
		if res.AIRefreshed {
			ai = "yes"
		} else if res.AISkipReason != "" {
			ai = "skp"
		}
		side := res.StrongestSide
		if side == "" {
			side = "—"
		}
		decay := "no"
		if res.DecayApplied {
			decay = "yes"
		}
		tg := "no"
		if res.TelegramSent {
			tg = "yes"
		}
		fmt.Printf("%-6d  %-32s  %-20s -> %-20s  %3s  %-16s  %-8s  %4d  %-6s  %-3s\n",
			res.PredictionID, truncateRune(res.EventSlug, 32),
			res.OldState, res.NewState, ai, truncateRune(res.RepricingStatus, 16),
			truncateRune(side, 8), res.MatchedAlerts, decay, tg)
	}
}

func truncateRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
