package memory

import (
	"sort"
	"strings"
	"unicode"
)

// recencyFloor is how many of the most-recent retained turns Select always
// includes for immediate continuity, before filling the rest of the inject
// budget with the most relevant older turns (ADR 0014).
const recencyFloor = 3

// Select returns the turns to inject into a prompt for a query, or nil when the
// message is standalone (the follow-up gate), the buffer is disabled/empty, or
// injectN is non-positive. It combines a recency floor (the last few turns,
// always) with the most relevant retained turns, capped at injectN and returned
// in chronological order so the injected context stays threaded and time-ordered.
//
// Call it with the CURRENT query before appending the current turn: the buffer
// then holds only prior turns, so the recency floor is prior context, not an echo
// of the message being answered.
func (b *Buffer) Select(chatID, query string, replyToBot bool, injectN int) []Turn {
	if !b.Enabled() || injectN <= 0 {
		return nil
	}
	if !IsFollowUp(query, replyToBot) {
		return nil // standalone question — no history, pre-0014 behavior
	}

	b.mu.Lock()
	turns := append([]Turn(nil), b.convos[chatID]...)
	b.mu.Unlock()
	if len(turns) == 0 {
		return nil
	}

	chosen := make(map[int]bool, injectN)

	// Recency floor: the last few turns, always included.
	floor := recencyFloor
	if floor > injectN {
		floor = injectN
	}
	for i := len(turns) - floor; i < len(turns); i++ {
		if i >= 0 {
			chosen[i] = true
		}
	}

	// Relevance: fill the remaining budget with the highest-scoring other turns.
	if len(chosen) < injectN {
		qterms := terms(query)
		type scored struct{ idx, score int }
		var ranked []scored
		for i := range turns {
			if chosen[i] {
				continue
			}
			if s := score(turns[i].Text, qterms); s > 0 {
				ranked = append(ranked, scored{i, s})
			}
		}
		// Highest score first; ties keep buffer (chronological) order via stability.
		sort.SliceStable(ranked, func(a, c int) bool { return ranked[a].score > ranked[c].score })
		for _, r := range ranked {
			if len(chosen) >= injectN {
				break
			}
			chosen[r.idx] = true
		}
	}

	// Emit in chronological (buffer) order.
	out := make([]Turn, 0, len(chosen))
	for i := range turns {
		if chosen[i] {
			out = append(out, turns[i])
		}
	}
	return out
}

// followUpMeta are short meta-requests that only make sense against prior context.
var followUpMeta = []string{
	"tldr", "tl;dr", "summarize", "summary", "more", "continue",
	"go on", "why", "shorter", "elaborate", "expand",
}

// referentialPrefix are leading words that point back at prior context.
var referentialPrefix = []string{
	"it ", "it's ", "that ", "this ", "they ", "those ", "these ", "what about ",
}

// shortMessageMaxTokens is the word-token ceiling under which a message is taken
// as a follow-up regardless of language (ADR 0014). A one- or two-word reply
// ("yes", "بله", "why not") carries almost no standalone meaning, so against an
// existing buffer it is far more likely a continuation than a fresh question.
const shortMessageMaxTokens = 2

// IsFollowUp is the v1 heuristic (ADR 0014): does the message reference prior
// context? A reply to the bot always does; otherwise a very short message (a brief
// ack/continuation in any language), or an English meta-request or leading
// referential word. It is intentionally cheap and improvable — a miss degrades to
// an isolated answer, and a false hit costs only up to the inject budget, so no
// separate knob is warranted.
func IsFollowUp(query string, replyToBot bool) bool {
	if replyToBot {
		return true
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return false
	}
	// Language-agnostic signal: a very short message is treated as a follow-up. The
	// English cue lists below can't reach non-English acks ("بله", "آره"), but a
	// low word-token count is script-neutral, so a short Persian/Arabic/etc. reply
	// is caught the same as "yes"/"go on". (Non-space-segmented scripts collapse to
	// one token and so also lean follow-up — acceptable per the false-hit rationale
	// above; the buffer-empty guard in Select means a wrong hit injects nothing.)
	if n := len(strings.FieldsFunc(q, notWord)); n >= 1 && n <= shortMessageMaxTokens {
		return true
	}
	for _, m := range followUpMeta {
		if q == m || strings.HasPrefix(q, m+" ") || strings.HasPrefix(q, m+"?") {
			return true
		}
	}
	for _, p := range referentialPrefix {
		if strings.HasPrefix(q, p) {
			return true
		}
	}
	return false
}

// terms tokenizes a query into distinct lowercase word tokens for scoring,
// dropping trivially short tokens.
func terms(q string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(q), notWord) {
		if len(w) > 2 {
			out[w] = true
		}
	}
	return out
}

// score is a simple full-text relevance: how many distinct query terms appear in
// the turn text. It mirrors the vault's full-text approach; embeddings are
// deferred (ADR 0014).
func score(text string, qterms map[string]bool) int {
	if len(qterms) == 0 {
		return 0
	}
	words := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), notWord) {
		words[w] = true
	}
	n := 0
	for t := range qterms {
		if words[t] {
			n++
		}
	}
	return n
}

func notWord(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }
