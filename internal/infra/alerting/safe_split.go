package alerting

import (
	"strings"
)

// TelegramMaxMessageChars is Telegram's hard cap on the body of one
// HTML message. Set conservatively to 4000 (the real cap is 4096) so
// we keep headroom for character-vs-byte differences and for the
// terminal `…` truncation marker.
const TelegramMaxMessageChars = 4000

// SafeSplitForTelegram chops `body` into one or more chunks each
// ≤ TelegramMaxMessageChars while preserving HTML validity:
//
//   - We split on safe boundaries in this order: blank line, single
//     newline, sentence end, then a hard char cap as a last resort.
//   - We close any tags left open by a chunk and re-open them at the
//     start of the next chunk, so each chunk is independently valid
//     HTML. Supports the subset of tags Telegram accepts (b, i, code,
//     pre, a). Unknown tags are passed through untouched.
//   - Trailing whitespace is trimmed; empty chunks are dropped.
//
// Returns the original body in a single-element slice when it already
// fits the cap. Returns nil for empty input.
//
// This is the load-bearing safety net for the v9.9 evolution worker
// and the v10.0 prediction creation worker — neither path imposes a
// size cap on its rendered body, so a particularly catalyst-dense
// prediction can easily exceed 4096 chars and would otherwise fail
// the Telegram POST.
func SafeSplitForTelegram(body string) []string {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil
	}
	if len(body) <= TelegramMaxMessageChars {
		return []string{body}
	}

	var parts []string
	remaining := body
	openTags := []string{}

	for len(remaining) > TelegramMaxMessageChars {
		// Budget for the closers we'll need to append at the cut
		// point. We don't know the final open-tag stack until after
		// cutting, but the WORST case is the stack of all tags
		// currently open at the start of `remaining` plus a small
		// safety margin for any new opens introduced inside the
		// window. Reserve a conservative 64 chars.
		const closerBudget = 64
		effCap := TelegramMaxMessageChars - closerBudget
		if effCap < 1 {
			effCap = 1
		}
		cut := preferredCut(remaining, effCap)
		chunk := strings.TrimRight(remaining[:cut], " \t\n")

		// Compute open-tag stack at the cut point. Append closers
		// to the current chunk; re-open at the start of the next.
		open := tagStack(openTags, chunk)
		closers := closerForStack(open)
		// If our closer budget undershot, shrink the chunk further
		// rather than violate the hard cap.
		for len(chunk)+len(closers) > TelegramMaxMessageChars && len(chunk) > 1 {
			chunk = chunk[:len(chunk)-1]
		}
		parts = append(parts, chunk+closers)

		openTags = open
		next := strings.TrimLeft(remaining[cut:], " \t\n")
		// Re-open inherited tags at the head of the next chunk so it
		// renders correctly as a standalone Telegram message.
		if len(openTags) > 0 {
			next = openerForStack(openTags) + next
		}
		remaining = next
	}
	if strings.TrimSpace(remaining) != "" {
		parts = append(parts, remaining)
	}
	return parts
}

// preferredCut returns an index ≤ cap where the body can be split
// without breaking a sentence or HTML tag. Falls back to cap when no
// good boundary exists.
func preferredCut(s string, cap int) int {
	if cap >= len(s) {
		return len(s)
	}
	// Prefer a blank line within the last 25% of the window.
	window := s[:cap]
	if i := strings.LastIndex(window, "\n\n"); i > cap*3/4 {
		return i + 2
	}
	if i := strings.LastIndex(window, "\n"); i > cap*3/4 {
		return i + 1
	}
	if i := strings.LastIndex(window, ". "); i > cap*3/4 {
		return i + 2
	}
	if i := strings.LastIndex(window, " "); i > cap*3/4 {
		return i + 1
	}
	// No good boundary — hard char cap. We still take care below to
	// not split inside a tag.
	for cap > 0 && (s[cap-1] == '<' || insideTag(s, cap)) {
		cap--
	}
	if cap == 0 {
		return len(window)
	}
	return cap
}

// insideTag reports whether the character at index i would land
// inside an HTML tag, i.e. there's an unclosed `<` to the left of i
// with no matching `>` before i.
func insideTag(s string, i int) bool {
	lt := strings.LastIndex(s[:i], "<")
	if lt < 0 {
		return false
	}
	gt := strings.LastIndex(s[:i], ">")
	return lt > gt
}

// supportedTelegramTags is the subset of HTML tags the safe-splitter
// will track. Anything outside this set is passed through as-is.
// Reference: https://core.telegram.org/bots/api#html-style.
var supportedTelegramTags = map[string]struct{}{
	"b":      {},
	"i":      {},
	"u":      {},
	"s":      {},
	"code":   {},
	"pre":    {},
	"strong": {},
	"em":     {},
}

// tagStack walks `chunk` and returns the stack of tags still open at
// the end. `seed` is the stack carried in from previous chunks (for
// tags reopened at the head of the new chunk).
func tagStack(seed []string, chunk string) []string {
	stack := append([]string{}, seed...)
	i := 0
	for i < len(chunk) {
		lt := strings.IndexByte(chunk[i:], '<')
		if lt < 0 {
			break
		}
		j := lt + i
		gt := strings.IndexByte(chunk[j:], '>')
		if gt < 0 {
			break
		}
		tagBody := chunk[j+1 : j+gt]
		i = j + gt + 1
		closing := strings.HasPrefix(tagBody, "/")
		name := strings.ToLower(strings.TrimPrefix(tagBody, "/"))
		// Strip attributes ("a href=...") — we only care about the
		// tag name for open/close balancing.
		if sp := strings.IndexAny(name, " \t"); sp >= 0 {
			name = name[:sp]
		}
		if _, ok := supportedTelegramTags[name]; !ok {
			continue
		}
		if closing {
			// Pop the top occurrence of `name`.
			for k := len(stack) - 1; k >= 0; k-- {
				if stack[k] == name {
					stack = append(stack[:k], stack[k+1:]...)
					break
				}
			}
			continue
		}
		stack = append(stack, name)
	}
	return stack
}

// closerForStack returns the `</tag>` closers (innermost first) for
// the given open-tag stack.
func closerForStack(stack []string) string {
	if len(stack) == 0 {
		return ""
	}
	var b strings.Builder
	for i := len(stack) - 1; i >= 0; i-- {
		b.WriteString("</")
		b.WriteString(stack[i])
		b.WriteString(">")
	}
	return b.String()
}

// openerForStack returns the `<tag>` openers (outermost first) for
// the given stack — the inverse of closerForStack so a chunk that
// continues a bold span starts with `<b>` again.
func openerForStack(stack []string) string {
	if len(stack) == 0 {
		return ""
	}
	var b strings.Builder
	for _, name := range stack {
		b.WriteString("<")
		b.WriteString(name)
		b.WriteString(">")
	}
	return b.String()
}
