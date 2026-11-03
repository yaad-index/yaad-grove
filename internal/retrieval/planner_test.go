package retrieval_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/retrieval"
	"github.com/yaad-index/yaad-grove/internal/store"
)

// fakeStore returns canned primitives and records which were consulted, so the
// planner's composition (fusion, fallback, partial-failure) is tested in isolation
// from any real backend.
type fakeStore struct {
	kw, sem         []core.Chunk
	kwErr, semErr   error
	kwCalled, semOK bool
	kwK, semK       int // the k each leg was last asked for
}

// top returns the first k chunks, as a store's top-k does.
func top(chunks []core.Chunk, k int) []core.Chunk {
	if k < len(chunks) {
		return chunks[:k]
	}
	return chunks
}

func (f *fakeStore) Index(context.Context, []store.Doc) error { return nil }

func (f *fakeStore) Semantic(_ context.Context, _ []float32, k int) ([]core.Chunk, error) {
	f.semOK, f.semK = true, k
	return top(f.sem, k), f.semErr
}

func (f *fakeStore) Keyword(_ context.Context, _ string, k int) ([]core.Chunk, error) {
	f.kwCalled, f.kwK = true, k
	return top(f.kw, k), f.kwErr
}

func (f *fakeStore) Enumerate(context.Context, string, string) ([]store.DocRef, error) {
	return nil, store.ErrEnumerateNotImplemented
}

func (f *fakeStore) Dimensions(context.Context) (map[string][]string, error) { return nil, nil }

// The planner never orders — grounding retrieval is the similarity path — so this
// fake refuses loudly rather than returning an empty set a caller could mistake
// for "no document carries that field".
func (f *fakeStore) Ordered(context.Context, string, store.Direction, int) ([]store.DocRef, error) {
	return nil, store.ErrOrderedNotImplemented
}

func (f *fakeStore) Close() error { return nil }

// fakeEmb yields one fixed vector per Embed and counts calls, so "did we embed the
// query" is observable.
type fakeEmb struct {
	vec   []float32
	err   error
	calls int
}

func (f *fakeEmb) Embed(context.Context, []string) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return [][]float32{f.vec}, nil
}

// ch makes a chunk whose Source and Text are both name, so its dedup key is unique
// per name and assertions can refer to it by name.
func ch(name string) core.Chunk { return core.Chunk{Source: name, Text: name} }

func names(chunks []core.Chunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.Text
	}
	return out
}

// Keyword mode consults only the keyword primitive — no query embed, no semantic.
func TestPlannerKeywordMode(t *testing.T) {
	st := &fakeStore{kw: []core.Chunk{ch("kw")}}
	emb := &fakeEmb{}
	got, err := retrieval.NewPlanner(st, emb, retrieval.ModeKeyword, 8).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Equal(t, []string{"kw"}, names(got))
	assert.False(t, st.semOK, "keyword mode never touches the semantic primitive")
	assert.Zero(t, emb.calls, "keyword mode never embeds the query")
}

// Semantic mode returns the semantic result and, on success, never falls back to
// keyword.
func TestPlannerSemanticSuccess(t *testing.T) {
	st := &fakeStore{sem: []core.Chunk{ch("sem")}, kw: []core.Chunk{ch("kw")}}
	emb := &fakeEmb{vec: []float32{1, 0}}
	got, err := retrieval.NewPlanner(st, emb, retrieval.ModeSemantic, 8).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Equal(t, []string{"sem"}, names(got))
	assert.False(t, st.kwCalled, "keyword untouched when semantic succeeds")
	assert.Equal(t, 1, emb.calls, "the query is embedded once")
}

// A dead embed endpoint degrades semantic mode to keyword.
func TestPlannerSemanticFallsBackOnEmbedError(t *testing.T) {
	st := &fakeStore{sem: []core.Chunk{ch("sem")}, kw: []core.Chunk{ch("kw")}}
	emb := &fakeEmb{err: errors.New("embed endpoint down")}
	got, err := retrieval.NewPlanner(st, emb, retrieval.ModeSemantic, 8).Retrieve(context.Background(), "q")
	require.NoError(t, err, "an embed failure degrades, not fails")
	assert.Equal(t, []string{"kw"}, names(got), "falls back to keyword")
	assert.True(t, st.kwCalled)
}

// A semantic-backend error degrades semantic mode to keyword.
func TestPlannerSemanticFallsBackOnStoreError(t *testing.T) {
	st := &fakeStore{semErr: errors.New("index unavailable"), kw: []core.Chunk{ch("kw")}}
	emb := &fakeEmb{vec: []float32{1, 0}}
	got, err := retrieval.NewPlanner(st, emb, retrieval.ModeSemantic, 8).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Equal(t, []string{"kw"}, names(got))
}

