package retrieval_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/retrieval"
)

// ch makes a chunk whose Source and Text are both name, so its dedup key is
// unique per name and assertions can refer to it by name.
func ch(name string) core.Chunk { return core.Chunk{Source: name, Text: name} }

func names(chunks []core.Chunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.Text
	}
	return out
}

// RRF fuses the legs' ranked lists: a chunk ranked by BOTH legs outscores one
// ranked by a single leg, and within a single leg a better rank wins. With
// keyword=[A,B,C] and semantic=[B,D], B (in both) leads, then A, D, C by score.
func TestHybridRRFOrdering(t *testing.T) {
	keyword := &stubRetriever{chunks: []core.Chunk{ch("A"), ch("B"), ch("C")}}
	semantic := &stubRetriever{chunks: []core.Chunk{ch("B"), ch("D")}}

	got, err := retrieval.NewHybrid(0, keyword, semantic).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Equal(t, []string{"B", "A", "D", "C"}, names(got), "B is fused from both legs → top; then A, D, C by RRF score")
}

// The Barcelona regression: the semantic leg returns EMPTY (nothing cleared the
// floor) but keyword matched the proper noun. Hybrid must still surface the
// lexical hit — the exact miss Fallback couldn't fix (empty is not an error).
func TestHybridSurfacesLexicalWhenSemanticEmpty(t *testing.T) {
	keyword := &stubRetriever{chunks: []core.Chunk{ch("barcelona-note")}}
	semantic := &stubRetriever{chunks: nil} // below the cosine floor → empty, no error

	got, err := retrieval.NewHybrid(0, keyword, semantic).Retrieve(context.Background(), "which episode discusses Barcelona?")
	require.NoError(t, err)
	require.Len(t, got, 1, "a lexical-only hit surfaces even when the semantic leg is empty")
	assert.Equal(t, "barcelona-note", got[0].Text)
	assert.True(t, semantic.called, "both legs are always consulted")
}

// A deduped chunk appears once even when both legs return it.
func TestHybridDedups(t *testing.T) {
	keyword := &stubRetriever{chunks: []core.Chunk{ch("X"), ch("Y")}}
	semantic := &stubRetriever{chunks: []core.Chunk{ch("X")}}
	got, err := retrieval.NewHybrid(0, keyword, semantic).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Equal(t, []string{"X", "Y"}, names(got), "X fused once (both legs), Y once")
}

// A single leg erroring degrades to the surviving leg rather than failing the
// query (e.g. an embedding-endpoint blip must not blind the lexical leg).
func TestHybridOneLegErrorDegrades(t *testing.T) {
	keyword := &stubRetriever{chunks: []core.Chunk{ch("kw")}}
	semantic := &stubRetriever{err: errors.New("embed endpoint down")}
	got, err := retrieval.NewHybrid(0, keyword, semantic).Retrieve(context.Background(), "q")
	require.NoError(t, err, "one leg down still answers from the other")
	assert.Equal(t, []string{"kw"}, names(got))
}

// Only when EVERY leg errors does Retrieve error — a silent empty would be
// misread by the engine as a legitimate refusal.
func TestHybridAllLegsErrorReturnsError(t *testing.T) {
	a := &stubRetriever{err: errors.New("a down")}
	b := &stubRetriever{err: errors.New("b down")}
	_, err := retrieval.NewHybrid(0, a, b).Retrieve(context.Background(), "q")
	assert.Error(t, err, "all legs failing surfaces an error, not empty")
}

// All legs empty (no error) is a valid "nothing relevant" — empty result, no
// error, so the grounding block fires (the single-retriever contract holds).
func TestHybridAllEmptyIsValidRefusal(t *testing.T) {
	a := &stubRetriever{chunks: nil}
	b := &stubRetriever{chunks: nil}
	got, err := retrieval.NewHybrid(0, a, b).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Empty(t, got, "both empty → empty result, not an error")
}

// MaxChunks caps the fused output.
func TestHybridCapsAtMaxChunks(t *testing.T) {
	keyword := &stubRetriever{chunks: []core.Chunk{ch("A"), ch("B"), ch("C")}}
	semantic := &stubRetriever{chunks: []core.Chunk{ch("D"), ch("E")}}
	got, err := retrieval.NewHybrid(2, keyword, semantic).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Len(t, got, 2, "the fused result is capped at MaxChunks")
}
