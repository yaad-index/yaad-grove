package retrieval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

// A pre-built index: chunks c0/c1/c2 with the given vectors, query "q" mapped to
// qvec. Cosines to q=[1,0]: c0=1.0, c1≈0.707, c2=0.
func fixtureSemantic(threshold float32, maxChunks int) *Semantic {
	return &Semantic{
		embedder:  &fakeEmb{byText: map[string][]float32{"q": {1, 0}}},
		maxChunks: maxChunks,
		threshold: threshold,
		chunks:    []core.Chunk{{Source: "a", Text: "c0"}, {Source: "b", Text: "c1"}, {Source: "c", Text: "c2"}},
		vectors:   [][]float32{{1, 0}, {0.7, 0.7}, {0, 1}},
	}
}

// Above-threshold chunks come back ranked by similarity; below-threshold are dropped.
func TestSemanticRetrieveRanksAndFilters(t *testing.T) {
	s := fixtureSemantic(0.3, 8)
	got, err := s.Retrieve(context.Background(), "q")
	require.NoError(t, err)
	require.Len(t, got, 2, "c2 (sim 0) is below the 0.3 floor")
	assert.Equal(t, "c0", got[0].Text, "highest similarity first")
	assert.Equal(t, "c1", got[1].Text)
}

// Nothing above the threshold → empty, so the pre-model grounding block fires.
func TestSemanticRetrieveEmptyBelowThreshold(t *testing.T) {
	s := fixtureSemantic(0.9, 8) // only c0 (1.0) would clear 0.9
	s.chunks = s.chunks[1:]      // drop c0; c1(0.707), c2(0) both below 0.9
	s.vectors = s.vectors[1:]
	got, err := s.Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Empty(t, got, "nothing clears the floor → empty (grounding block fires)")
}

// A zero threshold returns the top-k regardless (brain-judges mode).
func TestSemanticRetrieveThresholdZeroReturnsTopK(t *testing.T) {
	s := fixtureSemantic(0, 8)
	got, err := s.Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Len(t, got, 3, "no floor → all chunks reach the model, ranked")
	assert.Equal(t, "c0", got[0].Text)
}

// threshold=0 is "no floor": even a chunk with negative cosine is returned, so a
// non-empty vault is never empty in brain-judges mode (ADR 0017 contract).
func TestSemanticThresholdZeroIncludesNegativeSim(t *testing.T) {
	s := &Semantic{
		embedder:  &fakeEmb{byText: map[string][]float32{"q": {1, 0}}},
		maxChunks: 8,
		threshold: 0,
		chunks:    []core.Chunk{{Source: "p", Text: "pos"}, {Source: "n", Text: "neg"}},
		vectors:   [][]float32{{1, 0}, {-1, 0}}, // cosines to q=[1,0]: +1 and -1
	}
	got, err := s.Retrieve(context.Background(), "q")
	require.NoError(t, err)
	require.Len(t, got, 2, "no floor returns even the negative-cosine chunk")
	assert.Equal(t, "pos", got[0].Text, "still ranked by similarity")
	assert.Equal(t, "neg", got[1].Text)
}

// MaxChunks caps the result even when more clear the threshold.
func TestSemanticRetrieveCap(t *testing.T) {
	s := fixtureSemantic(0, 1)
	got, err := s.Retrieve(context.Background(), "q")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "c0", got[0].Text, "the single kept chunk is the top-ranked")
}

// An empty query or empty index returns nothing without an embedding call.
func TestSemanticRetrieveNoOp(t *testing.T) {
	s := fixtureSemantic(0.3, 8)
	fe := s.embedder.(*fakeEmb)

	got, err := s.Retrieve(context.Background(), "   ")
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Zero(t, fe.calls, "empty query makes no embed call")

	empty := &Semantic{embedder: fe, maxChunks: 8, threshold: 0.3}
	got, err = empty.Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Empty(t, got, "empty index → nothing")
	assert.Zero(t, fe.calls, "empty index makes no embed call")
}

// NewSemantic builds the index from the shared chunker and embeds every chunk;
// the chunk set matches vaultChunks (same chunking as the keyword retriever).
func TestNewSemanticBuildsIndex(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("# Intro\nhello\n# Setup\nworld\n"), 0o600))

	want, err := vaultChunks(context.Background(), dir)
	require.NoError(t, err)
	require.NotEmpty(t, want)

	vecs := map[string][]float32{}
	for i, c := range want {
		vecs[c.Text] = []float32{float32(i), 1}
	}
	fe := &fakeEmb{byText: vecs}
	s, err := NewSemantic(context.Background(), dir, fe, 8, 0.3)
	require.NoError(t, err)
	assert.Len(t, s.chunks, len(want), "reuses the shared chunker")
	assert.Len(t, s.vectors, len(want), "every chunk embedded")
}

// A build-time embedding failure is returned so the caller can fail startup.
func TestNewSemanticEmbedFailureIsFatal(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("content"), 0o600))
	_, err := NewSemantic(context.Background(), dir, &fakeEmb{err: errors.New("endpoint down")}, 8, 0.3)
	assert.Error(t, err, "an index-build embedding failure is fatal")
}
