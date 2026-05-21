-- Rollback for v11.0 Hourly News Intelligence persistence.
DROP TABLE IF EXISTS polymarket_news_intel_processed_items;
DROP TABLE IF EXISTS polymarket_news_intel_decisions;
DROP TABLE IF EXISTS polymarket_news_intel_runs;
