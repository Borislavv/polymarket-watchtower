// Package persist is the bridge between the discover/collect loops and
// the Postgres repository layer. It exists as its own package so the
// composition root stays clean and the loops never import the
// repository directly.
//
// Phase 2, stage 3/4: persistence runs ALONGSIDE the existing in-memory
// path. The in-memory observer/registry remains the authoritative source
// for live alerting until the DB-backed detector lands. DB write failures
// are operational, not fatal — callers log and continue.
package persist

import (
	"context"
	"fmt"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/market"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/trade"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

// Sink owns the three repository handles needed to persist a discovery
// sweep (categories + markets) and a batch of collected trades. Trader
// rows are upserted lazily as trades arrive so we don't query for
// trader info on the discovery hot path.
type Sink struct {
	categories *repository.CategoryRepository
	markets    *repository.MarketRepository
	trades     *repository.TradeRepository
	traders    *repository.TraderRepository
	whitelist  []string
}

// NewSink constructs a Sink. The whitelist is applied once on every
// discovery tick so a newly-discovered category is flagged enabled the
// first time it appears.
func NewSink(
	categories *repository.CategoryRepository,
	markets *repository.MarketRepository,
	trades *repository.TradeRepository,
	traders *repository.TraderRepository,
	whitelist []string,
) *Sink {
	return &Sink{
		categories: categories,
		markets:    markets,
		trades:     trades,
		traders:    traders,
		whitelist:  whitelist,
	}
}

// PersistDiscovery writes a full discovery sweep. The order matters:
// categories first (so market_category links resolve), then markets
// (with their category links), then a sweep-level inactive marker
// scoped to the whitelisted-category set.
func (s *Sink) PersistDiscovery(ctx context.Context, cats []market.Category, markets []market.Market) error {
	if s == nil {
		return nil
	}

	// 1. Upsert every seen category.
	repoCats := make([]repository.Category, 0, len(cats))
	for _, c := range cats {
		repoCats = append(repoCats, repository.Category{
			ExternalID: fmt.Sprintf("%d", c.ID),
			Slug:       c.Slug,
			Name:       c.Label,
		})
	}
	persisted, err := s.categories.UpsertSeen(ctx, repoCats)
	if err != nil {
		return fmt.Errorf("upsert categories: %w", err)
	}

	// 2. Refresh enabled flags from the whitelist on every tick — new
	//    categories become enabled the moment they appear upstream.
	enabled, err := s.categories.ApplyWhitelist(ctx, s.whitelist)
	if err != nil {
		return fmt.Errorf("apply whitelist: %w", err)
	}
	enabledByExternal := make(map[string]int64, len(enabled))
	for _, c := range enabled {
		enabledByExternal[c.ExternalID] = c.ID
	}
	// Resolve every persisted category external_id → DB id for link rows.
	dbIDByExternal := make(map[string]int64, len(persisted))
	for _, c := range persisted {
		dbIDByExternal[c.ExternalID] = c.ID
	}

	// 3. Upsert markets that have at least one whitelisted category.
	//    Non-whitelisted markets are skipped at persistence time to keep
	//    DB volume bounded (the in-memory registry already filters them
	//    in the discover loop; this is defence-in-depth).
	repoMarkets := make([]repository.UpsertMarketInput, 0, len(markets))
	seenConditionIDs := make([]string, 0, len(markets))
	for _, m := range markets {
		categoryIDs := make([]int64, 0, len(m.Categories))
		for _, catID := range m.Categories {
			extID := fmt.Sprintf("%d", catID)
			if _, ok := enabledByExternal[extID]; !ok {
				continue
			}
			if id, ok := dbIDByExternal[extID]; ok {
				categoryIDs = append(categoryIDs, id)
			}
		}
		if len(categoryIDs) == 0 {
			continue
		}
		repoMarkets = append(repoMarkets, repository.UpsertMarketInput{
			ConditionID: string(m.ID),
			Slug:        m.Slug,
			Question:    m.Question,
			EventSlug:   m.EventSlug,
			EventTitle:  m.EventTitle,
			StartDate:   m.StartDate,
			EndDate:     m.EndDate,
			Closed:      m.Closed,
			CategoryIDs: categoryIDs,
		})
		seenConditionIDs = append(seenConditionIDs, string(m.ID))
	}
	if _, err := s.markets.UpsertSeen(ctx, repoMarkets); err != nil {
		return fmt.Errorf("upsert markets: %w", err)
	}

	// 4. Mark markets that disappeared from the whitelisted sweep as
	//    inactive. Scope is the enabled-category set so non-whitelisted
	//    categories never get touched.
	scope := make([]int64, 0, len(enabledByExternal))
	for _, id := range enabledByExternal {
		scope = append(scope, id)
	}
	if err := s.markets.MarkSeenInactive(ctx, seenConditionIDs, scope); err != nil {
		return fmt.Errorf("mark inactive: %w", err)
	}
	return nil
}

// PersistTrades writes a freshly-fetched batch of trades for one market.
// Resolves the market's DB id and upserts traders inline. Skips silently
// when the market isn't in the DB yet (i.e. discovery hasn't caught up).
func (s *Sink) PersistTrades(ctx context.Context, m market.Market, trades []trade.Trade) error {
	if s == nil || len(trades) == 0 {
		return nil
	}
	dbMarket, err := s.markets.GetByConditionID(ctx, string(m.ID))
	if err != nil {
		// Market not yet persisted — this trade will be picked up on the
		// next discovery tick. Not an error.
		return nil
	}

	// Upsert traders first so we can resolve their ids when inserting trades.
	wallets := uniqueWallets(trades)
	persisted, err := s.traders.UpsertSeen(ctx, wallets)
	if err != nil {
		return fmt.Errorf("upsert traders: %w", err)
	}
	idByWallet := make(map[string]int64, len(persisted))
	for _, tr := range persisted {
		idByWallet[tr.WalletAddress] = tr.ID
	}

	repoTrades := make([]repository.InsertTradeInput, 0, len(trades))
	for _, t := range trades {
		var traderID *int64
		if id, ok := idByWallet[t.Taker]; ok {
			traderID = &id
		}
		repoTrades = append(repoTrades, repository.InsertTradeInput{
			MarketID:     dbMarket.ID,
			TraderID:     traderID,
			OutcomeToken: string(t.Token),
			Side:         string(t.Side),
			Price:        t.Price,
			SizeShares:   t.Size,
			NotionalUSD:  t.NotionalUSD(),
			TradedAt:     t.Timestamp,
			ExternalID:   t.ID,
			TxHash:       t.TxHash,
		})
	}
	if _, err := s.trades.UpsertBatch(ctx, repoTrades); err != nil {
		return fmt.Errorf("upsert trades: %w", err)
	}
	return nil
}

func uniqueWallets(trades []trade.Trade) []string {
	seen := make(map[string]struct{}, len(trades))
	out := make([]string, 0, len(trades))
	for _, t := range trades {
		if t.Taker == "" {
			continue
		}
		if _, ok := seen[t.Taker]; ok {
			continue
		}
		seen[t.Taker] = struct{}{}
		out = append(out, t.Taker)
	}
	return out
}
