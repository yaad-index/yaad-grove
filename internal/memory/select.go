package memory

import (
	"sort"
	"strings"
	"time"
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
// The follow-up gate is language-neutral (ADR 0014/0018): a reply is always a
// follow-up (isReply); otherwise the message is a follow-up only when its sender
// is mid-conversation — they already have a (non-bot) turn in this chat within
// window. There are no keyword or per-language heuristics. window <= 0 makes
// non-replies never qualify (replies-only mode). Once the gate passes, the
// language-agnostic token-overlap scorer picks what to inject.
//
// Call it with the CURRENT query before appending the current turn: the buffer
// then holds only prior turns, so the recency floor is prior context, not an echo
// of the message being answered.
func (b *Buffer) Select(chatID, query, senderID string, isReply bool, injectN int, window time.Duration) []Turn {
	if !b.Enabled() || injectN <= 0 {
		return nil
	}

	b.mu.Lock()
	turns := append([]Turn(nil), b.convos[chatID]...)
	b.mu.Unlock()
	if len(turns) == 0 {
		return nil
	}

	// Non-reply follow-up gate: the sender must be mid-conversation — a prior
	// (non-bot) turn of theirs in this chat, within window. A reply always passes.
	if !isReply {
		deadline := time.Now().Add(-window)
		mid := false
		for _, t := range turns {
			if !t.Bot && t.SpeakerID == senderID && t.Time.After(deadline) {
				mid = true
				break
			}
		}
		if !mid {
			return nil // standalone — no history, pre-0014 behavior
		}
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
