package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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

// Dimensions surfaces each declared dimension's distinct values by display form,
// sorted, with the first-seen raw spelling as the display (ADR 0020) — the
// vocabulary kb_dimensions serves so the model can pick a value that exists.
func TestMemoryDimensions(t *testing.T) {
	m := NewMemory(nil, 0)
	require.NoError(t, m.Index(context.Background(), []Doc{
		{Ref: DocRef{Path: "a.md"}, Dimensions: map[string][]string{"games": {"Acme Rail"}, "category": {"Trains"}}},
		// "acme-rail" folds to the same key as "Acme Rail"; the first-seen display wins.
		{Ref: DocRef{Path: "b.md"}, Dimensions: map[string][]string{"games": {"acme-rail", "Widget Wars"}}},
	}))

	dims, err := m.Dimensions(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"Acme Rail", "Widget Wars"}, dims["games"], "distinct display values, sorted, first-seen spelling")
	assert.Equal(t, []string{"Trains"}, dims["category"])
	assert.NotContains(t, dims, "publishers", "only indexed dimensions appear")

	// Empty before the first index.
	assert.Empty(t, mustDims(t, NewMemory(nil, 0)), "no dimensions before indexing")
}

func mustDims(t *testing.T, m *Memory) map[string][]string {
	t.Helper()
	d, err := m.Dimensions(context.Background())
	require.NoError(t, err)
	return d
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

// --- ordered recall (ADR 0022) ---

// orderedDocs is a vault where only some notes carry the field: ep20 has no
// episode value at all, which is the case an implementation must not sort as zero.
func orderedDocs() []Doc {
	return []Doc{
		{Ref: DocRef{Path: "c.md", Title: "C"}, Ordered: map[string]float64{"episode": 30}},
		{Ref: DocRef{Path: "a.md", Title: "A"}, Ordered: map[string]float64{"episode": 10}},
		{Ref: DocRef{Path: "ep20.md", Title: "No number"}},
		{Ref: DocRef{Path: "b.md", Title: "B"}, Ordered: map[string]float64{"episode": 20}},
	}
}

// Descending is the whole set, highest first — and it is the whole set, so the
// answer cannot be an artefact of which documents a sample happened to include.
func TestOrderedSortsTheWholeSet(t *testing.T) {
	m := NewMemory(nil, 0)
	require.NoError(t, m.Index(context.Background(), orderedDocs()))

	got, err := m.Ordered(context.Background(), "episode", Descending, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"c.md", "b.md", "a.md"}, paths(got))

	got, err = m.Ordered(context.Background(), "episode", Ascending, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"a.md", "b.md", "c.md"}, paths(got))
}

// A document with no value for the field is ABSENT from both ends, not sorted as
// zero. Checking both directions is the point: a zero-keyed document hides at the
// bottom of a descending answer and only surfaces when you ask for the oldest,
// which is exactly the question a reader would trust least to be wrong.
func TestOrderedOmitsDocumentsWithNoValue(t *testing.T) {
	m := NewMemory(nil, 0)
	require.NoError(t, m.Index(context.Background(), orderedDocs()))

	for _, dir := range []Direction{Descending, Ascending} {
		got, err := m.Ordered(context.Background(), "episode", dir, 0)
		require.NoError(t, err)
		assert.NotContains(t, paths(got), "ep20.md", "direction %s", dir)
		assert.Len(t, got, 3)
	}
}

// The cap applies after sorting, and takes from the asked-for end.
func TestOrderedLimitTakesFromTheSortedEnd(t *testing.T) {
	m := NewMemory(nil, 0)
	require.NoError(t, m.Index(context.Background(), orderedDocs()))

	got, err := m.Ordered(context.Background(), "episode", Descending, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"c.md"}, paths(got))

	got, err = m.Ordered(context.Background(), "episode", Ascending, 2)
	require.NoError(t, err)
	assert.Equal(t, []string{"a.md", "b.md"}, paths(got))
}

// Equal values break on path, so a repeated key yields a stable order rather than
// map iteration order — otherwise the same question answers differently per run.
func TestOrderedTiesBreakOnPathDeterministically(t *testing.T) {
	docs := []Doc{
		{Ref: DocRef{Path: "z.md"}, Ordered: map[string]float64{"n": 1}},
		{Ref: DocRef{Path: "y.md"}, Ordered: map[string]float64{"n": 1}},
		{Ref: DocRef{Path: "x.md"}, Ordered: map[string]float64{"n": 1}},
	}
	m := NewMemory(nil, 0)
	require.NoError(t, m.Index(context.Background(), docs))

	for i := 0; i < 20; i++ {
		got, err := m.Ordered(context.Background(), "n", Ascending, 0)
		require.NoError(t, err)
		assert.Equal(t, []string{"x.md", "y.md", "z.md"}, paths(got))
	}

	// Descending reverses the FIELD order, not the tie-break: equal keys stay in
	// ascending path order, which is what the graph backend's query does and what
	// both backends document. Reversing the sorted slice wholesale would flip this
	// too, and then "the latest one" with a tie at the top value answers with a
	// different document depending on which backend is deployed — a single wrong
	// answer under limit 1, not a cosmetic difference in a list.
	for i := 0; i < 20; i++ {
		got, err := m.Ordered(context.Background(), "n", Descending, 0)
		require.NoError(t, err)
		assert.Equal(t, []string{"x.md", "y.md", "z.md"}, paths(got))
	}
}

