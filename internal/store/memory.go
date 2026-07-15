package store

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync/atomic"
	"unicode"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/embed"
)

// Memory is the default in-process backend (ADR 0019): it holds the vault's chunks
// and their embeddings in RAM, rebuilt in full on every Index. It is pure Go, zero
// config, and reproduces the pre-store retrieval behavior exactly — the semantic
// leg is cosine similarity over an in-memory index with a floor, the keyword leg is
// deterministic term-frequency scoring over the same chunks.
//
// The similarity floor lives here, not in the query step: RRF fuses on rank, not
// score, so the floor was never fusion's concern, and keeping it in the backend
// lets a server-side backend apply it natively without leaking scores upward.
//
// The index state lives behind an atomic pointer so a live reindex (#86) can
// rebuild it and swap it in a single atomic store: a concurrent query always reads
// one consistent snapshot (the old index or the new one, never a torn mix) with no
// lock on the read path.
type Memory struct {
	embedder  embed.Embedder
	threshold float32
	idx       atomic.Pointer[memIndex]
}

// memIndex is one immutable snapshot of the built index — replaced wholesale by
// Index, never mutated in place, so readers need no lock.
type memIndex struct {
	chunks  []core.Chunk
	vectors [][]float32
	// dimIndex is the structured-lookup index (ADR 0019): dimension → normalized
	// value key → the complete set of documents carrying that value, in doc order.
	dimIndex map[string]map[string][]DocRef
	// aliasMap resolves a normalized surface form (a transliteration / cross-script
	// spelling) to the normalized canonical key of the entity it names.
	aliasMap map[string]string
}

// NewMemory builds an empty memory backend. embedder may be nil (keyword-only
// deployments): Index then skips embedding and Semantic returns nothing. threshold
// is the semantic similarity floor (<= 0 means no floor — every chunk reaches the
// model, ranked; ADR 0017).
func NewMemory(embedder embed.Embedder, threshold float32) *Memory {
	return &Memory{embedder: embedder, threshold: threshold}
}

// load returns the current index snapshot, or an empty one before the first Index.
func (m *Memory) load() *memIndex {
	if mi := m.idx.Load(); mi != nil {
		return mi
	}
	return &memIndex{}
}

// Index (re)builds the in-memory index from docs: it flattens their chunks in
// order and, when an embedder is configured, embeds every chunk; it also builds
// the structured-lookup index (normalized dimension values → docs) and the alias
// map. An embedding failure is returned so the caller can fail startup rather than
// serve an unindexed bot (ADR 0017). Re-indexing replaces the prior contents.
func (m *Memory) Index(ctx context.Context, docs []Doc) error {
	var chunks []core.Chunk
	for _, d := range docs {
		chunks = append(chunks, d.Chunks...)
	}
	vectors, err := m.embedChunks(ctx, chunks)
	if err != nil {
		// A reindex that fails to embed leaves the current index in place (the swap
		// never happens), so the bot keeps serving the last good snapshot.
		return err
	}
	dimIndex, aliasMap := buildStructured(docs)
	m.idx.Store(&memIndex{chunks: chunks, vectors: vectors, dimIndex: dimIndex, aliasMap: aliasMap})
	return nil
}

// buildStructured builds the dimension index and alias map (ADR 0019). Every
// dimension value and every alias surface form is stored under its normalized key,
// so lookup is spelling- and script-insensitive. A document is recorded once per
// distinct normalized value (so a note repeating a value doesn't duplicate), and
// documents accumulate in doc order for a deterministic complete set.
func buildStructured(docs []Doc) (map[string]map[string][]DocRef, map[string]string) {
	dimIndex := map[string]map[string][]DocRef{}
	aliasMap := map[string]string{}
	for _, d := range docs {
		// Alias registration: each surface form → this note's canonical key. The
		// canonical is the note's title normalized (the KB contract, see Doc).
		if canon := normalizeKey(d.Ref.Title); canon != "" {
			for _, a := range d.Aliases {
				if ak := normalizeKey(a); ak != "" {
					aliasMap[ak] = canon
				}
			}
		}
		// Dimension index: each declared value → this doc, deduped within the doc.
		for dim, values := range d.Dimensions {
			byKey := dimIndex[dim]
			if byKey == nil {
				byKey = map[string][]DocRef{}
				dimIndex[dim] = byKey
			}
			seen := map[string]bool{}
			for _, v := range values {
				vk := normalizeKey(v)
				if vk == "" || seen[vk] {
					continue
				}
				seen[vk] = true
				byKey[vk] = append(byKey[vk], d.Ref)
			}
		}
	}
	return dimIndex, aliasMap
}

