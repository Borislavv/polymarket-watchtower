package app

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestEnvFiles_StrategyKeysSynchronized pins the v11.9 NON-NEGOTIABLE
// invariant: `.env` and `.env.example` must contain the same set of
// keys for everything strategy-shaped. Drift is a config-safety bug
// because an operator running `cp .env.example .env` would silently
// disable the strategy stack.
func TestEnvFiles_StrategyKeysSynchronized(t *testing.T) {
	envKeys := readEnvKeys(t, repoFile(".env"))
	exampleKeys := readEnvKeys(t, repoFile(".env.example"))

	onlyEnv := diff(envKeys, exampleKeys)
	onlyExample := diff(exampleKeys, envKeys)

	if len(onlyEnv) > 0 || len(onlyExample) > 0 {
		t.Fatalf("env files not synchronized:\n  only in .env: %v\n  only in .env.example: %v",
			onlyEnv, onlyExample)
	}
}

// TestEnvFiles_StrategyV11KeysAllPresent guarantees every v11.5–v11.9
// strategy key the ТЗ documents lives in both files.
func TestEnvFiles_StrategyV11KeysAllPresent(t *testing.T) {
	required := []string{
		// v11.5/v11.6/v11.7 core
		"STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED",
		"STRATEGY_SHADOW_MAX_DECISIONS_PER_TRADE",
		"STRATEGY_SHADOW_RECORD_NOFIRE",
		"STRATEGY_STAGED_INPUTS_ENABLED",
		"STRATEGY_STAGED_CACHE_ENABLED",
		"STRATEGY_STAGED_CACHE_TTL",
		"STRATEGY_STAGED_MAX_QUERY_ROWS",
		"STRATEGY_STAGED_QUERY_TIMEOUT",
		"STRATEGY_PROMOTION_MIN_SAMPLE",
		"STRATEGY_PROMOTION_MIN_SIGNED_MOVE_6H_CENTS",
		"STRATEGY_PROMOTION_MAX_REVERSAL_15M_RATIO",
		"STRATEGY_PROMOTION_MAX_ALERTS_PER_DAY",
		"STRATEGY_PROMOTION_BYPASS_EXPLICIT",
		"STRATEGY_OUTCOME_EVALUATOR_ENABLED",
		"STRATEGY_OUTCOME_STANDALONE_ENABLED",
		"STRATEGY_OUTCOME_STANDALONE_RESOLVE_SIDE",
		// v11.9 new
		"HOLDERSYNC_WORKER_ENABLED",
		"HOLDERSYNC_SOURCE_MODE",
		"HOLDERSYNC_RATE_LIMIT_RPS",
		"HOLDERSYNC_REQUIRE_OPEN_INTEREST",
		"BOOK_FEATURE_BARS_ENABLED",
		"BOOK_FEATURE_BARS_REQUIRE_DEPTH_FOR_VACUUM",
		"THESIS_LINES_WORKER_ENABLED",
		"THESIS_LINES_LOOKBACK",
		"THESIS_HOTPATH_MAX_LINKED_MARKETS",
		"THESIS_HOTPATH_QUERY_TIMEOUT",
		"REPRICING_CLOSE_ENABLED",
		"REPRICING_MIN_PEER_COUNT",
		"REPRICING_MIN_LAG_CENTS",
		"REPRICING_PRICE_SOURCE",
		// per-strategy enable + shadow flags
		"THESIS_ACCUM_ENABLED", "THESIS_ACCUM_SHADOW_ONLY",
		"OWNERSHIP_V2_ENABLED", "OWNERSHIP_V2_SHADOW_ONLY",
		"CATALYST_WINDOW_ENABLED", "CATALYST_WINDOW_SHADOW_ONLY",
		"BOOK_VACUUM_ENABLED", "BOOK_VACUUM_SHADOW_ONLY",
		"REPRICING_LAG_ENABLED", "REPRICING_LAG_SHADOW_ONLY",
		"WALLET_COHORT_ENABLED", "WALLET_COHORT_SHADOW_ONLY",
		"CONFLICT_RESOLVE_ENABLED", "CONFLICT_RESOLVE_SHADOW_ONLY",
		"RULES_RISK_ENABLED",
		"CHEAPTAIL_ENABLED", "CHEAPTAIL_SHADOW_ONLY",
		// v11.10 admin/user Telegram flow split
		"TELEGRAM_STRATEGY_ADMIN_FLOW_ENABLED",
		"TELEGRAM_STRATEGY_USER_FLOW_ENABLED",
		"TELEGRAM_STRATEGY_SHADOW_TO_ADMIN",
		"TELEGRAM_STRATEGY_PROMOTED_TO_USER",
		"TELEGRAM_STRATEGY_MIN_USER_CONFIDENCE",
		"TELEGRAM_STRATEGY_MIN_USER_LEVEL",
		"TELEGRAM_STRATEGY_USER_DEDUPE_WINDOW",
		"TELEGRAM_STRATEGY_ADMIN_DEDUPE_WINDOW",
		// v11.10 PART 6 — worker priority-bucket budgeting
		"WORKER_BUDGET_OPERATOR_PINNED_MARKETS",
		"WORKER_BUDGET_RECENT_ALERT_MARKETS",
		"WORKER_BUDGET_CATALYST_NEAR_MARKETS",
		"WORKER_BUDGET_LINKED_TO_FIRED_MARKETS",
		"WORKER_BUDGET_LIQUID_MARKETS",
		"WORKER_BUDGET_FALLBACK_ACTIVE_MARKETS",
		"WORKER_OPERATOR_PINNED_CONDITION_IDS",
		// v11.10-insider-prior — full surface
		"STRATEGY_LEARNING_LOOP_VERSION",
		"STRATEGY_PROMOTION_FORCE_DISABLE",
		"STRATEGY_PROMOTION_REVIEW_INTERVAL",
		"STRATEGY_PROMOTION_REVIEW_LOOKBACK",
		"THESIS_ACCUM_LOOKBACK_RECENT",
		"THESIS_ACCUM_LOOKBACK_LIFETIME",
		"THESIS_ACCUM_MIN_CONSISTENCY",
		"THESIS_ACCUM_MIN_ALIGNED_SCORE",
		"THESIS_ACCUM_CATALYST_BOOST_MAX",
		"THESIS_ACCUM_LIQUIDITY_FLOOR_USD",
		"THESIS_ACCUM_MAX_LINKED_MARKETS",
		"OWNERSHIP_V2_MIN_PCT_OI_INFO",
		"OWNERSHIP_V2_MIN_PCT_OI_WARN",
		"OWNERSHIP_V2_MIN_PCT_OI_CRIT",
		"OWNERSHIP_V2_TOPK",
		"OWNERSHIP_V2_MIN_SHARES_DELTA",
		"OWNERSHIP_V2_FRESH_SNAPSHOT_MAX_AGE",
		"OWNERSHIP_V2_DENOMINATOR_PENALTY_OI",
		"REPRICING_LAG_CHECK_WINDOWS",
		"REPRICING_LAG_MAX_AMBIGUITY",
		"REPRICING_LAG_MIN_CENTS",
		"REPRICING_LAG_PEER_MIN_COUNT",
		"REPRICING_LAG_OPEN_INTERVAL",
		"REPRICING_LAG_CLOSE_GRACE",
		"WALLET_COHORT_COTRADE_WINDOW",
		"WALLET_COHORT_CONVERGENCE_WINDOW",
		"WALLET_COHORT_MIN_COHORT_HITS",
		"WALLET_COHORT_FRESH_WALLET_MIN_BURST",
		"WALLET_COHORT_FRESH_WALLET_MAX_AGE",
		"CONFLICT_RESOLVE_MM_PENALTY",
		"CONFLICT_RESOLVE_MIN_QUALITY_SUM",
		"RULES_RISK_HIGH_THRESHOLD",
		"RULES_RISK_MID_THRESHOLD",
		"RULES_RISK_HIGH_CAP_SEVERITY",
		"CHEAPTAIL_MIN_NOTIONAL_USD",
		"CHEAPTAIL_AMBIGUITY_CUTOFF",
		"CHEAPTAIL_MIN_PROB",
		"CHEAPTAIL_MAX_PROB",
		"HOLDERSYNC_TOPK",
		"HOLDERSYNC_MAX_MARKETS",
		"HOLDERSYNC_PER_MARKET_TIMEOUT",
		"BOOK_FEATURE_BARS_INTERVAL",
		"BOOK_FEATURE_BARS_MAX_MARKETS",
		"BOOK_FEATURE_BARS_RETENTION",
		"REPRICING_WORKER_INTERVAL",
		"REPRICING_WORKER_OPEN_LOOKBACK",
		"REPRICING_WORKER_MAX_OPEN_WINDOWS",
		"REPRICING_WORKER_CLOSE_AFTER",
		"RISKSCORE_INTERVAL",
		"RISKSCORE_BATCH_SIZE",
		"RISKSCORE_REFRESH_OLDER_THAN",
		"WALLETGRAPH_INTERVAL",
		"WALLETGRAPH_COTRADE_WINDOW",
		"WALLETGRAPH_MIN_SHARED_EVENTS",
		"WALLETGRAPH_USE_FUNDING_PROVIDER",
		"MARKETLINKS_INTERVAL",
		"MARKETLINKS_BATCH_SIZE",
		"MARKETLINKS_MIN_CONFIDENCE",
	}
	for _, name := range []string{".env", ".env.example"} {
		keys := readEnvKeys(t, repoFile(name))
		set := map[string]bool{}
		for _, k := range keys {
			set[k] = true
		}
		var missing []string
		for _, req := range required {
			if !set[req] {
				missing = append(missing, req)
			}
		}
		if len(missing) > 0 {
			t.Fatalf("%s missing required v11.x strategy keys: %v", name, missing)
		}
	}
}

