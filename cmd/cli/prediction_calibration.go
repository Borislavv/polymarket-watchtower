// prediction_calibration.go — one-shot v10.3 operator calibration
// report. Aggregates polymarket_prediction_evaluations +
// polymarket_market_predictions + polymarket_prediction_usefulness_scores
// for the configured lookback and prints a deterministic text report.
//
// Read-only; no AI; no Telegram. The same aggregation backs the
// daily calibration report worker (PART 7).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type evalRow struct {
	predictionID int64
	horizon      string
	evaluation   string
	score        float64
	createdAt    time.Time
	eventSlug    string
	sideBias     string
	state        string
	confidence   float64
}

func runPredictionCalibration(args []string) {
	fs := flag.NewFlagSet("prediction-calibration", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("POSTGRES_DSN"), "Postgres DSN")
	lookback := fs.Duration("lookback", 7*24*time.Hour, "lookback window for the report")
	minHorizon := fs.String("min-horizon", "6h", "ignore evaluations whose horizon is shorter than this")
	limit := fs.Int("limit", 500, "max evaluation rows to pull")
	_ = fs.Parse(args)
	if *dsn == "" {
		fmt.Fprintln(os.Stderr, "prediction-calibration: --dsn or $POSTGRES_DSN required")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, *dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "prediction-calibration:", err)
		os.Exit(1)
	}
	defer pool.Close()

	since := time.Now().Add(-*lookback)
	rows, err := pool.Query(ctx, `
		SELECT e.prediction_id, e.horizon, e.evaluation, e.score, e.created_at,
		       p.event_slug, p.side_bias, p.current_state, p.confidence
		FROM polymarket_prediction_evaluations e
		JOIN polymarket_market_predictions p ON p.id = e.prediction_id
		WHERE e.created_at >= $1
		ORDER BY e.created_at DESC
		LIMIT $2
	`, since, *limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "prediction-calibration query:", err)
		os.Exit(1)
	}
	defer rows.Close()

	var evals []evalRow
	for rows.Next() {
		var r evalRow
		if err := rows.Scan(&r.predictionID, &r.horizon, &r.evaluation, &r.score, &r.createdAt,
			&r.eventSlug, &r.sideBias, &r.state, &r.confidence); err != nil {
			fmt.Fprintln(os.Stderr, "prediction-calibration scan:", err)
			continue
		}
		if !horizonAtLeast(r.horizon, *minHorizon) {
			continue
		}
		evals = append(evals, r)
	}

	// Aggregate.
	classCounts := map[string]int{}
	stateCounts := map[string]int{}
	sideCounts := map[string]int{}
	usefulnessBuckets := map[string]int{}
	var totalDirectionCorrect, totalDirectionDecidable int
	for _, e := range evals {
		classCounts[e.evaluation]++
		stateCounts[e.state]++
		sideCounts[e.sideBias]++
		usefulnessBuckets[bucketUsefulness(e.score)]++
		switch e.evaluation {
		case "useful_correct", "useful_early", "correct_but_late":
			totalDirectionDecidable++
			totalDirectionCorrect++
		case "wrong_direction":
			totalDirectionDecidable++
		}
	}

	totalCount, err := scanInt(ctx, pool, `
		SELECT COUNT(*) FROM polymarket_market_predictions WHERE created_at >= $1
	`, since)
	if err != nil {
		fmt.Fprintln(os.Stderr, "prediction-calibration total:", err)
	}
	activeCount, _ := scanInt(ctx, pool, `
		SELECT COUNT(*) FROM polymarket_market_predictions
		WHERE current_state NOT IN ('resolved','invalidated','stale')
		  AND archived_at IS NULL
	`)

	// Report.
	fmt.Println("============================================================")
	fmt.Printf(" Prediction Calibration Report — last %s\n", *lookback)
	fmt.Println("============================================================")
	fmt.Printf("Predictions created (lookback):  %d\n", totalCount)
	fmt.Printf("Currently active (not archived): %d\n", activeCount)
	fmt.Printf("Evaluations sampled (horizon ≥ %s): %d\n", *minHorizon, len(evals))
	if totalDirectionDecidable > 0 {
		rate := float64(totalDirectionCorrect) / float64(totalDirectionDecidable)
		fmt.Printf("Direction correctness rate:      %.1f%% (%d/%d)\n",
			rate*100, totalDirectionCorrect, totalDirectionDecidable)
	}

	fmt.Println("\nEvaluation class distribution:")
	printSorted(classCounts)

	fmt.Println("\nState distribution (current):")
	printSorted(stateCounts)

	fmt.Println("\nSide-bias distribution:")
	printSorted(sideCounts)

	fmt.Println("\nUsefulness score bucket distribution (creation-time):")
	for _, b := range []string{"≥0.80 high", "0.60–0.80 actionable", "0.40–0.60 borderline", "<0.40 low"} {
		fmt.Printf("  %-26s %d\n", b, usefulnessBuckets[b])
	}

	fmt.Println("\nNotes:")
	fmt.Println("  • wrong_direction is the load-bearing failure class — tune RequireSignal / MinConfidence if it dominates.")
	fmt.Println("  • already_priced_noise = market moved BEFORE the prediction was created; the catalyst importer / repricing classifier could've caught it.")
	fmt.Println("  • blocked_unresolved = catalyst we expected never arrived; check EVENT_CATALYST_IMPORTER_STALE_AFTER.")
}

// horizonAtLeast reports whether `h` is at-or-greater than `min`,
// where both are "1h", "6h", "24h", "72h" enum values.
func horizonAtLeast(h, min string) bool {
	order := map[string]int{"1h": 1, "6h": 6, "24h": 24, "72h": 72}
	return order[h] >= order[min]
}

func bucketUsefulness(score float64) string {
	switch {
	case score >= 0.80:
		return "≥0.80 high"
	case score >= 0.60:
		return "0.60–0.80 actionable"
	case score >= 0.40:
		return "0.40–0.60 borderline"
	}
	return "<0.40 low"
}

func printSorted(m map[string]int) {
	type kv struct {
		k string
		v int
	}
	var pairs []kv
	for k, v := range m {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].v > pairs[j].v })
	for _, p := range pairs {
		fmt.Printf("  %-32s %d\n", p.k, p.v)
	}
}

func scanInt(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) (int64, error) {
	row := pool.QueryRow(ctx, sql, args...)
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
