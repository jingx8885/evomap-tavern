package livevoice

import "strings"

// appendTokenCap is below the gateway limit of 500 tokens on
// session.context.append. Non-ASCII is counted as two tokens so a
// Chinese note cannot slip past the real tokenizer.
const appendTokenCap = 460

// FitHead keeps the start of text inside the append cap.
func FitHead(text string) string {
	return fitAppend(text, false)
}

// FitTail keeps the end of text inside the append cap.
// Observation notes put the useful lines last.
func FitTail(text string) string {
	return fitAppend(text, true)
}

func fitAppend(text string, tail bool) string {
	text = strings.TrimSpace(text)
	if text == "" || estimateTokens(text) <= appendTokenCap {
		return text
	}
	r := []rune(text)
	lo, hi := 0, len(r)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		var chunk string
		if tail {
			chunk = string(r[len(r)-mid:])
		} else {
			chunk = string(r[:mid])
		}
		if estimateTokens(chunk) <= appendTokenCap-4 {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo <= 0 {
		return ""
	}
	if tail {
		return "…" + strings.TrimSpace(string(r[len(r)-lo:]))
	}
	return strings.TrimSpace(string(r[:lo])) + "…"
}

func estimateTokens(s string) int {
	ascii := 0
	tokens := 0
	for _, r := range s {
		if r < 128 {
			ascii++
			continue
		}
		tokens += 2
	}
	tokens += (ascii + 3) / 4
	return tokens
}
