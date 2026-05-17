# Test scenarios

The canonical numeric table the detector test suite asserts against. Use it
to sanity-check any threshold change before shipping — every row maps a
concrete trade shape (median, notional, price) to the severity the
`single_cluster` detector should emit under the test-suite defaults.

## Test-suite defaults

Defined in `internal/app/usecase/detect/detect_test.go::defaultThresholds`:

| Tier              | notional ≥ | odds ≥ | multiplier ≥ |
|-------------------|------------|--------|--------------|
| Info              | $10,000    | 3      | 100×         |
| Warning           | $25,000    | 5      | 1,000×       |
| Critical          | $100,000   | 8      | 10,000×      |
| HardPromotionA    | $250,000   | 5      | 1,000×       |
| HardPromotionB    | $100,000   | 10     | 2,500×       |
| HugeWhale         | $250,000   | 5      | 1,000×       |
| MegaWhale         | $1,000,000 | 3      | 250×         |

Baseline gates: `MinBaselineTrades=20`, `MinBaselineNotionalUSD=$1,000`,
`BaselineMinTradeUSD=$50` (the warm fixture seeds 30 trades).

## Single-trade severity table

Each row is asserted by `TestSeverityTableFromStrategy` in
`detect_test.go`.

| Scenario                                  | median | notional | odds | abs    | mul                   | conservative | overrides fire | **emitted** |
|-------------------------------------------|-------:|---------:|-----:|--------|----------------------:|--------------|----------------|-------------|
| $100k @ odds 8 / median 100 (mul 1000)    |   $100 |  $100k   |   8  | Crit   | 1,000 → Warning       | Warning      | none           | **Warning** |
| $100k @ odds 3 / median 100               |   $100 |  $100k   |   3  | Info   | 1,000 → Warning       | Info         | none           | **Info**    |
| $25k @ odds 5 / median 100 (mul 250)      |   $100 |   $25k   |   5  | Warn   |   250 → Info          | Info         | none           | **Info**    |
| $10k @ odds 3 / median 100                |   $100 |   $10k   |   3  | Info   |   100 → Info          | Info         | none           | **Info**    |
| $9,999 @ odds 3                           |   $100 |  $9,999  |   3  | —      | —                     | —            | —              | **none**    |
| $10k @ odds 2.99                          |   $100 |   $10k   | 2.99 | —      | —                     | —            | —              | **none**    |
| $100k @ odds 8 / median 60 (mul 1666)     |    $60 |  $100k   |   8  | Crit   | 1,666 → Warning       | Warning      | none           | **Warning** |
| $1M @ odds 3 / median 60 (mul 16666)      |    $60 |    $1M   |   3  | Info   | 16,666 → Crit         | Info         | MegaWhale → Hard | **Hard**  |
| $100k @ odds 8 / median 1000 (mul 100)    |  $1000 |  $100k   |   8  | Crit   |   100 → Info          | Info         | none           | **Info**    |
| $300k @ odds 6 / median 60 (mul 5000)     |    $60 |  $300k   |   6  | Warn   | 5,000 → Warning       | Warning      | HardPromotionA → Hard | **Hard** |
| $150k @ odds 12 / median 60 (mul 2500)    |    $60 |  $150k   |  12  | Crit   | 2,500 → Warning       | Warning      | HardPromotionB → Hard | **Hard** |

A few rows worth highlighting:

- **`$100k @ odds 8 / median 100` ⇒ Warning, not Hard.** With the new
  defaults the Hard floor moved off the Critical-tier shape (Critical now
  fires on `100k+8+10000×`, while Hard requires either `$250k+5+1000×` or
  `$100k+10+2500×`). A classic "$100k at fair odds" no longer auto-promotes —
  it lands at Warning, which is the right place for a notable-but-not-
  conclusive trade.
- **MegaWhale catches the $1M low-odds case.** `$1M @ odds 3` looks pedestrian
  to conservative-MIN (Info on the absolute side because odds clears 3 but
  not 5; Critical on the multiplier side; conservative-MIN = Info). MegaWhale
  forces Hard because the raw size is the signal.
- **HardPromotionA vs B catch different shapes.** A $300k bet at moderate
  odds 6 is HardPromotionA territory (notional-led). A $150k bet at extreme
  odds 12 is HardPromotionB territory (odds-led). Both produce Hard.

## Sub-cluster scenarios

Asserted by `TestSubClusterFiresOnDistributedWhales` and friends:

| Scenario                                       | Behaviour                                              |
|------------------------------------------------|--------------------------------------------------------|
| 10 wallets × $6k @ odds 6, median $60          | None fire individually; sub-cluster fires HARD on the 5th wallet (5 wallets, ≥$25k). |
| 4 wallets × $6k @ odds 6                       | No fire — below `SUB_CLUSTER_MIN_UNIQUE_TRADERS=5`.    |
| 10 wallets × $6k an hour ago, 1 fresh          | No fire — stale candidates fall outside the 30m window.|
| 6 wallets × $3k                                | No fire — wallets clear but total $18k below $25k floor.|
| Trade clearing the absolute floor ($15k @ 5)   | Fires single-trade alert, does NOT enter sub-cluster.  |

The sub-cluster and the normal cluster see disjoint trade sets (firing →
cluster; non-firing-but-qualifying → sub-cluster), so a single category can
legitimately produce both signals on the same window when both the high-
profile bets and the split-wallet pattern are present.

## Gates

| Gate                                  | Test                                          | Behaviour |
|---------------------------------------|-----------------------------------------------|-----------|
| Lifecycle pct below floor             | `TestLifecycleGateSkipsEarlyMarkets`          | No alert (baseline still warms). |
| Lifecycle pct in HOT range            | `TestLifecycleMarksHotInFinalStretch`         | Fires with `Hot=true`. |
| Market with no start/end + fail-closed | `TestUnknownLifecycleFailsClosedByDefault`   | No alert. |
| Market age < `MARKET_MIN_AGE`         | `TestMarketMinAgeBlocksTooYoung`              | No alert. |
| Baseline span < `BASELINE_MIN_READY_WINDOW` | `TestBaselineMinReadySpanBlocksThinSpan` | No alert. |
| 1-month market on 1y `BASELINE_WINDOW`| `TestBaselineWindowDoesNotBlockShortMarkets`  | Fires; alert shows actual ~29d span. |
| Blacklisted category                  | `TestBlacklistedCategoryNoAlert`              | No alert. |
| Primary sports category (slug `sports`) | `TestPrimarySportsCategorySkipped`          | No alert. |
| Sports-themed market under non-sports category | `TestSportsLikeMarketUnderNonSportsCategoryAllowed` | **Fires** — category filter is identity-only; market title / event slug are not scanned. |
| `sports` keyword in market metadata only | `TestBlacklistStaysCategoryOnly`           | Fires — blacklist matches category slug+label, never market wording. |
