package eventcatalyst

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Borislavv/polymarket-watchtower/internal/domain/model/anomaly"
	"github.com/Borislavv/polymarket-watchtower/internal/domain/vo"
	"github.com/Borislavv/polymarket-watchtower/internal/infra/repository"
)

type fakeStore struct {
	rows []repository.EventCatalyst
	err  error
}

func (s *fakeStore) ListActive(_ context.Context, _ string) ([]repository.EventCatalyst, error) {
	return s.rows, s.err
}

var _ Store = (*fakeStore)(nil)

func mkCatalyst(t repository.EventCatalystType, status repository.EventCatalystStatus, title string, expected time.Time) repository.EventCatalyst {
	return repository.EventCatalyst{
		EventSlug:            "tx",
		CatalystType:         t,
		Title:                title,
		Description:          "test catalyst description",
		ExpectedAt:           expected,
		Status:               status,
		BullishScenario:      "decisive win reprices YES sharply higher",
		BearishScenario:      "weak showing weakens confidence",
		InvalidationScenario: "disputed outcome extends volatility",
	}
}

func mkFinding(conditionID string) anomaly.Finding {
	return anomaly.Finding{
		Severity: anomaly.SeverityWarning,
		Trade: &anomaly.TradeRef{
			Market:  vo.MarketID(conditionID),
			Outcome: "Ken Paxton",
		},
	}
}

func TestProvider_LoadAndRender_NoCatalystReturnsEmpty(t *testing.T) {
	p := New(Config{Enabled: true}, &fakeStore{}, func(_ context.Context, _ string) string { return "tx" }, nil, nil)
	out := p.LoadAndRenderForFinding(context.Background(), mkFinding("0xpaxton"), 2000)
	if out != "" {
		t.Fatalf("expected empty when no catalysts, got %q", out)
	}
}

func TestProvider_LoadAndRender_UnresolvedSlugReturnsEmpty(t *testing.T) {
	store := &fakeStore{rows: []repository.EventCatalyst{
		mkCatalyst(repository.CatalystTypeRunoff, repository.CatalystStatusActive, "TX runoff", time.Now()),
	}}
	p := New(Config{Enabled: true}, store, func(_ context.Context, _ string) string { return "" }, nil, nil)
	out := p.LoadAndRenderForFinding(context.Background(), mkFinding("unknown-cid"), 2000)
	if out != "" {
		t.Fatalf("expected empty when slug unresolved, got %q", out)
	}
}

func TestProvider_LoadAndRender_RendersAllCatalystFields(t *testing.T) {
	expected := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{rows: []repository.EventCatalyst{
		mkCatalyst(repository.CatalystTypeRunoff, repository.CatalystStatusActive, "Texas GOP runoff", expected),
	}}
	p := New(Config{Enabled: true}, store, func(_ context.Context, _ string) string { return "tx" }, nil, nil)
	out := p.LoadAndRenderForFinding(context.Background(), mkFinding("0xpaxton"), 2000)
	for _, want := range []string{
		"type=runoff",
		"status=active",
		"expected_at=2026-06-15T12:00:00Z",
		"Texas GOP runoff",
		"bullish:",
		"bearish:",
		"invalidation:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in rendered block:\n%s", want, out)
		}
	}
}

func TestProvider_StampBlockedAlert_PicksActiveOverExpected(t *testing.T) {
	earlier := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{rows: []repository.EventCatalyst{
		mkCatalyst(repository.CatalystTypeRunoff, repository.CatalystStatusExpected, "Future ruling", earlier),
		mkCatalyst(repository.CatalystTypeDebate, repository.CatalystStatusActive, "TX runoff", later),
	}}
	p := New(Config{Enabled: true}, store, func(_ context.Context, _ string) string { return "tx" }, nil, nil)
	f := mkFinding("0xpaxton")
	p.StampBlockedAlert(context.Background(), &f)
	if f.Blocked == nil {
		t.Fatal("Blocked must be stamped")
	}
	if !strings.Contains(f.Blocked.Status, "active catalyst resolves: TX runoff") {
		t.Errorf("active catalyst should win; got status %q", f.Blocked.Status)
	}
	if f.Blocked.CatalystType != "debate" {
		t.Errorf("catalyst type wrong: %q", f.Blocked.CatalystType)
	}
}

func TestProvider_StampBlockedAlert_PicksNearestExpectedWhenNoActive(t *testing.T) {
	near := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	far := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{rows: []repository.EventCatalyst{
		mkCatalyst(repository.CatalystTypeRunoff, repository.CatalystStatusExpected, "Far event", far),
		mkCatalyst(repository.CatalystTypeDebate, repository.CatalystStatusExpected, "Near event", near),
	}}
	p := New(Config{Enabled: true}, store, func(_ context.Context, _ string) string { return "tx" }, nil, nil)
	f := mkFinding("0xpaxton")
	p.StampBlockedAlert(context.Background(), &f)
	if f.Blocked == nil {
		t.Fatal("Blocked must be stamped")
	}
	if !strings.Contains(f.Blocked.Status, "Near event") {
		t.Errorf("nearest expected catalyst should win; got %q", f.Blocked.Status)
	}
}

func TestRenderCatalystPromptBlock_CapsItemsAndChars(t *testing.T) {
	rows := []repository.EventCatalyst{
		mkCatalyst(repository.CatalystTypeRunoff, repository.CatalystStatusActive, "A", time.Now()),
		mkCatalyst(repository.CatalystTypeDebate, repository.CatalystStatusExpected, "B", time.Now()),
		mkCatalyst(repository.CatalystTypePoll, repository.CatalystStatusExpected, "C", time.Now()),
	}
	out := RenderCatalystPromptBlock(rows, 2, 0)
	if strings.Contains(out, "title=C") {
		t.Errorf("maxItems=2 should drop row C: %s", out)
	}
	uncapped := RenderCatalystPromptBlock(rows, 100, 0)
	capped := RenderCatalystPromptBlock(rows, 100, 50)
	// The ellipsis "…" is a 3-byte UTF-8 rune, so a 50-byte cap can
	// land at up to 52 bytes (49 of payload + 3 of ellipsis). What
	// matters is that capping shortens the output materially.
	if len(capped) >= len(uncapped) {
		t.Errorf("maxChars must shorten output: capped=%d uncapped=%d", len(capped), len(uncapped))
	}
	if !strings.HasSuffix(capped, "…") {
		t.Errorf("capped output must end with ellipsis marker: %q", capped)
	}
}
