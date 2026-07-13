package store

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// fakeEmb maps text -> vector deterministically and counts calls, so tests need
// no live endpoint.
type fakeEmb struct {
	byText map[string][]float32
	err    error
	calls  int
}

func (f *fakeEmb) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = f.byText[t]
	}
	return out, nil
}

// A pre-built index: chunks c0/c1/c2 with the given vectors. Cosines to q=[1,0]:
// c0=1.0, c1≈0.707, c2=0.
func fixtureMemory(threshold float32) *Memory {
	return &Memory{
		threshold: threshold,
		chunks:    []core.Chunk{{Source: "a", Text: "c0"}, {Source: "b", Text: "c1"}, {Source: "c", Text: "c2"}},
		vectors:   [][]float32{{1, 0}, {0.7, 0.7}, {0, 1}},
	}
}

// Above-floor chunks come back ranked by similarity; below-floor are dropped.
func TestMemorySemanticRanksAndFilters(t *testing.T) {
	m := fixtureMemory(0.3)
	got, err := m.Semantic(context.Background(), []float32{1, 0}, 8)
	require.NoError(t, err)
	require.Len(t, got, 2, "c2 (sim 0) is below the 0.3 floor")
	assert.Equal(t, "c0", got[0].Text, "highest similarity first")
	assert.Equal(t, "c1", got[1].Text)
}

// Nothing above the floor → empty, so the pre-model grounding block fires.
func TestMemorySemanticEmptyBelowThreshold(t *testing.T) {
	m := fixtureMemory(0.9) // only c0 (1.0) would clear 0.9
	m.chunks = m.chunks[1:] // drop c0; c1(0.707), c2(0) both below 0.9
	m.vectors = m.vectors[1:]
	got, err := m.Semantic(context.Background(), []float32{1, 0}, 8)
	require.NoError(t, err)
	assert.Empty(t, got, "nothing clears the floor → empty (grounding block fires)")
}

// A zero floor returns the top-k regardless (brain-judges mode).
func TestMemorySemanticThresholdZeroReturnsTopK(t *testing.T) {
	m := fixtureMemory(0)
	got, err := m.Semantic(context.Background(), []float32{1, 0}, 8)
	require.NoError(t, err)
	assert.Len(t, got, 3, "no floor → all chunks reach the model, ranked")
	assert.Equal(t, "c0", got[0].Text)
}

// threshold=0 is "no floor": even a chunk with negative cosine is returned, so a
// non-empty index is never empty in brain-judges mode (ADR 0017 contract).
func TestMemorySemanticThresholdZeroIncludesNegativeSim(t *testing.T) {
	m := &Memory{
		threshold: 0,
		chunks:    []core.Chunk{{Source: "p", Text: "pos"}, {Source: "n", Text: "neg"}},
		vectors:   [][]float32{{1, 0}, {-1, 0}}, // cosines to q=[1,0]: +1 and -1
	}
	got, err := m.Semantic(context.Background(), []float32{1, 0}, 8)
	require.NoError(t, err)
	require.Len(t, got, 2, "no floor returns even the negative-cosine chunk")
	assert.Equal(t, "pos", got[0].Text, "still ranked by similarity")
	assert.Equal(t, "neg", got[1].Text)
}

// k caps the result even when more clear the floor.
func TestMemorySemanticCap(t *testing.T) {
	m := fixtureMemory(0)
	got, err := m.Semantic(context.Background(), []float32{1, 0}, 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "c0", got[0].Text, "the single kept chunk is the top-ranked")
}

// An empty query embedding or an unembedded index returns nothing.
func TestMemorySemanticNoOp(t *testing.T) {
	m := fixtureMemory(0.3)
	got, err := m.Semantic(context.Background(), nil, 8)
	require.NoError(t, err)
	assert.Empty(t, got, "empty query embedding → nothing")

	empty := &Memory{threshold: 0.3}
	got, err = empty.Semantic(context.Background(), []float32{1, 0}, 8)
	require.NoError(t, err)
	assert.Empty(t, got, "unembedded index → nothing")
}

// Keyword ranks by term frequency: a chunk with more mentions outranks one with
// fewer; a non-matching chunk is dropped.
func TestMemoryKeywordRanks(t *testing.T) {
	m := &Memory{chunks: []core.Chunk{
		{Source: "a", Text: "install install install"},
		{Source: "b", Text: "install once"},
		{Source: "c", Text: "unrelated"},
	}}
	got, err := m.Keyword(context.Background(), "install", 8)
	require.NoError(t, err)
	require.Len(t, got, 2, "only matching chunks")
	assert.Equal(t, "a", got[0].Source, "more mentions ranks higher")
	assert.Equal(t, "b", got[1].Source)
}

