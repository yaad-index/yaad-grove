package retrieval

import (
	"context"
	"log/slog"
	"sort"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// rrfK is the Reciprocal Rank Fusion damping constant. 60 is the value from the
// original RRF paper and the de-facto default: large enough that a document's
// contribution decays gently with rank (so a solid top-10 hit still matters), and
// it bounds any single leg's influence so no one retriever dominates the fusion.
const rrfK = 60

// Hybrid fuses several retrievers into one (issue #65, increment A). It runs every
// leg for a query and merges their ranked results with Reciprocal Rank Fusion:
// a chunk's fused score is the sum, across legs that returned it, of 1/(rrfK +
// rank). Chunks are deduped, sorted by fused score, and capped at MaxChunks.
//
// Why fusion and not the Fallback: Fallback consults its secondary only when the
// primary ERRORS, so a semantic leg that returns empty (nothing cleared the
// cosine floor) never reaches keyword — and an exact-name / "which episode" query,
// semantic's blind spot, gets refused even though the lexical leg would match it
// trivially. Hybrid runs both legs every time, so a lexical-only hit surfaces even
// when the semantic leg is empty. The semantic leg's similarity floor still gates
// that leg (it returns pre-filtered), but lexical hits are not floor-gated.
//
// It is resilient: a leg that errors (e.g. an embedding-endpoint blip) is logged
// and skipped, and fusion proceeds over the legs that succeeded; only if EVERY
// leg errors does Retrieve return an error. All legs returning empty (no error) is
// a valid "nothing relevant" — Hybrid returns no chunks, and the engine's
// grounding block fires, exactly as a single retriever would.
type Hybrid struct {
	// Legs are the retrievers to fuse, in priority order — the order is used only to
	// break exact score ties deterministically (earlier leg wins).
	Legs []core.Retriever
	// MaxChunks caps the fused result; <= 0 uses defaultMaxChunks.
	MaxChunks int
}

// NewHybrid builds a Hybrid over legs, fused by RRF and capped at maxChunks.
func NewHybrid(maxChunks int, legs ...core.Retriever) Hybrid {
	return Hybrid{Legs: legs, MaxChunks: maxChunks}
}

// Retrieve fuses every leg's ranked results for query with RRF and returns the
// top chunks (see the type doc for the error/empty semantics).
func (h Hybrid) Retrieve(ctx context.Context, query string) ([]core.Chunk, error) {
	type agg struct {
		chunk core.Chunk
		score float64
		order int // first-seen order, for a deterministic tie-break
	}
	byKey := map[string]*agg{}
	var seen int
	var anyOK bool
	var lastErr error

	for _, leg := range h.Legs {
		chunks, err := leg.Retrieve(ctx, query)
		if err != nil {
			// A single leg failing must not fail the whole query — fuse the rest.
			slog.Warn("hybrid: retriever leg failed; fusing the remaining legs", "err", err)
			lastErr = err
			continue
		}
		anyOK = true
		for rank, c := range chunks {
			key := c.Source + "\x00" + c.Text
			a := byKey[key]
			if a == nil {
				a = &agg{chunk: c, order: seen}
				byKey[key] = a
				seen++
			}
			a.score += 1.0 / float64(rrfK+rank) // rank is 0-based
		}
	}

	if !anyOK {
		// Every leg errored — surface it rather than a silent empty (which the engine
		// would read as a legitimate refusal).
		return nil, lastErr
	}

	fused := make([]*agg, 0, len(byKey))
	for _, a := range byKey {
		fused = append(fused, a)
	}
	sort.SliceStable(fused, func(i, j int) bool {
		if fused[i].score != fused[j].score {
			return fused[i].score > fused[j].score
		}
		return fused[i].order < fused[j].order
	})

	limit := h.MaxChunks
	if limit <= 0 {
		limit = defaultMaxChunks
	}
	if len(fused) > limit {
		fused = fused[:limit]
	}
	out := make([]core.Chunk, len(fused))
	for i, a := range fused {
		out[i] = a.chunk
	}
	return out, nil
}

// compile-time assertion that Hybrid satisfies core.Retriever.
var _ core.Retriever = Hybrid{}