// The tie-break survives a cap, which is the case that actually reaches a reader:
// "the latest one" is limit 1, so a flipped tie order is not a reordered list, it
// is a different document.
func TestOrderedTieBreakDecidesTheSingleTopAnswer(t *testing.T) {
	docs := []Doc{
		{Ref: DocRef{Path: "z.md"}, Ordered: map[string]float64{"n": 9}},
		{Ref: DocRef{Path: "x.md"}, Ordered: map[string]float64{"n": 9}},
		{Ref: DocRef{Path: "m.md"}, Ordered: map[string]float64{"n": 1}},
	}
	m := NewMemory(nil, 0)
	require.NoError(t, m.Index(context.Background(), docs))

	got, err := m.Ordered(context.Background(), "n", Descending, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"x.md"}, paths(got), "the tie is broken on ascending path, even at the top")
}

// An undeclared or unindexed field is an empty set, not an error — matching how
// Enumerate answers an unmatched value.
func TestOrderedUnknownFieldIsEmpty(t *testing.T) {
	m := NewMemory(nil, 0)
	require.NoError(t, m.Index(context.Background(), orderedDocs()))

	got, err := m.Ordered(context.Background(), "published", Descending, 0)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// Re-indexing replaces the ordered index wholesale, so a value removed from the
// vault stops being answerable rather than lingering in the old order.
func TestOrderedIsRebuiltOnReindex(t *testing.T) {
	m := NewMemory(nil, 0)
	require.NoError(t, m.Index(context.Background(), orderedDocs()))
	require.NoError(t, m.Index(context.Background(), []Doc{
		{Ref: DocRef{Path: "only.md"}, Ordered: map[string]float64{"episode": 5}},
	}))

	got, err := m.Ordered(context.Background(), "episode", Descending, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"only.md"}, paths(got))
}

// ParseOrderedValue reads the shapes a YAML frontmatter parser actually produces.
// An unquoted date arrives as a time.Time and a quoted one as a string, so both
// have to work; a field that reads only one of them silently loses half a vault.
func TestParseOrderedValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		key  float64
		kind OrderKind
		ok   bool
	}{
		{"int", 47, 47, OrderNumber, true},
		{"int64", int64(47), 47, OrderNumber, true},
		{"float", 3.5, 3.5, OrderNumber, true},
		{"negative", -2, -2, OrderNumber, true},
		{"zero is a real value", 0, 0, OrderNumber, true},
		{"numeric string", "47", 47, OrderNumber, true},
		{"typed date", time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), 1767571200, OrderDate, true},
		{"quoted date", "2026-01-05", 1767571200, OrderDate, true},
		{"rfc3339", "2026-01-05T00:00:00Z", 1767571200, OrderDate, true},
		{"slashed date", "2026/01/05", 1767571200, OrderDate, true},
		{"nil", nil, 0, OrderNone, false},
		{"empty", "", 0, OrderNone, false},
		{"prose", "N/A", 0, OrderNone, false},
		{"list", []any{1, 2}, 0, OrderNone, false},
		{"bool is a facet, not an ordinal", true, 0, OrderNone, false},
		// ParseFloat accepts these, and each would poison the ordering: NaN compares
		// false against everything including itself, so it breaks both the sort and
		// the equal-key grouping the tie-break depends on.
		{"NaN is not a position", "NaN", 0, OrderNone, false},
		{"Inf is not a position", "Inf", 0, OrderNone, false},
		{"-Inf is not a position", "-Inf", 0, OrderNone, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, kind, ok := ParseOrderedValue(tc.in)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.kind, kind)
			if tc.ok {
				assert.Equal(t, tc.key, key)
			}
		})
	}
}

// A date and a number must not be read into the same scale: the Unix second for a
// 2026 date is ~1.7e9, so a mixed field would sort every date above every episode
// number regardless of meaning. The kinds are what let the caller refuse the mix.
func TestNumbersAndDatesAreDistinguishableKinds(t *testing.T) {
	_, numKind, ok := ParseOrderedValue(47)
	require.True(t, ok)
	_, dateKind, ok := ParseOrderedValue("2026-01-05")
	require.True(t, ok)
	assert.NotEqual(t, numKind, dateKind)
}