// An EMPTY (non-error) semantic result is a valid "nothing relevant" — NOT a
// fallback trigger — so it is returned as-is and the grounding block fires.
func TestPlannerSemanticEmptyDoesNotFallBack(t *testing.T) {
	st := &fakeStore{sem: nil, kw: []core.Chunk{ch("kw")}}
	emb := &fakeEmb{vec: []float32{1, 0}}
	got, err := retrieval.NewPlanner(st, emb, retrieval.ModeSemantic, 8).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Empty(t, got, "empty is a valid refuse, not a fallback trigger")
	assert.False(t, st.kwCalled, "keyword untouched on a valid-empty semantic result")
}

// An empty query makes no embed call and returns nothing (semantic mode).
func TestPlannerSemanticEmptyQueryNoEmbed(t *testing.T) {
	st := &fakeStore{}
	emb := &fakeEmb{vec: []float32{1, 0}}
	got, err := retrieval.NewPlanner(st, emb, retrieval.ModeSemantic, 8).Retrieve(context.Background(), "   ")
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Zero(t, emb.calls, "empty query makes no embed call")
}

// RRF fuses the legs' ranked lists in [keyword, semantic] order: a chunk ranked by
// BOTH legs outscores one ranked by a single leg. keyword=[A,B,C], semantic=[B,D]
// → B (in both) leads, then A, D, C by RRF score.
func TestPlannerHybridRRFOrdering(t *testing.T) {
	st := &fakeStore{kw: []core.Chunk{ch("A"), ch("B"), ch("C")}, sem: []core.Chunk{ch("B"), ch("D")}}
	emb := &fakeEmb{vec: []float32{1, 0}}
	got, err := retrieval.NewPlanner(st, emb, retrieval.ModeHybrid, 0).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Equal(t, []string{"B", "A", "D", "C"}, names(got), "B fused from both legs → top; then A, D, C by RRF score")
}

// A chunk returned by both legs is deduped to one.
func TestPlannerHybridDedups(t *testing.T) {
	st := &fakeStore{kw: []core.Chunk{ch("X"), ch("Y")}, sem: []core.Chunk{ch("X")}}
	emb := &fakeEmb{vec: []float32{1, 0}}
	got, err := retrieval.NewPlanner(st, emb, retrieval.ModeHybrid, 0).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Equal(t, []string{"X", "Y"}, names(got), "X fused once (both legs), Y once")
}

// The empty-LEG case (distinct from an errored leg): the semantic leg returns
// EMPTY (nothing cleared the floor) but keyword matched the proper noun. The empty
// leg is a consulted-and-ok leg — it flows through fusion contributing nothing, so
// the lexical-only hit still surfaces. This is the exact under-recall miss Fallback
// couldn't fix (empty is not an error).
func TestPlannerHybridEmptySemanticLegSurfacesLexical(t *testing.T) {
	st := &fakeStore{kw: []core.Chunk{ch("zorbon-note")}, sem: nil}
	emb := &fakeEmb{vec: []float32{1, 0}}
	got, err := retrieval.NewPlanner(st, emb, retrieval.ModeHybrid, 0).Retrieve(context.Background(), "which episode discusses Zorbon?")
	require.NoError(t, err)
	require.Len(t, got, 1, "a lexical-only hit surfaces even when the semantic leg is empty")
	assert.Equal(t, "zorbon-note", got[0].Text)
	assert.True(t, st.semOK, "both legs are always consulted")
}

// The errored-LEG case (distinct from an empty leg): a leg that ERRORS is logged
// and skipped, and the surviving leg still answers.
func TestPlannerHybridErroredLegIsSkipped(t *testing.T) {
	st := &fakeStore{kw: []core.Chunk{ch("kw")}, semErr: errors.New("embed index down")}
	emb := &fakeEmb{vec: []float32{1, 0}}
	got, err := retrieval.NewPlanner(st, emb, retrieval.ModeHybrid, 0).Retrieve(context.Background(), "q")
	require.NoError(t, err, "one leg down still answers from the other")
	assert.Equal(t, []string{"kw"}, names(got))
}

// The empty/error distinction at the extreme, pinned as two behaviors:
//
//	(a) ALL legs empty-but-ok → empty result, NO error (a valid refusal; the
//	    grounding block fires).
//	(b) ALL legs errored → an error (a silent empty would be misread as a
//	    legitimate refusal).
//
// If the two were conflated, one of these would flip.
func TestPlannerHybridAllEmptyVsAllError(t *testing.T) {
	emb := &fakeEmb{vec: []float32{1, 0}}

	allEmpty := &fakeStore{kw: nil, sem: nil}
	got, err := retrieval.NewPlanner(allEmpty, emb, retrieval.ModeHybrid, 0).Retrieve(context.Background(), "q")
	require.NoError(t, err, "all-empty is a valid refusal, not an error")
	assert.Empty(t, got)

	allError := &fakeStore{kwErr: errors.New("kw down"), semErr: errors.New("sem down")}
	_, err = retrieval.NewPlanner(allError, emb, retrieval.ModeHybrid, 0).Retrieve(context.Background(), "q")
	assert.Error(t, err, "all-legs-error surfaces an error, not empty")
}

