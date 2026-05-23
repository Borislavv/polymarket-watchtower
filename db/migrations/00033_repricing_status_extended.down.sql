ALTER TABLE polymarket_repricing_windows
    DROP CONSTRAINT IF EXISTS polymarket_repricing_windows_status_check;

ALTER TABLE polymarket_repricing_windows
    ADD CONSTRAINT polymarket_repricing_windows_status_check
    CHECK (status IN (
        'open',
        'closed_no_lag',
        'closed_lag_detected',
        'closed_blocked'
    ));
