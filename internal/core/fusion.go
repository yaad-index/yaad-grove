package core

import "sort"

// rrfK is the Reciprocal Rank Fusion damping constant. 60 is the value from the
// original RRF paper and the de-facto default: large enough that a document's
// contribution decays gently with rank (so a solid top-10 hit still matters), and
// it bounds any single list's influence so no one retriever dominates the fusion.
const rrfK = 60

// FuseRanked merges ranked chunk lists with Reciprocal Rank Fusion: a chunk's
// fused score is the sum, across the lists that returned it, of 1/(rrfK + rank),
// rank being 0-based. Chunks are deduped by source+text, sorted by fused score
// (ties broken by first-seen order, so an earlier list wins, for determinism),
// and capped at k when k > 0. The planner's hybrid legs and the engine's
// history-blended retrieval (#250) both fuse through it.
func FuseRanked(lists [][]Chunk, k int) []Chunk {
	type agg struct {
		chunk Chunk
		score float64
		order int // first-seen order, for a deterministic tie-break
	}
	byKey := map[string]*agg{}
	var seen int
	for _, chunks := range lists {
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
	out := make([]Chunk, len(fused))
	for i, a := range fused {
		out[i] = a.chunk
	}
	return out
}