// TestEnvFiles_DangerousDefaultsBlocked guarantees no env file ships
// with a dangerous live-promotion / Telegram-noise default.
func TestEnvFiles_DangerousDefaultsBlocked(t *testing.T) {
	// v11.10 precision-first: only check keys that are ACTIVE in the
	// config struct. Legacy noisy surfaces (WATCHTOWER_STATS_TELEGRAM
	// _ENABLED, PREDICTION_*_TELEGRAM_ENABLED) are stale-rejected via
	// `staleEnvKeys{}` — setting them with ANY value boot-fails, so
	// they must NOT appear in env files at all.
	mustFalse := map[string]struct{}{
		"STRATEGY_LEARNING_LOOP_PROMOTION_ALLOWED": {},
		"STRATEGY_PROMOTION_BYPASS_EXPLICIT":       {},
		"STRATEGY_PROMOTION_FORCE_DISABLE":         {},
		"STRATEGY_SHADOW_RECORD_NOFIRE":            {},
		"TELEGRAM_STRATEGY_USER_FLOW_ENABLED":      {},
	}
	pairRe := regexp.MustCompile(`^([A-Z][A-Z0-9_]+)=(.*)$`)
	for _, file := range []string{".env", ".env.example"} {
		raw, err := os.ReadFile(repoFile(file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if !pairRe.MatchString(line) {
				continue
			}
			parts := pairRe.FindStringSubmatch(line)
			key, val := parts[1], strings.TrimSpace(parts[2])
			if _, ok := mustFalse[key]; ok {
				if !strings.EqualFold(val, "false") {
					t.Fatalf("%s: %s must default false (got %q)", file, key, val)
				}
			}
		}
	}
}

// repoFile resolves a path relative to the repository root from the
// `internal/app` test working directory.
func repoFile(p string) string { return "../../" + p }

func readEnvKeys(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	re := regexp.MustCompile(`^([A-Z][A-Z0-9_]+)=`)
	set := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if m := re.FindStringSubmatch(line); len(m) == 2 {
			set[m[1]] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

func diff(a, b []string) []string {
	in := map[string]bool{}
	for _, k := range b {
		in[k] = true
	}
	var out []string
	for _, k := range a {
		if !in[k] {
			out = append(out, k)
		}
	}
	return out
}
