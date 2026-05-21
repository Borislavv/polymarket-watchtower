DROP INDEX IF EXISTS idx_realtime_work_queue_condition;
DROP INDEX IF EXISTS idx_realtime_work_queue_pending;
DROP TABLE IF EXISTS polymarket_realtime_work_queue;

DROP INDEX IF EXISTS idx_gap_recoveries_condition;
DROP TABLE IF EXISTS polymarket_ws_gap_recoveries;

DROP INDEX IF EXISTS idx_live_market_state_event;
DROP TABLE IF EXISTS polymarket_live_market_state;

DROP INDEX IF EXISTS idx_ws_events_type_received;
DROP INDEX IF EXISTS idx_ws_events_token_received;
DROP INDEX IF EXISTS idx_ws_events_event_received;
DROP INDEX IF EXISTS idx_ws_events_condition_received;
DROP TABLE IF EXISTS polymarket_ws_events;
