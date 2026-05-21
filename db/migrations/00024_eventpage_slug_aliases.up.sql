-- v10.5 EventPage canonical-slug aliases.
--
-- Polymarket's Next.js data route returns 307 to /event/<canonical>
-- for some slugs (rename, locale, normalisation, vercel deploy
-- rotation). The client stashes (original, canonical) here so future
-- fetches hit the canonical URL directly without paying the 307
-- round-trip. This is metadata only — annotations / catalysts /
-- predictions remain keyed on event_slug as before, and the canonical
-- slug typically IS the operator-known event_slug.
--
-- Rows are kept indefinitely; the client refreshes the in-memory
-- cache on Provider boot via repository lookup.
CREATE TABLE IF NOT EXISTS polymarket_event_slug_aliases (
    original_slug   TEXT NOT NULL PRIMARY KEY,
    canonical_slug  TEXT NOT NULL,
    source          TEXT NOT NULL DEFAULT 'redirect',
    first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_event_slug_aliases_canonical
    ON polymarket_event_slug_aliases(canonical_slug);
