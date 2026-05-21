// ws_smoke.go — one-shot Polymarket CLOB WebSocket smoke command.
//
// Purpose: validate the v10.4 hybrid realtime fast-lane against the
// LIVE upstream on 1-3 markets, end-to-end, for a bounded duration.
//
// Default mode is dry-run: connect → subscribe → print observed
// event types → exit. Pass --persist with a --dsn to additionally
// upsert polymarket_live_market_state and insert polymarket_ws_events
// rows through the same realtime worker the daemon uses.
//
// IMPORTANT: this CLI deliberately does NOT call AI, Telegram, or the
// strategy/severity layer. It exercises the infra + persistence path
// only — the v10.4 non-bypass rules MUST hold here too.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/realtime"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/polymarket/ws"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/postgres"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

func runWSSmoke(args []string) {
	fs := flag.NewFlagSet("ws-smoke", flag.ExitOnError)
	tokensArg := fs.String("tokens", "", "comma-separated CLOB token ids to subscribe to (required)")
	conditionArg := fs.String("condition", "", "optional condition_id (for labeling + persist)")
	eventSlug := fs.String("event-slug", "", "optional event_slug (for labeling)")
	outcomeArg := fs.String("outcome", "", "optional outcome label for the first token (e.g. Yes/No)")
	endpoint := fs.String("endpoint", "wss://ws-subscriptions-clob.polymarket.com/ws/market", "WS endpoint (staging override only)")
	duration := fs.Duration("duration", 60*time.Second, "how long to stay subscribed")
	persist := fs.Bool("persist", false, "persist ws_events + live_market_state rows to Postgres")
	dsn := fs.String("dsn", os.Getenv("POSTGRES_DSN"), "Postgres DSN (defaults to $POSTGRES_DSN); required when --persist")
	reconcile := fs.Bool("reconcile", false, "run one gap-recovery audit row for the supplied condition (requires --persist + --condition)")
	rawCap := fs.Bool("raw", false, "capture raw JSON payload bytes in the persisted row (heartbeats only if --persist)")
	_ = fs.Parse(args)

	tokenList := splitAndTrim(*tokensArg)
	if len(tokenList) == 0 {
		fmt.Fprintln(os.Stderr, "ws-smoke: --tokens <id1,id2,…> is required (these are CLOB token ids, NOT condition_ids)")
		os.Exit(2)
	}
	if *persist && strings.TrimSpace(*dsn) == "" {
		fmt.Fprintln(os.Stderr, "ws-smoke: --persist requires --dsn or $POSTGRES_DSN")
		os.Exit(2)
	}
	if *reconcile && (!*persist || strings.TrimSpace(*conditionArg) == "") {
		fmt.Fprintln(os.Stderr, "ws-smoke: --reconcile requires --persist + --condition")
		os.Exit(2)
	}

	logger := zerolog.New(os.Stderr).With().Timestamp().Logger()

	ctx, cancel := context.WithTimeout(context.Background(), *duration+30*time.Second)
	defer cancel()
	// Graceful Ctrl-C.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		cancel()
	}()

	// Build the subscription set from the supplied tokens.
	sub := ws.MarketSubscription{
		EventSlug:      strings.TrimSpace(*eventSlug),
		ConditionID:    strings.TrimSpace(*conditionArg),
		CLOBTokenIDs:   tokenList,
		OutcomeByToken: map[string]string{},
	}
	if *outcomeArg != "" {
		sub.OutcomeByToken[tokenList[0]] = strings.TrimSpace(*outcomeArg)
	}
	set := ws.SubscriptionSet{Markets: []ws.MarketSubscription{sub}}

	// Always run the client in non-persist mode first so we have a
	// stdout printer regardless of --persist. The same Events stream
	// optionally feeds the realtime.Worker for DB writes.
	client := ws.New(ws.Config{
		Endpoint:     *endpoint,
		PingInterval: 10 * time.Second,
		ReadTimeout:  45 * time.Second,
		WriteTimeout: 10 * time.Second,
		EventBuffer:  4096,
		MaxTokens:    16, // smoke ≤ 3 markets ⇒ tiny cap is fine
	}, nil, &logger)
	client.Subscribe(set)

	out := make(chan ws.Event, 4096)
	var wg sync.WaitGroup

	// Optional persistence path.
	var worker *realtime.Worker
	if *persist {
		pool, err := postgres.Open(ctx, postgres.Config{DSN: *dsn})
		if err != nil {
			fmt.Fprintln(os.Stderr, "ws-smoke: postgres open:", err)
			os.Exit(1)
		}
		defer pool.Close()
		repo := repository.NewRealtimeRepository(pool)
		worker = realtime.New(realtime.Config{
			Enabled:                  true,
			SubscriptionMode:         "off", // we provide the subs directly
			RawCaptureEnabled:        *rawCap,
			RawCaptureMaxBytes:       4096,
			PriceMoveTrigger:         0.03,
			RepricingTriggerCooldown: 60 * time.Second,
			Clock:                    time.Now,
		}, repo, nil, nil, nil, &logger)

		if *reconcile {
			id, err := repo.InsertGapRecovery(ctx, sub.ConditionID, time.Now().Add(-10*time.Minute), time.Now())
			if err != nil {
				fmt.Fprintln(os.Stderr, "ws-smoke: insert gap recovery:", err)
			} else {
				_ = repo.FinishGapRecovery(ctx, id, "ok", 0, "")
				fmt.Println("ws-smoke: reconciliation audit row inserted + closed")
			}
		}
	}

	// Counters per type so the operator gets one summary line at the end.
	counts := map[string]int{}
	var countsMu sync.Mutex

	// Drainer goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		deadline := time.NewTimer(*duration)
		defer deadline.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-deadline.C:
				return
			case ev, ok := <-out:
				if !ok {
					return
				}
				countsMu.Lock()
				counts[ev.Type]++
				countsMu.Unlock()
				printEvent(ev)
				if worker != nil {
					// Drive ONLY the persistence + live-state path.
					// Trigger evaluation is fine to run; it writes to
					// polymarket_realtime_work_queue but never to AI/
					// Telegram.
					worker.HandleForSmoke(ctx, ev)
				}
			}
		}
	}()

	// Run the client until the duration elapses or context cancels.
	clientDone := make(chan struct{})
	go func() {
		_ = client.Run(ctx, out)
		close(clientDone)
	}()

	select {
	case <-time.After(*duration):
	case <-clientDone:
	case <-ctx.Done():
	}
	cancel()
	<-clientDone
	close(out)
	wg.Wait()

	// Summary.
	fmt.Println("\n=== ws-smoke summary ===")
	fmt.Printf("endpoint:    %s\n", *endpoint)
	fmt.Printf("tokens:      %d\n", len(tokenList))
	fmt.Printf("duration:    %s\n", duration.String())
	fmt.Printf("persist:     %v\n", *persist)
	fmt.Printf("raw-capture: %v\n", *rawCap)
	fmt.Printf("reconcile:   %v\n", *reconcile)
	fmt.Println("events-by-type:")
	countsMu.Lock()
	for typ, n := range counts {
		fmt.Printf("  %-22s %d\n", typ, n)
	}
	countsMu.Unlock()
}

func printEvent(ev ws.Event) {
	parts := []string{
		"[" + ev.ReceivedAt.UTC().Format(time.RFC3339Nano) + "]",
		fmt.Sprintf("type=%-18s", ev.Type),
	}
	if ev.ConditionID != "" {
		parts = append(parts, "cond="+truncateWS(ev.ConditionID, 14))
	}
	if ev.CLOBTokenID != "" {
		parts = append(parts, "tok="+truncateWS(ev.CLOBTokenID, 12))
	}
	if ev.Price != nil {
		parts = append(parts, fmt.Sprintf("px=%.4f", *ev.Price))
	}
	if ev.Size != nil {
		parts = append(parts, fmt.Sprintf("sz=%.4f", *ev.Size))
	}
	if ev.BestBid != nil && ev.BestAsk != nil {
		parts = append(parts, fmt.Sprintf("bid/ask=%.4f/%.4f", *ev.BestBid, *ev.BestAsk))
	}
	if ev.Side != "" {
		parts = append(parts, "side="+ev.Side)
	}
	if ev.SideSource != "" {
		parts = append(parts, "src="+ev.SideSource)
	}
	fmt.Println(strings.Join(parts, " "))
}

func splitAndTrim(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func truncateWS(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
