-- v11.9 PART 4 — extend repricing_windows.status CHECK to cover the
-- new "stale_missing_price" and "stale_missing_peers" states that
-- the real close-phase sampler emits when target / peer price data
-- is unavailable. The old CHECK only allowed open + 3 closed_*
-- statuses; the new sampler distinguishes "ran out of data" from
-- "blocked by ambiguity" so the operator can tell them apart.
ALTER TABLE polymarket_repricing_windows
    DROP CONSTRAINT IF EXISTS polymarket_repricing_windows_status_check;

ALTER TABLE polymarket_repricing_windows
    ADD CONSTRAINT polymarket_repricing_windows_status_check
    CHECK (status IN (
        'open',
        'closed_no_lag',
        'closed_lag_detected',
        'closed_blocked',
        'stale_missing_price',
        'stale_missing_peers'
    ));
