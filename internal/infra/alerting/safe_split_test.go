package alerting

import (
	"strings"
	"testing"
)

func TestSafeSplit_BelowCapReturnsSingle(t *testing.T) {
	body := "<b>short</b>\nhello world"
	parts := SafeSplitForTelegram(body)
	if len(parts) != 1 || parts[0] != body {
		t.Fatalf("expected single chunk; got %v", parts)
	}
}

func TestSafeSplit_EmptyReturnsNil(t *testing.T) {
	if got := SafeSplitForTelegram(""); got != nil {
		t.Fatalf("expected nil; got %v", got)
	}
	if got := SafeSplitForTelegram("   \n   "); got != nil {
		t.Fatalf("expected nil for whitespace-only; got %v", got)
	}
}

func TestSafeSplit_OverCapSplitsWithoutBreakingTags(t *testing.T) {
	// Construct a body that's just over the cap, with a bold span
	// straddling the natural break.
	half := strings.Repeat("paragraph A. ", 200)
	// 200 * 13 = 2600 chars; add a bold span that crosses the cap.
	body := half + "\n\n<b>start of long bold span " + strings.Repeat("X", 2500) + " end of bold</b>\n\ntail"
	parts := SafeSplitForTelegram(body)
	if len(parts) < 2 {
		t.Fatalf("expected ≥2 chunks; got %d", len(parts))
	}
	for i, p := range parts {
		if len(p) > TelegramMaxMessageChars {
			t.Errorf("chunk %d exceeds cap: %d > %d", i, len(p), TelegramMaxMessageChars)
		}
		// Open <b> count must equal close </b> count per chunk.
		if openCount(p, "<b>") != closeCount(p, "</b>") {
			t.Errorf("chunk %d has unbalanced <b>/</b>: %q", i, p)
		}
	}
}

func TestSafeSplit_PrefersParagraphBoundary(t *testing.T) {
	// First chunk-worth ends with a clean \n\n; we should cut there.
	body := strings.Repeat("a", 3500) + "\n\n" + strings.Repeat("b", 1500)
	parts := SafeSplitForTelegram(body)
	if len(parts) != 2 {
		t.Fatalf("expected 2 chunks; got %d", len(parts))
	}
	if !strings.HasSuffix(parts[0], "a") {
		t.Errorf("first chunk should end on paragraph boundary; tail=%q", parts[0][len(parts[0])-5:])
	}
	if !strings.HasPrefix(parts[1], "b") {
		t.Errorf("second chunk should start at next paragraph; head=%q", parts[1][:5])
	}
}

func TestSafeSplit_NestedTagsClosedAndReopened(t *testing.T) {
	body := "<b>" + strings.Repeat("X", 3500) + "<i>" + strings.Repeat("Y", 2500) + "</i></b>"
	parts := SafeSplitForTelegram(body)
	if len(parts) < 2 {
		t.Fatalf("expected ≥2 chunks; got %d", len(parts))
	}
	for i, p := range parts {
		if openCount(p, "<b>") != closeCount(p, "</b>") {
			t.Errorf("chunk %d b unbalanced: %q", i, snippet(p))
		}
		if openCount(p, "<i>") != closeCount(p, "</i>") {
			t.Errorf("chunk %d i unbalanced: %q", i, snippet(p))
		}
	}
}

func TestSafeSplit_UnknownTagPassedThrough(t *testing.T) {
	// `<custom>` is not in the supported set — it should not be
	// rebalanced (we don't pretend to manage tags we don't know).
	body := "<custom attr=1>" + strings.Repeat("Z", 4500) + "</custom>"
	parts := SafeSplitForTelegram(body)
	if len(parts) < 2 {
		t.Fatalf("expected ≥2 chunks; got %d", len(parts))
	}
	// The opener should appear once at the head of part[0]; we are
	// not responsible for re-opening unknown tags.
	if !strings.HasPrefix(parts[0], "<custom") {
		t.Errorf("first chunk lost <custom>: %q", snippet(parts[0]))
	}
}

func TestSafeSplit_HardCapWhenNoBoundary(t *testing.T) {
	body := strings.Repeat("X", 5000)
	parts := SafeSplitForTelegram(body)
	if len(parts) < 2 {
		t.Fatalf("expected ≥2 chunks; got %d", len(parts))
	}
	for i, p := range parts {
		if len(p) > TelegramMaxMessageChars {
			t.Errorf("chunk %d exceeds cap: %d", i, len(p))
		}
	}
}

func openCount(s, tag string) int  { return strings.Count(s, tag) }
func closeCount(s, tag string) int { return strings.Count(s, tag) }
func snippet(s string) string {
	if len(s) <= 80 {
		return s
	}
	return s[:40] + "…" + s[len(s)-40:]
}
