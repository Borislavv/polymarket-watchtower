-- v10.8 alert-concentration queries.
--
-- These are the two seam queries the concentration.Gate calls before
-- an alert is persisted. Both intentionally scan only the recent
-- window so they stay cheap; the existing
-- idx_alerts_created_at + idx_alerts_market_id indexes already cover
-- them.

-- name: RecentAlertsForEventConcentration :many
-- Returns the recent alerts on an event, oldest-first. The caller
-- (concentration.Gate) sorts internally; row order isn't load-bearing.
-- Only `status='sent'` rows count — pending/failed rows didn't reach
-- the operator and shouldn't constrain future emits.
SELECT
    a.created_at,
    pm.event_slug,
    (a.payload->'Trade'->>'Wallet')           AS wallet,
    COALESCE((a.payload->'Trade'->>'NotionalUSD')::double precision, 0)::double precision AS notional_usd,
    a.severity
FROM polymarket_alerts a
JOIN polymarket_markets pm ON pm.id = a.market_id
WHERE pm.event_slug   = @event_slug
  AND a.created_at    > @since
  AND a.status        = 'sent';

-- name: RecentAlertsForWalletConcentration :many
-- Returns the wallet's recent alerts on the event. Same shape; the
-- wallet column comes from polymarket_alerts.payload (Pascal-case
-- key `Wallet`). The check is operator-targeted, so unsent rows
-- don't count here either.
SELECT
    a.created_at,
    pm.event_slug,
    (a.payload->'Trade'->>'Wallet')           AS wallet,
    COALESCE((a.payload->'Trade'->>'NotionalUSD')::double precision, 0)::double precision AS notional_usd,
    a.severity
FROM polymarket_alerts a
JOIN polymarket_markets pm ON pm.id = a.market_id
WHERE pm.event_slug                  = @event_slug
  AND (a.payload->'Trade'->>'Wallet') = @wallet
  AND a.created_at                   > @since
  AND a.status                       = 'sent';
