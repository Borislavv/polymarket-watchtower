-- v11.10 PART 7 — bucketed promotion review diagnostics.
--
-- Adds per-bucket diagnostics to polymarket_strategy_promotion_reviews
-- so a strategy cannot be declared healthy solely from a weak whole-
-- strategy median. Bucket dimensions in v11.10:
--
--   * decision_level: info / warning / critical / hard
--   * linkage:        linked (linked_alert_dedup_key NOT NULL) /
--                     standalone (NULL)
--
-- Stored as JSONB so future strategies can add their own bucket
-- dimensions (breadth bands for thesisaccum, pct_oi bands for
-- holderdelta, etc.) without a schema migration. Shape:
--
--   {
--     "by_decision_level": [
--        {"key":"info",     "sample_size": 12, "median_clv_6h": 0.4, ...},
--        {"key":"warning",  "sample_size": 30, "median_clv_6h": 1.8, ...},
--        ...
--     ],
--     "by_linkage": [
--        {"key":"linked",     ...},
--        {"key":"standalone", ...}
--     ]
--   }
ALTER TABLE polymarket_strategy_promotion_reviews
    ADD COLUMN IF NOT EXISTS bucket_diagnostics JSONB;

CREATE INDEX IF NOT EXISTS idx_strategy_promotion_reviews_buckets
    ON polymarket_strategy_promotion_reviews USING GIN (bucket_diagnostics jsonb_path_ops)
    WHERE bucket_diagnostics IS NOT NULL;
