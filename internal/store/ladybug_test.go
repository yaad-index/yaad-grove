//go:build ladybug

package store

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/core"
)

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
