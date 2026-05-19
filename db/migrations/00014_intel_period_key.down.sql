DROP INDEX IF EXISTS idx_intel_reports_period_key;
DROP INDEX IF EXISTS idx_intel_reports_summary_hash;
CREATE UNIQUE INDEX idx_intel_reports_summary_hash
    ON polymarket_market_intelligence_reports (summary_hash);
ALTER TABLE polymarket_market_intelligence_reports DROP COLUMN IF EXISTS period_key;
