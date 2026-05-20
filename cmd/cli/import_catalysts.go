// import_catalysts.go — one-shot Political-Catalyst Intelligence
// smoke command. Fetches a single Polymarket event page and (when
// an OpenAI key is wired) runs the catalyst-extraction prompt
// against it, printing the result to stdout.
//
// This is a DRY-RUN tool by design. Persistence to
// polymarket_event_catalysts happens via the running watchtower
// daemon (EVENT_CATALYST_IMPORTER_ENABLED=true) — the same Upsert
// path the periodic worker uses. Splitting the persist path into a
// CLI would duplicate database wiring without payoff.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/analysis"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/ai/openai"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/eventpage"
)

func runImportCatalysts(args []string) {
	fs := flag.NewFlagSet("import-catalysts", flag.ExitOnError)
	eventSlug := fs.String("event", "", "Polymarket event slug (required)")
	aiKey := fs.String("ai-key", os.Getenv("OPENAI_API_KEY"), "OpenAI API key (defaults to $OPENAI_API_KEY)")
	aiBase := fs.String("ai-base", "https://api.openai.com/v1", "OpenAI base URL")
	aiModel := fs.String("model", "gpt-4.1-mini", "OpenAI model name")
	aiTimeout := fs.Duration("ai-timeout", 45*time.Second, "AI extraction timeout")
	htmlBase := fs.String("html-base", "https://polymarket.com", "Polymarket HTML base URL (staging override only)")
	_ = fs.Parse(args)

	if strings.TrimSpace(*eventSlug) == "" {
		fmt.Fprintln(os.Stderr, "import-catalysts: --event <slug> is required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	logger := zerolog.New(os.Stderr).With().Timestamp().Logger()

	resolver := eventpage.NewBuildIDResolver(eventpage.BuildIDResolverConfig{
		HTMLBaseURL: *htmlBase, TTL: 30 * time.Minute, Logger: &logger,
	})
	client, err := eventpage.NewClient(eventpage.ClientConfig{
		HTMLBaseURL: *htmlBase, Resolver: resolver, Logger: &logger,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "import-catalysts: build event-page client:", err)
		os.Exit(1)
	}
	payload, err := client.FetchEventPage(ctx, *eventSlug)
	if err != nil {
		fmt.Fprintln(os.Stderr, "import-catalysts: fetch event page:", err)
		os.Exit(1)
	}
	fmt.Printf("event_slug: %s\nbuild_id: %s\nannotations: %d\nmarkets: %d\nquery_keys: %v\n",
		payload.EventSlug, payload.BuildID, len(payload.Annotations), len(payload.Markets), payload.RawQueryKeys)

	if strings.TrimSpace(*aiKey) == "" {
		fmt.Println("\nAI: no API key provided — skipping catalyst extraction.")
		fmt.Println("(re-run with --ai-key $OPENAI_API_KEY to exercise the extractor)")
		return
	}

	cli := openai.New(openai.Config{
		APIKey: *aiKey, BaseURL: *aiBase, Model: *aiModel, Timeout: *aiTimeout,
		RatePerMin: 100, DailyBudget: 5,
	})

	req := analysis.CatalystExtractionRequest{
		EventSlug:       payload.EventSlug,
		AnalysisTimeUTC: time.Now().UTC(),
		EventMetadata: analysis.CatalystEventMetadata{
			Title: payload.Event.Title, Description: payload.Event.Description,
			ResolutionRules: payload.Event.ResolutionRules, Category: payload.Event.Category,
			StartDate: payload.Event.StartDate, EndDate: payload.Event.EndDate,
			ContextDescription: payload.Event.ContextDescription, ContextUpdatedAt: payload.Event.ContextUpdatedAt,
		},
	}
	for _, m := range payload.Markets {
		req.Markets = append(req.Markets, analysis.CatalystMarket{
			ConditionID: m.ConditionID, Question: m.Question, GroupItemTitle: m.GroupItemTitle,
			Outcomes: m.Outcomes, OutcomePrices: m.OutcomePrices,
			Volume24hUSD: m.Volume24h, Liquidity: m.Liquidity,
			OneHourPriceChange: m.OneHourPriceChange, OneDayPriceChange: m.OneDayPriceChange,
			OneWeekPriceChange: m.OneWeekPriceChange, LastTradePrice: m.LastTradePrice,
			Active: m.Active, Closed: m.Closed, EndDate: m.EndDate,
		})
	}
	for _, a := range payload.Annotations {
		req.Annotations = append(req.Annotations, analysis.CatalystAnnotation{
			Timestamp: a.Timestamp, Title: a.Title, Summary: a.Summary, Outcome: a.Outcome,
			PriceBefore: a.PriceBefore, PriceAfter: a.PriceAfter, PriceChange: a.PriceChange,
		})
	}

	res, err := cli.ExtractCatalysts(ctx, req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "AI: extract catalysts:", err)
		os.Exit(1)
	}
	fmt.Printf("\nAI status: %s\nmodel: %s\nprompt_tokens: %d\ncompletion_tokens: %d\ncatalysts: %d\n\n",
		res.Status, res.Model, res.PromptTokens, res.CompletionTokens, len(res.Catalysts))
	out, _ := json.MarshalIndent(res.Catalysts, "", "  ")
	fmt.Println(string(out))
	fmt.Println("\nDRY RUN — no DB writes. The running watchtower daemon performs persistence via the periodic importer.")
}
