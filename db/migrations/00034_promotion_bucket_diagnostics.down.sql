DROP INDEX IF EXISTS idx_strategy_promotion_reviews_buckets;
ALTER TABLE polymarket_strategy_promotion_reviews
    DROP COLUMN IF EXISTS bucket_diagnostics;
