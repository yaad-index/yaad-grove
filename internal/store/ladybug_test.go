//go:build ladybug

package store

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// scaleEmb returns a small fixed-dimension vector for any text — for the
// large-index regression test, where the exact vectors don't matter.
type scaleEmb struct{ calls int }

func (s *scaleEmb) Embed(_ context.Context, texts []string) ([][]float32, error) {
	s.calls++
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(i%7) + 1, 1, 0, 0}
	}
	return out, nil
}

// dimEmb returns a fixed-width vector of a chosen dimension, so a regression can
// exercise the chunk-upsert path with realistically-sized embedding arrays (not the
// 4-float toy) — the per-chunk query volume is what deadlocked liblbug (#141).
type dimEmb struct{ dim int }

func (e dimEmb) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		v := make([]float32, e.dim)
		for j := range v {
			v[j] = float32((i+j)%13) * 0.1
		}
		out[i] = v
	}
	return out, nil
}

// Regression for #141: the per-chunk upsert path deadlocked liblbug on a full
// production-sized vault (~1890 chunks) even after #132 batched the structured
// rebuild, because chunk writes were still one MERGE per chunk. Batched UNWIND
// chunk upserts must index a full vault promptly under the write deadline, not
// hang. Realistically-sized embeddings so the per-chunk query volume matches prod.
func TestLadybugFullVaultChunkUpsert(t *testing.T) {
	l, err := NewLadybug(t.TempDir()+"/db", dimEmb{dim: 64}, 0)
	require.NoError(t, err)
	defer l.Close()

	const n = 1890
	docs := make([]Doc, 0, n)
	for i := 0; i < n; i++ {
		p := fmt.Sprintf("doc%05d.md", i)
		docs = append(docs, Doc{
			Ref:    DocRef{Path: p, Title: fmt.Sprintf("Doc %d", i)},
			Chunks: []core.Chunk{{Source: p, Text: fmt.Sprintf("chunk %d body text", i)}},
		})
	}
	require.NoError(t, l.Index(context.Background(), docs), "a full-vault chunk upsert completes, it does not hang")

	// The full chunk set is queryable — the vector + FTS indexes built over all of it.
	q := make([]float32, 64)
	for j := range q {
		q[j] = 0.1
	}
	sem, err := l.Semantic(context.Background(), q, 5)
	require.NoError(t, err)
	assert.NotEmpty(t, sem, "semantic works over the full-vault index")
	kw, err := l.Keyword(context.Background(), "body", 5)
	require.NoError(t, err)
	assert.NotEmpty(t, kw, "keyword works over the full-vault index")

	// Re-index the unchanged vault embeds nothing new and still completes (#86 delta).
	require.NoError(t, l.Index(context.Background(), docs))
}

// A poisoned store — a prior index write blew the wall-clock deadline, leaving a
// wedged cgo call — fails every method fast rather than issuing a query on an
// unusable connection (#141).
func TestLadybugPoisonedFailsFast(t *testing.T) {
	s, err := NewLadybug(t.TempDir()+"/db", dimEmb{dim: 4}, 0)
	require.NoError(t, err)
	defer s.Close()
	s.(*Ladybug).poisoned = true

	assert.ErrorIs(t, s.Index(context.Background(), nil), errStorePoisoned)
	_, err = s.Semantic(context.Background(), []float32{1, 0, 0, 0}, 1)
	assert.ErrorIs(t, err, errStorePoisoned)
	_, err = s.Keyword(context.Background(), "x", 1)
	assert.ErrorIs(t, err, errStorePoisoned)
	_, err = s.Enumerate(context.Background(), "games", "x")
	assert.ErrorIs(t, err, errStorePoisoned)
	_, err = s.Dimensions(context.Background())
	assert.ErrorIs(t, err, errStorePoisoned)
}

