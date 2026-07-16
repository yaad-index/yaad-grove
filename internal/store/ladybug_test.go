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