// An embed failure in hybrid mode takes down only the semantic leg; the keyword
// leg still fuses (degrade, not fail).
func TestPlannerHybridEmbedErrorDegradesToKeyword(t *testing.T) {
	st := &fakeStore{kw: []core.Chunk{ch("kw")}, sem: []core.Chunk{ch("sem")}}
	emb := &fakeEmb{err: errors.New("embed endpoint down")}
	got, err := retrieval.NewPlanner(st, emb, retrieval.ModeHybrid, 0).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Equal(t, []string{"kw"}, names(got), "semantic leg down via embed → keyword-only fusion")
}

// maxChunks caps the fused output.
func TestPlannerHybridCapsAtMaxChunks(t *testing.T) {
	st := &fakeStore{kw: []core.Chunk{ch("A"), ch("B"), ch("C")}, sem: []core.Chunk{ch("D"), ch("E")}}
	emb := &fakeEmb{vec: []float32{1, 0}}
	got, err := retrieval.NewPlanner(st, emb, retrieval.ModeHybrid, 2).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Len(t, got, 2, "the fused result is capped at maxChunks")
}

// A chunk ranked just below the cap by both legs is what fusion exists to find
// (#246). Each leg here is led by eleven chunks the other leg never returns, and
// M is tenth in both. Capping each leg at the 8 chunks the answer gets, before
// fusing, never lets M reach fusion; fusing each leg's deeper list first puts M,
// the only chunk both legs agree on, at the top.
func TestPlannerHybridFusesBeforeCapping(t *testing.T) {
	leg := func(prefix string) []core.Chunk {
		var out []core.Chunk
		for i := 1; i <= 12; i++ {
			if i == 10 {
				out = append(out, ch("M"))
				continue
			}
			out = append(out, ch(fmt.Sprintf("%s%d", prefix, i)))
		}
		return out
	}
	emb := &fakeEmb{vec: []float32{1, 0}}

	st := &fakeStore{kw: leg("kw"), sem: leg("sem")}
	got, err := retrieval.NewPlanner(st, emb, retrieval.ModeHybrid, 8, retrieval.WithFusionDepth(8)).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Len(t, got, 8)
	assert.NotContains(t, names(got), "M", "capped before fusing, M never reaches fusion")

	st = &fakeStore{kw: leg("kw"), sem: leg("sem")}
	got, err = retrieval.NewPlanner(st, emb, retrieval.ModeHybrid, 8).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	require.Len(t, got, 8, "the fused list is still capped at maxChunks")
	assert.Equal(t, "M", got[0].Text, "fused before capping, the chunk both legs rank is first")
	assert.Equal(t, retrieval.DefaultFusionDepth, st.kwK)
	assert.Equal(t, retrieval.DefaultFusionDepth, st.semK)
}

// The fusion depth is hybrid's alone: keyword and semantic modes return the
// store's top maxChunks, and a depth below maxChunks fuses maxChunks per leg.
func TestPlannerFusionDepthBounds(t *testing.T) {
	emb := &fakeEmb{vec: []float32{1, 0}}
	st := &fakeStore{}
	_, err := retrieval.NewPlanner(st, emb, retrieval.ModeKeyword, 8, retrieval.WithFusionDepth(30)).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Equal(t, 8, st.kwK)
	_, err = retrieval.NewPlanner(st, emb, retrieval.ModeSemantic, 8, retrieval.WithFusionDepth(30)).Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Equal(t, 8, st.semK)

	p := retrieval.NewPlanner(st, emb, retrieval.ModeHybrid, 8, retrieval.WithFusionDepth(3))
	assert.Equal(t, 8, p.FusionDepth())
	_, err = p.Retrieve(context.Background(), "q")
	require.NoError(t, err)
	assert.Equal(t, 8, st.kwK)
	assert.Equal(t, 8, st.semK)
	assert.Equal(t, 30, retrieval.NewPlanner(st, emb, retrieval.ModeHybrid, 8, retrieval.WithFusionDepth(30)).FusionDepth())
}

// An unknown mode is a loud error, not a silent empty.
func TestPlannerUnknownMode(t *testing.T) {
	_, err := retrieval.NewPlanner(&fakeStore{}, &fakeEmb{}, "bogus", 8).Retrieve(context.Background(), "q")
	assert.ErrorContains(t, err, "unknown mode")
}
