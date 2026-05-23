// strategy_wiring.go — bridges the env-loaded StrategyConfig into a
// strategybus.Bus. Keeping the bridge in `internal/app` avoids the
// reverse dependency (strategybus must not import app).
//
// The bus is constructed once at boot and shared by every v11.5
// detector orchestration site. When the StrategyConfig has every
// detector disabled (the default), the bus is still safe to call —
// Record() short-circuits unknown / disabled strategies.
package app

import (
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/shadowdecisions"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/strategybus"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/metrics"
)

// BuildStrategyBus assembles a strategybus.Bus from the app-level
// StrategyConfig and the writer + metrics handles. Pass
// shadowdecisions.NopWriter when running without Postgres so the
// bus stays inert.
func BuildStrategyBus(s StrategyConfig, writer shadowdecisions.Writer, met *metrics.Metrics) *strategybus.Bus {
	flags := map[string]strategybus.StrategyFlag{
		"thesisaccum":     {Name: "thesisaccum", Enabled: s.ThesisAccum.Enabled, ShadowOnly: s.ThesisAccum.ShadowOnly},
		"holderdelta":     {Name: "holderdelta", Enabled: s.OwnershipV2.Enabled, ShadowOnly: s.OwnershipV2.ShadowOnly},
		"catalystwindow":  {Name: "catalystwindow", Enabled: s.CatalystWindow.Enabled, ShadowOnly: s.CatalystWindow.ShadowOnly},
		"bookvacuum":      {Name: "bookvacuum", Enabled: s.BookVacuum.Enabled, ShadowOnly: s.BookVacuum.ShadowOnly},
		"repricinglag":    {Name: "repricinglag", Enabled: s.RepricingLag.Enabled, ShadowOnly: s.RepricingLag.ShadowOnly},
		"walletcohort":    {Name: "walletcohort", Enabled: s.WalletCohort.Enabled, ShadowOnly: s.WalletCohort.ShadowOnly},
		"conflictresolve": {Name: "conflictresolve", Enabled: s.ConflictResolve.Enabled, ShadowOnly: s.ConflictResolve.ShadowOnly},
		"rulesrisk":       {Name: "rulesrisk", Enabled: s.RulesRisk.Enabled, ShadowOnly: s.RulesRisk.ShadowOnly},
		"cheaptail":       {Name: "cheaptail", Enabled: s.CheapTail.Enabled, ShadowOnly: s.CheapTail.ShadowOnly},
	}
	cfg := strategybus.Config{
		StrategyVersion:        s.StrategyVersion,
		GlobalPromotionAllowed: s.GlobalPromotionAllowed,
		Flags:                  flags,
	}
	return strategybus.New(cfg, writer, met, nil)
}