// Regression for #132: indexing a real-shaped vault (many docs, each with a chunk
// and several dimension values) must complete promptly, not hang. Before the fix —
// a per-row write for every doc/value — this took tens of seconds and hung at vault
// scale; the batched UNWIND + single transaction make it fast. The count is large
// enough to have tripped the old path but stays quick under the fix.
func TestLadybugLargeIndex(t *testing.T) {
	emb := &scaleEmb{}
	l, err := NewLadybug(t.TempDir()+"/db", emb, 0)
	require.NoError(t, err)
	defer l.Close()

	const n = 600
	var docs []Doc
	for i := 0; i < n; i++ {
		docs = append(docs, Doc{
			Ref:    DocRef{Path: fmt.Sprintf("ep%04d.md", i), Title: fmt.Sprintf("Episode %d", i)},
			Chunks: []core.Chunk{{Source: fmt.Sprintf("ep%04d.md", i), Text: fmt.Sprintf("episode %d transcript", i)}},
			Dimensions: map[string][]string{
				"games": {fmt.Sprintf("Game %d", i%150), fmt.Sprintf("Game %d", (i+1)%150)},
				"hosts": {fmt.Sprintf("Host %d", i%12)},
			},
		})
	}
	require.NoError(t, l.Index(context.Background(), docs))

	// Structured lookup is correct at scale: every episode sharing a game enumerates.
	got, err := l.Enumerate(context.Background(), "games", "Game 5")
	require.NoError(t, err)
	assert.NotEmpty(t, got, "the complete set for a shared game")

	// The vector + FTS indexes built over the full chunk set.
	sem, err := l.Semantic(context.Background(), []float32{1, 1, 0, 0}, 3)
	require.NoError(t, err)
	assert.NotEmpty(t, sem, "semantic works over the large index")

	// Re-index is the #86 delta: unchanged chunks aren't re-embedded.
	callsAfterFirst := emb.calls
	require.NoError(t, l.Index(context.Background(), docs))
	assert.Equal(t, callsAfterFirst, emb.calls, "an unchanged vault re-index embeds nothing new")
}

// End to end against the real embedded engine: Index a small vault, then exercise
// all three query paths + the content-hash delta. Runs only under -tags ladybug
// (needs the C library), so it lives behind the tag with the adapter.
func TestLadybugEndToEnd(t *testing.T) {
	fe := &fakeEmb{byText: map[string][]float32{
		"install the widget": {1, 0, 0},
		"reset the gadget":   {0, 1, 0},
	}}
	l, err := NewLadybug(t.TempDir()+"/db", fe, 0)
	require.NoError(t, err)
	defer l.Close()

	docs := []Doc{
		{Ref: DocRef{Path: "ep01.md", Title: "Ep 1"}, Chunks: []core.Chunk{{Source: "ep01.md", Text: "install the widget"}}, Dimensions: map[string][]string{"games": {"Acme"}}},
		{Ref: DocRef{Path: "ep02.md", Title: "Ep 2"}, Chunks: []core.Chunk{{Source: "ep02.md", Text: "reset the gadget"}}, Dimensions: map[string][]string{"games": {"Acme"}}},
		{Ref: DocRef{Path: "acme.md", Title: "Acme"}, Aliases: []string{"اکمی"}},
	}
	require.NoError(t, l.Index(context.Background(), docs))
	require.Equal(t, 1, fe.calls, "one batch embed of the two new chunks")

	// Semantic: query [1,0,0] → the "install the widget" chunk is nearest.
	sem, err := l.Semantic(context.Background(), []float32{1, 0, 0}, 5)
	require.NoError(t, err)
	require.NotEmpty(t, sem, "vector KNN returns chunks")
	assert.Equal(t, "install the widget", sem[0].Text, "nearest by cosine")

	// Keyword: BM25 FTS on "widget".
	kw, err := l.Keyword(context.Background(), "widget", 5)
	require.NoError(t, err)
	require.NotEmpty(t, kw, "FTS returns chunks")
	assert.Contains(t, kw[0].Text, "widget")

	// Enumerate by canonical name AND by cross-script alias → both episodes.
	byName, err := l.Enumerate(context.Background(), "games", "Acme")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"ep01.md", "ep02.md"}, paths(byName), "complete set by name")
	byAlias, err := l.Enumerate(context.Background(), "games", "اکمی")
	require.NoError(t, err)
	assert.ElementsMatch(t, paths(byName), paths(byAlias), "alias resolves to the same set")

	// Dimensions surfaces the value vocabulary by display form (ADR 0020): both
	// episodes carry games:[Acme], so the graph holds one distinct display value.
	dims, err := l.Dimensions(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"Acme"}, dims["games"], "the value vocabulary by display form")

	// Delta: re-index the same vault → no new embedding (content hashes unchanged).
	require.NoError(t, l.Index(context.Background(), docs))
	assert.Equal(t, 1, fe.calls, "unchanged chunks are not re-embedded (#86 delta)")
}

