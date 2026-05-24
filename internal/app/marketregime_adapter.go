// marketregime_adapter.go — v11.12-insider-prior wiring shim that
// adapts the marketregime classifier (pure analytics package) to the
// detect.MarketRegimeClassifier interface. Lives in app/ rather than
// detect/ so the detect package stays free of imports on every
// analytics package.
package app

import (
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/analytics/marketregime"
	"github.com/Borislavv/polymarket-watchtower/internal/app/usecase/detect"
)

type marketRegimeAdapter struct {
	c *marketregime.Classifier
}

func newMarketRegimeAdapter() *marketRegimeAdapter {
	return &marketRegimeAdapter{c: marketregime.New()}
}

func (a *marketRegimeAdapter) Classify(in detect.MarketRegimeInput) detect.MarketRegimeVerdict {
	v := a.c.Classify(marketregime.Input{
		CategorySlug:    in.CategorySlug,
		CategoryLabel:   in.CategoryLabel,
		Title:           in.Title,
		Description:     in.Description,
		ResolutionRules: in.ResolutionRules,
		EventSlug:       in.EventSlug,
	})
	return detect.MarketRegimeVerdict{
		Regime:  string(v.Regime),
		Score:   v.Score,
		Reasons: v.Reasons,
	}
}
