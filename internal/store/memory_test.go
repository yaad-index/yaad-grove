package store

import (
	"context"
	"errors"
	"sync"
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

// memWith builds a Memory with a pre-populated index snapshot (the index state
// now lives behind an atomic pointer, so tests set it via the snapshot rather than
// struct fields).
func memWith(threshold float32, chunks []core.Chunk, vectors [][]float32) *Memory {
	m := &Memory{threshold: threshold}
	m.idx.Store(&memIndex{chunks: chunks, vectors: vectors})
	return m
}

// A pre-built index: chunks c0/c1/c2 with the given vectors. Cosines to q=[1,0]:
// c0=1.0, c1≈0.707, c2=0.
func fixtureMemory(threshold float32) *Memory {
	return memWith(threshold,
		[]core.Chunk{{Source: "a", Text: "c0"}, {Source: "b", Text: "c1"}, {Source: "c", Text: "c2"}},
		[][]float32{{1, 0}, {0.7, 0.7}, {0, 1}})
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
	// c1(0.707), c2(0) both below the 0.9 floor (c0 dropped).
	m := memWith(0.9,
		[]core.Chunk{{Source: "b", Text: "c1"}, {Source: "c", Text: "c2"}},
		[][]float32{{0.7, 0.7}, {0, 1}})
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
	m := memWith(0,
		[]core.Chunk{{Source: "p", Text: "pos"}, {Source: "n", Text: "neg"}},
		[][]float32{{1, 0}, {-1, 0}}) // cosines to q=[1,0]: +1 and -1
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
	m := memWith(0, []core.Chunk{
		{Source: "a", Text: "install install install"},
		{Source: "b", Text: "install once"},
		{Source: "c", Text: "unrelated"},
	}, nil)
	got, err := m.Keyword(context.Background(), "install", 8)
	require.NoError(t, err)
	require.Len(t, got, 2, "only matching chunks")
	assert.Equal(t, "a", got[0].Source, "more mentions ranks higher")
	assert.Equal(t, "b", got[1].Source)
}

// k caps the keyword result.
func TestMemoryKeywordCap(t *testing.T) {
	m := memWith(0, []core.Chunk{
		{Source: "a", Text: "install install"},
		{Source: "b", Text: "install"},
	}, nil)
	got, err := m.Keyword(context.Background(), "install", 1)
	require.NoError(t, err)
	assert.Len(t, got, 1)
}

// Ties break by source path then index order, so output is deterministic (no
// map-order leak) for a given corpus+query.
func TestMemoryKeywordDeterministicTieBreak(t *testing.T) {
	m := memWith(0, []core.Chunk{
		{Source: "z", Text: "widget"},
		{Source: "a", Text: "widget"},
		{Source: "m", Text: "widget"},
	}, nil)
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
	m := memWith(0, []core.Chunk{{Source: "a", Text: "install"}}, nil)
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

	mi := m.load()
	assert.Equal(t, 3, m.Len(), "chunks flattened in doc order")
	assert.Len(t, mi.vectors, 3, "every chunk embedded")
	assert.Equal(t, "hello", mi.chunks[0].Text)
	assert.Equal(t, "again", mi.chunks[2].Text)
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
	assert.Nil(t, m.load().vectors, "no embedder → no vectors")

	sem, err := m.Semantic(context.Background(), []float32{1, 0}, 8)
	require.NoError(t, err)
	assert.Empty(t, sem, "unembedded index → no semantic hits")

	kw, err := m.Keyword(context.Background(), "hello", 8)
	require.NoError(t, err)
	assert.Len(t, kw, 1, "keyword still works without embeddings")
}

// Enumerate returns the complete set of docs carrying a dimension value, deduped
// and in doc order; an undeclared dimension or an unmatched value is empty.
func TestMemoryEnumerateCompleteSet(t *testing.T) {
	m := NewMemory(nil, 0)
	require.NoError(t, m.Index(context.Background(), []Doc{
		{Ref: DocRef{Path: "ep01.md", Title: "Ep 1"}, Dimensions: map[string][]string{"games": {"Acme Rail"}}},
		{Ref: DocRef{Path: "ep02.md", Title: "Ep 2"}, Dimensions: map[string][]string{"games": {"Acme Rail", "Widget Wars"}}},
		{Ref: DocRef{Path: "ep03.md", Title: "Ep 3"}, Dimensions: map[string][]string{"games": {"Widget Wars"}}},
	}))

	got, err := m.Enumerate(context.Background(), "games", "Acme Rail")
	require.NoError(t, err)
	assert.Equal(t, []string{"ep01.md", "ep02.md"}, paths(got), "complete set, doc order")

	empty, err := m.Enumerate(context.Background(), "games", "Nonexistent")
	require.NoError(t, err)
	assert.Empty(t, empty, "an unmatched value is an empty set, not an error")

	undeclared, err := m.Enumerate(context.Background(), "designers", "anyone")
	require.NoError(t, err)
	assert.Empty(t, undeclared, "an undeclared dimension is empty, not an error")
}

// Enumerate matches spelling- and script-insensitively: the query value normalizes
// the same as the indexed value, so casing/hyphen/format drift can't drop a doc.
func TestMemoryEnumerateNormalizedMatch(t *testing.T) {
	m := NewMemory(nil, 0)
	require.NoError(t, m.Index(context.Background(), []Doc{
		{Ref: DocRef{Path: "a.md"}, Dimensions: map[string][]string{"games": {"Acme Rail"}}},
	}))
	got, err := m.Enumerate(context.Background(), "games", "  acme-rail  ")
	require.NoError(t, err)
	assert.Equal(t, []string{"a.md"}, paths(got), "normalized query matches the indexed value")
}

// Enumerate resolves an alias surface form to its entity: a note declares aliases
// against its own title, and those forms resolve to docs referencing the title in
// their dimension lists.
func TestMemoryEnumerateAliasResolution(t *testing.T) {
	m := NewMemory(nil, 0)
	require.NoError(t, m.Index(context.Background(), []Doc{
		// The entity note: its title is the canonical name, with a cross-script alias.
		{Ref: DocRef{Path: "acme-rail.md", Title: "Acme Rail"}, Aliases: []string{"اکمی ریل"}},
		// Docs that reference the entity by its canonical name in a dimension.
		{Ref: DocRef{Path: "ep01.md"}, Dimensions: map[string][]string{"games": {"Acme Rail"}}},
		{Ref: DocRef{Path: "ep02.md"}, Dimensions: map[string][]string{"games": {"Acme Rail"}}},
	}))

	viaCanonical, err := m.Enumerate(context.Background(), "games", "Acme Rail")
	require.NoError(t, err)
	assert.Equal(t, []string{"ep01.md", "ep02.md"}, paths(viaCanonical))

	viaAlias, err := m.Enumerate(context.Background(), "games", "اکمی ریل")
	require.NoError(t, err)
	assert.Equal(t, []string{"ep01.md", "ep02.md"}, paths(viaAlias), "the alias resolves to the same complete set")
}

// A live reindex is safe against concurrent queries (#86): Index rebuilds a fresh
// snapshot and swaps it atomically, so a reader always sees one consistent index
// (the old one or the new one), never a torn mix. The value of this test is under
// -race — a query reading a field mid-rebuild would trip the detector.
func TestMemoryConcurrentReindexAndQuery(t *testing.T) {
	m := NewMemory(nil, 0)
	doc := func(text string) []Doc {
		return []Doc{{
			Ref:        DocRef{Path: "a.md", Title: "Acme"},
			Chunks:     []core.Chunk{{Source: "a.md", Text: text}},
			Dimensions: map[string][]string{"games": {"Acme"}},
		}}
	}
	require.NoError(t, m.Index(context.Background(), doc("install widget")))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ { // concurrent readers across all three query paths
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = m.Keyword(context.Background(), "install", 8)
				_, _ = m.Semantic(context.Background(), []float32{1, 0}, 8)
				_, _ = m.Enumerate(context.Background(), "games", "Acme")
			}
		}()
	}

	done := make(chan struct{})
	go func() { // reindexer: 100 live rebuilds while the readers run
		defer close(done)
		for i := 0; i < 100; i++ {
			_ = m.Index(context.Background(), doc("install widget reset"))
		}
	}()
	<-done
	close(stop)
	wg.Wait()
	assert.Equal(t, 1, m.Len(), "the last reindex is the live snapshot")
}

// Close is a no-op for the memory backend.
func TestMemoryClose(t *testing.T) {
	assert.NoError(t, NewMemory(nil, 0).Close())
}

// paths projects the DocRef paths, for order-sensitive assertions.
func paths(refs []DocRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Path
	}
	return out
}
