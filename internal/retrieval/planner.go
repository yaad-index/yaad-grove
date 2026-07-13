package retrieval

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/embed"
	"github.com/yaad-index/yaad-grove/internal/store"
)

// Retrieval modes (ADR 0001/0017/0019): how the query step combines the Store's
// primitives. keyword is lexical only; semantic is embeddings with keyword as an
// error-fallback (the pre-hybrid default); hybrid fuses both every query.
const (
	ModeKeyword  = "keyword"
	ModeSemantic = "semantic"
	ModeHybrid   = "hybrid"
)

// defaultMaxChunks caps a retrieval when maxChunks is unset (<= 0): retrieval
// feeds a bounded prompt, so it is capped rather than unbounded by default.
const defaultMaxChunks = 8

// rrfK is the Reciprocal Rank Fusion damping constant. 60 is the value from the
// original RRF paper and the de-facto default: large enough that a document's
// contribution decays gently with rank (so a solid top-10 hit still matters), and
// it bounds any single leg's influence so no one retriever dominates the fusion.
const rrfK = 60

// Planner is the engine-layer query step (ADR 0019): it implements core.Retriever
// over a Store, owning the two things that are NOT a backend's business — query
// embedding (done once, the vector passed to Store.Semantic) and rank fusion. The
// Store exposes only primitives; the Planner composes them per mode:
//
//   - keyword:  Store.Keyword only.
//   - semantic: Store.Semantic, degrading to Store.Keyword on an embed/semantic
//     error (an empty semantic result is a valid refusal, NOT a fallback trigger).
//   - hybrid:   both legs every query, fused with RRF; a leg that errors is
//     skipped and the rest fuse, only all-legs-error fails, and an empty-but-ok
//     leg flows through fusion unchanged (a lexical-only hit still surfaces).
type Planner struct {
	store     store.Store
	embedder  embed.Embedder
	mode      string
	maxChunks int
}

// NewPlanner builds the query step over st for the given mode. embedder may be nil
// only in keyword mode (semantic/hybrid embed the query). maxChunks <= 0 uses the
// default cap.
func NewPlanner(st store.Store, embedder embed.Embedder, mode string, maxChunks int) *Planner {
	if maxChunks <= 0 {
		maxChunks = defaultMaxChunks
	}
	return &Planner{store: st, embedder: embedder, mode: mode, maxChunks: maxChunks}
}

// Mode reports the configured retrieval mode (keyword / semantic / hybrid).
func (p *Planner) Mode() string { return p.mode }

// Retrieve returns the ranked chunks to ground on for query, per the configured
// mode. An empty result is a valid "nothing relevant" (the engine's grounding
// block then fires); an error is a genuine retrieval failure.
func (p *Planner) Retrieve(ctx context.Context, query string) ([]core.Chunk, error) {
	switch p.mode {
	case ModeKeyword:
		return p.store.Keyword(ctx, query, p.maxChunks)
	case ModeSemantic:
		return p.semanticWithFallback(ctx, query)
	case ModeHybrid:
		return p.hybrid(ctx, query)
	default:
		return nil, fmt.Errorf("retrieval: unknown mode %q", p.mode)
	}
}

// semanticWithFallback runs the semantic leg and, only on its ERROR (a dead embed
// endpoint or a store failure), degrades to keyword — the pre-hybrid default. An
// empty semantic result is a valid refusal and is returned as-is, so the grounding
// block still fires rather than silently widening to a lexical match.
func (p *Planner) semanticWithFallback(ctx context.Context, query string) ([]core.Chunk, error) {
	chunks, err := p.semanticLeg(ctx, query)
	if err != nil {
		slog.Warn("retrieval: semantic leg failed; falling back to keyword", "err", err)
		return p.store.Keyword(ctx, query, p.maxChunks)
	}
	return chunks, nil
}

// hybrid fuses the keyword and semantic legs with RRF, in that leg order (so the
// fusion tie-break is deterministic). A leg that errors is logged and skipped; the
// remaining legs fuse. Only when EVERY leg errors does it fail — a silent empty
// would be misread by the engine as a legitimate refusal. An empty-but-ok leg
// contributes nothing yet counts as consulted, so a lexical-only hit surfaces even
// when the semantic leg cleared nothing.
func (p *Planner) hybrid(ctx context.Context, query string) ([]core.Chunk, error) {
	var legs [][]core.Chunk
	var anyOK bool
	var lastErr error

	if kw, err := p.store.Keyword(ctx, query, p.maxChunks); err != nil {
		slog.Warn("retrieval: keyword leg failed; fusing the remaining legs", "err", err)
		lastErr = err
	} else {
		anyOK = true
		legs = append(legs, kw)
	}
	if sem, err := p.semanticLeg(ctx, query); err != nil {
		slog.Warn("retrieval: semantic leg failed; fusing the remaining legs", "err", err)
		lastErr = err
	} else {
		anyOK = true
		legs = append(legs, sem)
	}

	if !anyOK {
		// Every leg errored — surface it rather than a silent empty.
		return nil, lastErr
	}
	return rrfFuse(legs, p.maxChunks), nil
}

// semanticLeg embeds the query once and returns Store.Semantic's ranked chunks. An
// empty query (or a keyword-only store) returns nothing WITHOUT an embed call, so
// this stays a no-op where the pre-store semantic retriever was. An embed failure
// is returned so the caller can decide to fall back (semantic mode) or skip the
// leg (hybrid mode).
func (p *Planner) semanticLeg(ctx context.Context, query string) ([]core.Chunk, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	qv, err := p.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(qv) != 1 {
		return nil, fmt.Errorf("retrieval: query embed returned %d vectors", len(qv))
	}
	return p.store.Semantic(ctx, qv[0], p.maxChunks)
}

// rrfFuse merges the legs' ranked lists with Reciprocal Rank Fusion: a chunk's
// fused score is the sum, across legs that returned it, of 1/(rrfK + rank). Chunks
// are deduped by source+text, sorted by fused score (ties broken by first-seen
// order for determinism), and capped at k.
func rrfFuse(legs [][]core.Chunk, k int) []core.Chunk {
	type agg struct {
		chunk core.Chunk
		score float64
		order int // first-seen order, for a deterministic tie-break
	}
	byKey := map[string]*agg{}
	var seen int
	for _, chunks := range legs {
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
	if k > 0 && len(fused) > k {
		fused = fused[:k]
	}
	out := make([]core.Chunk, len(fused))
	for i, a := range fused {
		out[i] = a.chunk
	}
	return out
}

// compile-time assertion that Planner satisfies core.Retriever.
var _ core.Retriever = (*Planner)(nil)
