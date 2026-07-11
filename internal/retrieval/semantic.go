package retrieval

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/embed"
)

// Semantic is an embedding-backed retriever (ADR 0017): it embeds every vault
// chunk once at construction (in-memory index) and answers a query by embedding
// it and returning the chunks whose cosine similarity clears the threshold,
// ranked by similarity, capped at MaxChunks. It implements core.Retriever and
// reuses the shared vault chunking, so query- and chunk-embedding are over the
// same units.
//
// When nothing clears the threshold it returns no chunks — so the engine's
// pre-model grounding block fires. That is the point of a non-zero threshold
// (default 0.30): a threshold of 0 makes every query return its top-k and hands
// grounding to the model's scope-refusal instead (ADR 0017).
type Semantic struct {
	embedder  embed.Embedder
	maxChunks int
	threshold float32
	chunks    []core.Chunk
	vectors   [][]float32
}

// NewSemantic builds the in-memory index: it reads + chunks the vault and embeds
// every chunk. An embedding (or vault-scan) failure is returned so the caller can
// fail startup (ADR 0017) rather than serve an unindexed bot. An empty vault
// yields an empty index — every query then returns nothing (refuses).
func NewSemantic(ctx context.Context, vaultDir string, embedder embed.Embedder, maxChunks int, threshold float32) (*Semantic, error) {
	chunks, err := vaultChunks(ctx, vaultDir)
	if err != nil {
		return nil, err
	}
	limit := maxChunks
	if limit <= 0 {
		limit = defaultMaxChunks
	}
	s := &Semantic{embedder: embedder, maxChunks: limit, threshold: threshold, chunks: chunks}
	if len(chunks) > 0 {
		texts := make([]string, len(chunks))
		for i, c := range chunks {
			texts[i] = c.Text
		}
		vecs, err := embedder.Embed(ctx, texts)
		if err != nil {
			return nil, fmt.Errorf("retrieval: embed vault index: %w", err)
		}
		if len(vecs) != len(chunks) {
			return nil, fmt.Errorf("retrieval: embedded %d of %d vault chunks", len(vecs), len(chunks))
		}
		s.vectors = vecs
	}
	return s, nil
}

// Retrieve embeds the query and returns the chunks whose cosine similarity to it
// is at least the threshold, ranked by similarity (desc), capped at MaxChunks.
// Nothing clears the threshold → no chunks (the grounding block then fires). An
// empty query or an empty index returns nothing without an embedding call.
func (s *Semantic) Retrieve(ctx context.Context, query string) ([]core.Chunk, error) {
	if strings.TrimSpace(query) == "" || len(s.chunks) == 0 {
		return nil, nil
	}
	qv, err := s.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(qv) != 1 {
		return nil, fmt.Errorf("retrieval: query embed returned %d vectors", len(qv))
	}

	type ranked struct {
		idx int
		sim float32
	}
	var hits []ranked
	for i, v := range s.vectors {
		sim := cosine(qv[0], v)
		// threshold <= 0 is "no floor" (brain-judges, ADR 0017): every chunk reaches
		// the model, ranked — even a negative-cosine one — so a non-empty vault is
		// never empty. A positive floor filters normally.
		if s.threshold <= 0 || sim >= s.threshold {
			hits = append(hits, ranked{i, sim})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].sim > hits[b].sim })
	if len(hits) > s.maxChunks {
		hits = hits[:s.maxChunks]
	}

	out := make([]core.Chunk, len(hits))
	for i, h := range hits {
		out[i] = s.chunks[h.idx]
	}
	return out, nil
}

// cosine is the cosine similarity of two equal-length vectors, in float64
// internally for stability. Mismatched lengths or a zero-norm vector score 0.
func cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return float32(dot / (math.Sqrt(na) * math.Sqrt(nb)))
}

// Len is the number of indexed vault chunks — surfaced in the startup log so the
// index size (and thus the boot embedding cost) is visible (ADR 0017).
func (s *Semantic) Len() int { return len(s.chunks) }

// compile-time assertion that Semantic satisfies core.Retriever.
var _ core.Retriever = (*Semantic)(nil)