// A live reindex (Index) must be safe against concurrent queries: LadybugDB
// connections are not concurrent-safe, so the backend serializes every method on a
// mutex. Value is under -race — an unserialized l.conn access would trip it (and
// would confirm no deadlock, since internal helpers must not re-lock).
func TestLadybugConcurrentReindexAndQuery(t *testing.T) {
	fe := &fakeEmb{byText: map[string][]float32{"install the widget": {1, 0, 0}, "reset the gadget": {0, 1, 0}}}
	l, err := NewLadybug(t.TempDir()+"/db", fe, 0)
	require.NoError(t, err)
	defer l.Close()
	docs := []Doc{
		{Ref: DocRef{Path: "ep01.md", Title: "Ep 1"}, Chunks: []core.Chunk{{Source: "ep01.md", Text: "install the widget"}}, Dimensions: map[string][]string{"games": {"Acme"}}},
		{Ref: DocRef{Path: "ep02.md", Title: "Ep 2"}, Chunks: []core.Chunk{{Source: "ep02.md", Text: "reset the gadget"}}, Dimensions: map[string][]string{"games": {"Acme"}}},
	}
	require.NoError(t, l.Index(context.Background(), docs))

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = l.Semantic(context.Background(), []float32{1, 0, 0}, 5)
				_, _ = l.Keyword(context.Background(), "widget", 5)
				_, _ = l.Enumerate(context.Background(), "games", "Acme")
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10; i++ {
			_ = l.Index(context.Background(), docs)
		}
	}()
	<-done
	close(stop)
	wg.Wait()
}

// Ordered recall on the graph backend (ADR 0022). The sort and the cap are pushed
// into the database, so this is the only place their behaviour can be confirmed —
// the memory backend's tests say nothing about a Cypher ORDER BY.
//
// It also pins the two properties both backends must agree on: a document with no
// value is absent rather than zero-keyed, and equal keys break on path so the two
// backends do not answer the same question in different orders.
func TestLadybugOrdered(t *testing.T) {
	l, err := NewLadybug(t.TempDir()+"/db", dimEmb{dim: 4}, 0)
	require.NoError(t, err)
	defer l.Close()

	docs := []Doc{
		{Ref: DocRef{Path: "ep30.md", Title: "Thirty"}, Chunks: []core.Chunk{{Source: "ep30.md", Text: "a"}},
			Ordered: map[string]float64{"episode": 30}},
		{Ref: DocRef{Path: "ep10.md", Title: "Ten"}, Chunks: []core.Chunk{{Source: "ep10.md", Text: "b"}},
			Ordered: map[string]float64{"episode": 10}},
		// No episode value at all — must never appear in an ordered answer.
		{Ref: DocRef{Path: "none.md", Title: "None"}, Chunks: []core.Chunk{{Source: "none.md", Text: "c"}}},
		{Ref: DocRef{Path: "ep20.md", Title: "Twenty"}, Chunks: []core.Chunk{{Source: "ep20.md", Text: "d"}},
			Ordered: map[string]float64{"episode": 20}},
	}
	require.NoError(t, l.Index(context.Background(), docs))

	desc, err := l.Ordered(context.Background(), "episode", Descending, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"ep30.md", "ep20.md", "ep10.md"}, paths(desc))

	asc, err := l.Ordered(context.Background(), "episode", Ascending, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"ep10.md", "ep20.md", "ep30.md"}, paths(asc))

	// Both ends omit the valueless document, not just the one it would sort last on.
	for _, refs := range [][]DocRef{desc, asc} {
		assert.NotContains(t, paths(refs), "none.md")
	}

	top, err := l.Ordered(context.Background(), "episode", Descending, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"ep30.md"}, paths(top), "the cap takes from the sorted end")

	unknown, err := l.Ordered(context.Background(), "episode2", Descending, 0)
	require.NoError(t, err)
	assert.Empty(t, unknown, "an unindexed field is empty, not an error")
}

// A reindex replaces the ordered index rather than adding to it, so a document
// dropped from the vault stops being answerable.
//
// ⚠️ Note what this does NOT prove. Deleting the Doc nodes is DETACH, so the
// HAS_ORDER edges go with them and an orphaned Ordinal is already unreachable —
// this test passes with the Ordinal cleanup removed entirely (verified by
// mutation). The cleanup exists to stop orphaned Ordinal nodes accumulating on
// every reindex, which is storage rather than correctness, and that is pinned
// separately below. Left unsaid, this test's name would have implied cover it does
// not give.
func TestLadybugOrderedIsRebuiltOnReindex(t *testing.T) {
	l, err := NewLadybug(t.TempDir()+"/db", dimEmb{dim: 4}, 0)
	require.NoError(t, err)
	defer l.Close()

	require.NoError(t, l.Index(context.Background(), []Doc{
		{Ref: DocRef{Path: "old.md", Title: "Old"}, Chunks: []core.Chunk{{Source: "old.md", Text: "a"}},
			Ordered: map[string]float64{"episode": 99}},
	}))
	require.NoError(t, l.Index(context.Background(), []Doc{
		{Ref: DocRef{Path: "new.md", Title: "New"}, Chunks: []core.Chunk{{Source: "new.md", Text: "b"}},
			Ordered: map[string]float64{"episode": 1}},
	}))

	got, err := l.Ordered(context.Background(), "episode", Descending, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"new.md"}, paths(got), "the removed document is gone from the order")
}