// embedChunks embeds every chunk's text, or returns nil vectors when no embedder
// is configured or there is nothing to embed. This is the seam the content-hash
// embedding cache slots into (ADR 0019): a persistent backend keys each chunk's
// text hash to a stored vector and only embeds the misses. The memory backend
// embeds them all, every boot.
func (m *Memory) embedChunks(ctx context.Context, chunks []core.Chunk) ([][]float32, error) {
	if m.embedder == nil || len(chunks) == 0 {
		return nil, nil
	}
	texts := make([]string, len(chunks))
	for i, c := range chunks {
		texts[i] = c.Text
	}
	vecs, err := m.embedder.Embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("store: embed vault index: %w", err)
	}
	if len(vecs) != len(chunks) {
		return nil, fmt.Errorf("store: embedded %d of %d vault chunks", len(vecs), len(chunks))
	}
	return vecs, nil
}

// Semantic returns up to k chunks whose cosine similarity to queryEmbedding clears
// the floor, ranked by similarity (desc). threshold <= 0 is "no floor" — every
// chunk reaches the model, ranked, even a negative-cosine one — so a non-empty
// index is never empty (ADR 0017). An empty embedding or an unembedded index
// returns nothing.
func (m *Memory) Semantic(_ context.Context, queryEmbedding []float32, k int) ([]core.Chunk, error) {
	mi := m.load()
	if len(queryEmbedding) == 0 || len(mi.vectors) == 0 {
		return nil, nil
	}
	type ranked struct {
		idx int
		sim float32
	}
	var hits []ranked
	for i, v := range mi.vectors {
		sim := cosine(queryEmbedding, v)
		if m.threshold <= 0 || sim >= m.threshold {
			hits = append(hits, ranked{i, sim})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].sim > hits[b].sim })
	if k > 0 && len(hits) > k {
		hits = hits[:k]
	}
	out := make([]core.Chunk, len(hits))
	for i, h := range hits {
		out[i] = mi.chunks[h.idx]
	}
	return out, nil
}

// Keyword returns up to k chunks ranked by the summed case-insensitive frequency
// of the query terms, ties broken by source path then index order so output is
// reproducible for a given corpus+query. An empty query, no matches, or an empty
// index returns nothing.
func (m *Memory) Keyword(_ context.Context, query string, k int) ([]core.Chunk, error) {
	queryTerms := tokenize(query)
	var scored []scoredChunk
	for order, c := range m.load().chunks {
		if s := score(queryTerms, c.Text); s > 0 {
			scored = append(scored, scoredChunk{chunk: c, score: s, order: order})
		}
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		if scored[i].chunk.Source != scored[j].chunk.Source {
			return scored[i].chunk.Source < scored[j].chunk.Source
		}
		return scored[i].order < scored[j].order
	})
	if k > 0 && len(scored) > k {
		scored = scored[:k]
	}
	out := make([]core.Chunk, len(scored))
	for i, sc := range scored {
		out[i] = sc.chunk
	}
	return out, nil
}

// Enumerate returns EVERY document whose dimension carries value — the complete
// authoritative set, not top-k (ADR 0019). value is normalized and resolved
// through the alias map (so a cross-script or aliased spelling reaches the same
// entity), then looked up in the dimension index. An undeclared dimension or an
// unmatched value returns an empty set, not an error. The result is a copy in
// stable doc order, so callers can't mutate the index.
func (m *Memory) Enumerate(_ context.Context, dimension, value string) ([]DocRef, error) {
	mi := m.load()
	byKey := mi.dimIndex[dimension]
	if byKey == nil {
		return nil, nil
	}
	key := normalizeKey(value)
	if canon, ok := mi.aliasMap[key]; ok {
		key = canon
	}
	refs := byKey[key]
	out := make([]DocRef, len(refs))
	copy(out, refs)
	return out, nil
}

// Close releases resources; the memory backend holds none.
func (m *Memory) Close() error { return nil }

// Len is the number of indexed chunks — surfaced in the startup log so the index
// size (and thus the boot embedding cost) is visible (ADR 0017).
func (m *Memory) Len() int { return len(m.load().chunks) }

// scoredChunk pairs a chunk with its keyword match score and index order, for a
// stable, deterministic tie-break.
type scoredChunk struct {
	chunk core.Chunk
	score int
	order int
}

// tokenize lowercases and splits on non-alphanumeric runes.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// score sums, over the query terms, how often each appears in text (case-
// insensitive term frequency). Zero query terms scores zero.
func score(queryTerms []string, text string) int {
	if len(queryTerms) == 0 {
		return 0
	}
	freq := map[string]int{}
	for _, tok := range tokenize(text) {
		freq[tok]++
	}
	total := 0
	for _, q := range queryTerms {
		total += freq[q]
	}
	return total
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

// compile-time assertion that Memory satisfies the Store port.
var _ Store = (*Memory)(nil)
