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

type stubRetriever struct {
	chunks []core.Chunk
	err    error
	called bool
}

func (s *stubRetriever) Retrieve(context.Context, string) ([]core.Chunk, error) {
	s.called = true
	return s.chunks, s.err
}

// A primary error falls back to the secondary and returns its result.
func TestFallbackOnPrimaryError(t *testing.T) {
	primary := &stubRetriever{err: errors.New("embed endpoint down")}
	secondary := &stubRetriever{chunks: []core.Chunk{{Source: "a", Text: "kw"}}}
	got, err := retrieval.Fallback{Primary: primary, Secondary: secondary}.Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.True(t, secondary.called, "secondary is used when the primary errors")
	require.Len(t, got, 1)
	assert.Equal(t, "kw", got[0].Text)
}

// A successful primary result is returned without touching the secondary.
func TestFallbackPrimarySucceeds(t *testing.T) {
	primary := &stubRetriever{chunks: []core.Chunk{{Source: "a", Text: "sem"}}}
	secondary := &stubRetriever{}
	got, err := retrieval.Fallback{Primary: primary, Secondary: secondary}.Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.False(t, secondary.called, "secondary untouched when the primary succeeds")
	require.Len(t, got, 1)
	assert.Equal(t, "sem", got[0].Text)
}

// An EMPTY (non-error) primary result is a valid "nothing relevant" — not a
// failure — so it does NOT fall back (the grounding block must still fire).
func TestFallbackEmptyPrimaryDoesNotFallBack(t *testing.T) {
	primary := &stubRetriever{chunks: nil}
	secondary := &stubRetriever{chunks: []core.Chunk{{Text: "kw"}}}
	got, err := retrieval.Fallback{Primary: primary, Secondary: secondary}.Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Empty(t, got, "empty is a valid refuse, not a fallback trigger")
	assert.False(t, secondary.called)
}
