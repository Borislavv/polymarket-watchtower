-- 00002_alerts_sending_state.down.sql
--
-- Roll back the `sending` status. Any rows currently in `sending` are
-- moved back to `pending` so the constraint can be re-tightened.

BEGIN;

UPDATE polymarket_alerts SET status = 'pending' WHERE status = 'sending';

ALTER TABLE polymarket_alerts DROP CONSTRAINT polymarket_alerts_status_valid;
ALTER TABLE polymarket_alerts ADD CONSTRAINT polymarket_alerts_status_valid
    CHECK (status IN ('pending','sent','failed'));

COMMIT;
