# Test scenarios

The canonical numeric table the detector test suite asserts against. Use it
to sanity-check any threshold change before shipping — every row maps a
concrete trade shape (median, notional, price) to the severity the
`single_cluster` detector should emit under the test-suite defaults.

## Test-suite defaults

Defined in `internal/app/usecase/detect/detect_test.go::defaultThresholds`:

| Tier     | notional ≥ | odds ≥ | multiplier ≥ |
|----------|------------|--------|--------------|
| Info     | $10,000    | 3      | 100×         |
| Warning  | $25,000    | 5      | 1,000×       |
| Critical | $100,000   | 8      | 10,000×      |

Baseline gates: `MinBaselineTrades=20`, `MinBaselineNotionalUSD=$1,000`,
`BaselineMinTradeUSD=$50` (the warm fixture seeds 30 trades).

Single-trade severity caps at **Critical**. `Hard` is reserved for the
cluster detector.

## Single-trade severity table

Each row is asserted by `TestSeverityTableFromStrategy`:

| Scenario                              | median | notional | odds  | absolute | multiplier             | **emitted** |
|---------------------------------------|-------:|---------:|------:|----------|-----------------------:|-------------|
| $100k @ odds 8, mul 1,000             |  $100  |  $100k   |   8   | Critical | 1,000 → Warning        | **Warning** |
| $100k @ odds 3                        |  $100  |  $100k   |   3   | Info     | 1,000 → Warning        | **Info**    |
| $25k @ odds 5, mul 250                |  $100  |   $25k   |   5   | Warning  |   250 → Info           | **Info**    |
| $10k @ odds 3, mul 100                |  $100  |   $10k   |   3   | Info     |   100 → Info           | **Info**    |
| $9,999 @ odds 3 (below floor)         |  $100  |  $9,999  |   3   | —        | —                      | **none**    |
| $10k @ odds 2.99 (below odds floor)   |  $100  |   $10k   | 2.99  | —        | —                      | **none**    |
| $100k @ odds 8, mul ≈1,666            |   $60  |  $100k   |   8   | Critical | 1,666 → Warning        | **Warning** |
| $1M  @ odds 3, mul ≈16,666            |   $60  |    $1M   |   3   | Info     | 16,666 → Critical      | **Info**    |
| $100k @ odds 8, mul 100               | $1,000 |  $100k   |   8   | Critical |   100 → Info           | **Info**    |
| $300k @ odds 6, mul 5,000             |   $60  |  $300k   |   6   | Warning  | 5,000 → Warning        | **Warning** |
| $150k @ odds 12, mul 2,500            |   $60  |  $150k   |  12   | Critical | 2,500 → Warning        | **Warning** |
| $700k @ odds 10, mul ≈11,666          |   $60  |  $700k   |  10   | Critical | 11,666 → Critical      | **Critical**|

A few rows worth highlighting:

- **`$100k @ odds 8 / median 100` ⇒ Warning.** The absolute side qualifies
  Critical, but multiplier 1,000 is only Warning rung; conservative-MIN
  rules.
- **`$1M @ odds 3` ⇒ Info.** Multiplier alone is critical (16,666×) but
  odds 3 only clears Info on the absolute side. Conservative-MIN keeps
  this honest: low odds means little asymmetric-payoff signal regardless
  of raw size.
- **`$700k @ odds 10 / median 60` ⇒ Critical.** Both sides clear Critical
  → conservative-MIN = Critical. The strongest single-trade signal short
  of cluster escalation.

## Regression cases

| Scenario                                       | Test                                       | Expected |
|------------------------------------------------|--------------------------------------------|----------|
| France/FIFA inside `Hide From New` (the original silenced alert) | `TestFranceFifaHideFromNewStillAlerts` | **Fires** Info |
| Primary `sports` category                      | `TestPrimarySportsCategorySkipped`         | No alert |
| Sports-themed market under non-sports category | `TestSportsLikeMarketUnderNonSportsCategoryAllowed` | **Fires** |
| `sports` keyword only in market metadata        | `TestBlacklistStaysCategoryOnly`           | **Fires** |

## Gates

| Gate                                  | Test                                          | Behaviour |
|---------------------------------------|-----------------------------------------------|-----------|
| Lifecycle pct below floor             | `TestLifecycleGateSkipsEarlyMarkets`          | No alert (baseline still warms). |
| Lifecycle pct in HOT range            | `TestLifecycleMarksHotInFinalStretch`         | Fires with `Hot=true`. |
| Market with no start/end + fail-closed | `TestUnknownLifecycleFailsClosedByDefault`   | No alert. |
| Market age < `MARKET_MIN_AGE`         | `TestMarketMinAgeBlocksTooYoung`              | No alert. |
| Baseline span < `BASELINE_MIN_READY_WINDOW` | `TestBaselineMinReadySpanBlocksThinSpan` | No alert. |
| 1-month market on 1y `BASELINE_WINDOW`| `TestBaselineWindowDoesNotBlockShortMarkets`  | Fires; alert shows the actual ~29 d span. |
| Blacklisted category                  | `TestBlacklistedCategoryNoAlert`              | No alert. |

## Cluster

| Scenario                                              | Test                                          | Expected |
|-------------------------------------------------------|-----------------------------------------------|----------|
| 3 single-trade Info alerts from 3 wallets in 1 category | `TestClusterHardAlert`                      | One HARD `WhaleClusterDetected`. |
| 2 trades same wallet                                  | (cluster unit tests)                          | No fire — needs distinct wallets. |
| Cluster within cooldown                               | (cluster unit tests)                          | No fire on second window. |