// The Ordinal cleanup itself: reindexing must not leave the previous pass's nodes
// behind. Orphaned Ordinals are unreachable by query, so no answer goes wrong —
// which is exactly why this needs its own assertion. Without it the store grows by
// one node per (document, field) on every reindex, and a persistent backend that
// reindexes on every vault edit would never give the space back.
func TestLadybugReindexLeavesNoOrphanedOrdinals(t *testing.T) {
	l, err := NewLadybug(t.TempDir()+"/db", dimEmb{dim: 4}, 0)
	require.NoError(t, err)
	defer l.Close()

	lb := l.(*Ladybug) // the node count is a storage fact, not part of the port
	countOrdinals := func() int {
		r, qerr := lb.conn.Query("MATCH (o:Ordinal) RETURN COUNT(o);")
		require.NoError(t, qerr)
		defer r.Close()
		require.True(t, r.HasNext())
		row, nerr := r.Next()
		require.NoError(t, nerr)
		vals, serr := row.GetAsSlice()
		require.NoError(t, serr)
		require.NotEmpty(t, vals)
		// COUNT comes back as an integer, so it is read as one. The package's
		// toFloat only handles the float cases and returns 0 for anything else,
		// which would make this probe report "no orphans" no matter what the store
		// held — a check that cannot fail is worse than no check.
		switch n := vals[0].(type) {
		case int64:
			return int(n)
		case int:
			return n
		case float64:
			return int(n)
		default:
			t.Fatalf("unexpected COUNT type %T", vals[0])
			return 0
		}
	}

	index := func(path string, key float64) {
		require.NoError(t, l.Index(context.Background(), []Doc{
			{Ref: DocRef{Path: path}, Chunks: []core.Chunk{{Source: path, Text: "x"}},
				Ordered: map[string]float64{"episode": key}},
		}))
	}

	index("a.md", 1)
	require.Equal(t, 1, countOrdinals())
	index("b.md", 2)
	assert.Equal(t, 1, countOrdinals(), "the previous pass's Ordinal is gone, not orphaned")
	index("c.md", 3)
	assert.Equal(t, 1, countOrdinals(), "and it does not grow with each reindex")
}

// The tie-break contract, pinned on this backend too. Descending reverses the
// field order but NOT the tie-break: equal keys stay in ascending path order.
//
// This is the half that makes the claim "both backends answer the same question
// in the same order" checkable. Asserting it only in the memory backend's tests
// leaves the two free to drift apart, and the drift is invisible until a
// deployment swaps backends and "the latest one" starts naming a different
// document whenever the top value is tied.
func TestLadybugOrderedTieBreakMatchesTheMemoryBackend(t *testing.T) {
	l, err := NewLadybug(t.TempDir()+"/db", dimEmb{dim: 4}, 0)
	require.NoError(t, err)
	defer l.Close()

	docs := []Doc{
		{Ref: DocRef{Path: "z.md"}, Chunks: []core.Chunk{{Source: "z.md", Text: "a"}}, Ordered: map[string]float64{"n": 9}},
		{Ref: DocRef{Path: "x.md"}, Chunks: []core.Chunk{{Source: "x.md", Text: "b"}}, Ordered: map[string]float64{"n": 9}},
		{Ref: DocRef{Path: "y.md"}, Chunks: []core.Chunk{{Source: "y.md", Text: "c"}}, Ordered: map[string]float64{"n": 9}},
	}
	require.NoError(t, l.Index(context.Background(), docs))

	for _, dir := range []Direction{Ascending, Descending} {
		got, gerr := l.Ordered(context.Background(), "n", dir, 0)
		require.NoError(t, gerr)
		assert.Equal(t, []string{"x.md", "y.md", "z.md"}, paths(got), "direction %s", dir)
	}

	// And it decides the single top answer, which is the form a reader sees.
	top, err := l.Ordered(context.Background(), "n", Descending, 1)
	require.NoError(t, err)
	assert.Equal(t, []string{"x.md"}, paths(top))
}