// k caps the keyword result.
func TestMemoryKeywordCap(t *testing.T) {
	m := &Memory{chunks: []core.Chunk{
		{Source: "a", Text: "install install"},
		{Source: "b", Text: "install"},
	}}
	got, err := m.Keyword(context.Background(), "install", 1)
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

// Ties break by source path then index order, so output is deterministic (no
// map-order leak) for a given corpus+query.
func TestMemoryKeywordDeterministicTieBreak(t *testing.T) {
	m := &Memory{chunks: []core.Chunk{
		{Source: "z", Text: "widget"},
		{Source: "a", Text: "widget"},
		{Source: "m", Text: "widget"},
	}}
	got, err := m.Keyword(context.Background(), "widget", 8)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, []string{"a", "m", "z"}, []string{got[0].Source, got[1].Source, got[2].Source}, "equal scores tie-break by source path")

	again, err := m.Keyword(context.Background(), "widget", 8)
	require.NoError(t, err)
	assert.Equal(t, got, again, "deterministic across calls")
}

// An empty query and a no-match query both return nothing without error.
func TestMemoryKeywordEmptyAndNoMatch(t *testing.T) {
	m := &Memory{chunks: []core.Chunk{{Source: "a", Text: "install"}}}
	for _, q := range []string{"", "   ", "nonexistentterm12345"} {
		got, err := m.Keyword(context.Background(), q, 8)
		require.NoError(t, err, "query %q", q)
		assert.Empty(t, got, "query %q", q)
	}
}

// Index flattens the docs' chunks in order and embeds every one; the indexed set
// matches the input, and re-indexing replaces the prior contents.
func TestMemoryIndexBuildsAndEmbeds(t *testing.T) {
	docs := []Doc{
		{Ref: DocRef{Path: "a.md"}, Chunks: []core.Chunk{{Source: "a.md#i", Text: "hello"}, {Source: "a.md#s", Text: "world"}}},
		{Ref: DocRef{Path: "b.md"}, Chunks: []core.Chunk{{Source: "b.md", Text: "again"}}},
	}
	fe := &fakeEmb{byText: map[string][]float32{"hello": {1, 0}, "world": {0, 1}, "again": {1, 1}}}
	m := NewMemory(fe, 0.3)
	require.NoError(t, m.Index(context.Background(), docs))

	assert.Equal(t, 3, m.Len(), "chunks flattened in doc order")
	assert.Len(t, m.vectors, 3, "every chunk embedded")
	assert.Equal(t, "hello", m.chunks[0].Text)
	assert.Equal(t, "again", m.chunks[2].Text)
	assert.Equal(t, 1, fe.calls, "one batch embed call")
}

// A build-time embedding failure is returned so the caller can fail startup.
func TestMemoryIndexEmbedFailureIsFatal(t *testing.T) {
	m := NewMemory(&fakeEmb{err: errors.New("endpoint down")}, 0.3)
	err := m.Index(context.Background(), []Doc{{Chunks: []core.Chunk{{Text: "x"}}}})
	assert.Error(t, err, "an index-build embedding failure is fatal")
}

// A nil embedder (keyword-only deployment) indexes chunks but no vectors, so
// Semantic returns nothing while Keyword still works.
func TestMemoryIndexNoEmbedder(t *testing.T) {
	m := NewMemory(nil, 0.3)
	require.NoError(t, m.Index(context.Background(), []Doc{{Chunks: []core.Chunk{{Source: "a", Text: "hello"}}}}))
	assert.Equal(t, 1, m.Len())
	assert.Nil(t, m.vectors, "no embedder → no vectors")

	sem, err := m.Semantic(context.Background(), []float32{1, 0}, 8)
	require.NoError(t, err)
	assert.Empty(t, sem, "unembedded index → no semantic hits")

	kw, err := m.Keyword(context.Background(), "hello", 8)
	require.NoError(t, err)
	assert.Len(t, kw, 1, "keyword still works without embeddings")
}

// Enumerate is not implemented this increment and fails loudly.
func TestMemoryEnumerateNotImplemented(t *testing.T) {
	m := NewMemory(nil, 0)
	_, err := m.Enumerate(context.Background(), "games", "acme-rail-game")
	assert.ErrorIs(t, err, ErrEnumerateNotImplemented)
}

// Close is a no-op for the memory backend.
func TestMemoryClose(t *testing.T) {
	assert.NoError(t, NewMemory(nil, 0).Close())
}
