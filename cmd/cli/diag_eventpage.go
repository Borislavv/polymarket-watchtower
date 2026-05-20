// diag_eventpage.go — one-off operator diagnostic.
//
// Prints structural facts about a Polymarket event page payload: buildId,
// annotation count, market count, and the first N annotations rendered as
// JSON so an operator can confirm priceBefore / priceAfter / outcome /
// summary all populate. Read-only; no DB writes; no AI call.
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

	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/eventpage"
)

func runDiagEventPage(args []string) {
	fs := flag.NewFlagSet("diag-eventpage", flag.ExitOnError)
	eventSlug := fs.String("event", "", "Polymarket event slug (required)")
	htmlBase := fs.String("html-base", "https://polymarket.com", "Polymarket HTML base URL")
	sampleN := fs.Int("sample", 3, "number of sample annotations to print")
	_ = fs.Parse(args)

	if strings.TrimSpace(*eventSlug) == "" {
		fmt.Fprintln(os.Stderr, "diag-eventpage: --event <slug> is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	logger := zerolog.New(os.Stderr).With().Timestamp().Logger()

	resolver := eventpage.NewBuildIDResolver(eventpage.BuildIDResolverConfig{
		HTMLBaseURL: *htmlBase, TTL: 30 * time.Minute, Logger: &logger,
	})
	cli, err := eventpage.NewClient(eventpage.ClientConfig{
		HTMLBaseURL: *htmlBase, Resolver: resolver, Logger: &logger,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "diag-eventpage:", err)
		os.Exit(1)
	}
	pl, err := cli.FetchEventPage(ctx, *eventSlug)
	if err != nil {
		fmt.Fprintln(os.Stderr, "diag-eventpage:", err)
		os.Exit(1)
	}
	fmt.Printf("event_slug:        %s\n", pl.EventSlug)
	fmt.Printf("build_id:          %s\n", pl.BuildID)
	fmt.Printf("event_title:       %s\n", pl.Event.Title)
	fmt.Printf("event_category:    %s\n", pl.Event.Category)
	fmt.Printf("event_active:      %v\n", pl.Event.Active)
	fmt.Printf("event_closed:      %v\n", pl.Event.Closed)
	fmt.Printf("event_start:       %s\n", pl.Event.StartDate.Format(time.RFC3339))
	fmt.Printf("event_end:         %s\n", pl.Event.EndDate.Format(time.RFC3339))
	fmt.Printf("annotations:       %d\n", len(pl.Annotations))
	fmt.Printf("markets:           %d\n", len(pl.Markets))
	fmt.Printf("similar_markets:   %d\n", len(pl.SimilarMarkets))
	fmt.Printf("series:            %d\n", len(pl.Series))
	fmt.Printf("tags:              %d\n", len(pl.Tags))
	fmt.Printf("query_keys:        %d\n", len(pl.RawQueryKeys))
	if n := *sampleN; n > 0 && len(pl.Annotations) > 0 {
		if n > len(pl.Annotations) {
			n = len(pl.Annotations)
		}
		fmt.Printf("\n--- first %d annotations ---\n", n)
		for i := 0; i < n; i++ {
			out, _ := json.MarshalIndent(pl.Annotations[i], "", "  ")
			fmt.Println(string(out))
		}
	}
	if len(pl.Markets) > 0 {
		fmt.Printf("\n--- first market ---\n")
		out, _ := json.MarshalIndent(pl.Markets[0], "", "  ")
		fmt.Println(string(out))
	}
}
