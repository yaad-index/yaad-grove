package tools_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/store"
	"github.com/yaad-index/yaad-grove/internal/tools"
)

// fakeBase is a stand-in for the MCP registry: it advertises its own tools and
// records the last call routed to it.
type fakeBase struct {
	defs   []core.ToolDef
	called string
}

func (f *fakeBase) Defs() []core.ToolDef { return f.defs }
func (f *fakeBase) Call(_ context.Context, name string, _ map[string]any) (string, error) {
	f.called = name
	return "base:" + name, nil
}

// fakeEnum is a stand-in for the structured Store surface: it returns canned refs
// and a canned value vocabulary, and records the query it was asked.
type fakeEnum struct {
	refs     []store.DocRef
	dim, val string
	vocab    map[string][]string
}

func (f *fakeEnum) Enumerate(_ context.Context, dimension, value string) ([]store.DocRef, error) {
	f.dim, f.val = dimension, value
	return f.refs, nil
}

func (f *fakeEnum) Dimensions(context.Context) (map[string][]string, error) {
	return f.vocab, nil
}

func defByName(defs []core.ToolDef, name string) (core.ToolDef, bool) {
	for _, d := range defs {
		if d.Name == name {
			return d, true
		}
	}
	return core.ToolDef{}, false
}

// With no declared dimensions the base tool set is returned unchanged — the
// structured-lookup tool is not advertised for a bot without structured data.
func TestWithEnumerateNoDimensionsIsIdentity(t *testing.T) {
	base := &fakeBase{defs: []core.ToolDef{{Name: "search"}}}
	got := tools.WithEnumerate(base, &fakeEnum{}, nil)
	_, hasEnum := defByName(got.Defs(), "kb_enumerate")
	_, hasDims := defByName(got.Defs(), "kb_dimensions")
	assert.False(t, hasEnum, "kb_enumerate is not advertised without declared dimensions")
	assert.False(t, hasDims, "kb_dimensions is not advertised without declared dimensions")
	assert.Len(t, got.Defs(), 1, "base tools unchanged")
}

// With dimensions declared, kb_enumerate and kb_dimensions are advertised alongside
// the base tools; kb_enumerate's description names the dimensions and its schema
// constrains them.
func TestWithEnumerateAdvertises(t *testing.T) {
	base := &fakeBase{defs: []core.ToolDef{{Name: "search"}}}
	got := tools.WithEnumerate(base, &fakeEnum{}, []string{"games", "hosts"})

	defs := got.Defs()
	require.Len(t, defs, 3, "base tool + kb_enumerate + kb_dimensions")
	def, has := defByName(defs, "kb_enumerate")
	require.True(t, has)
	assert.Contains(t, def.Description, "games")
	assert.Contains(t, def.Description, "hosts")
	assert.Contains(t, string(def.Schema), "games", "declared dimensions constrain the schema")
	_, hasDims := defByName(defs, "kb_dimensions")
	assert.True(t, hasDims, "kb_dimensions advertised alongside kb_enumerate")
}

// A kb_enumerate call routes to the store and formats the complete result as
// compact Title (path) refs.
func TestEnumerateCallFormatsRefs(t *testing.T) {
	enum := &fakeEnum{refs: []store.DocRef{
		{Path: "ep01.md", Title: "Episode 1"},
		{Path: "ep02.md"}, // no title → path only
	}}
	ts := tools.WithEnumerate(&fakeBase{}, enum, []string{"games"})

	out, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{"dimension": "games", "value": "Acme Rail"})
	require.NoError(t, err)
	assert.Equal(t, "games", enum.dim, "the query reaches the store")
	assert.Equal(t, "Acme Rail", enum.val)
	assert.Contains(t, out, "2 document(s)")
	assert.Contains(t, out, "- Episode 1 (ep01.md)", "a titled ref shows title and path")
	assert.Contains(t, out, "- ep02.md", "an untitled ref shows the path")
	assert.NotContains(t, strings.ToLower(out), "chunk", "refs, not chunk bodies")
}

// An empty result is stated, not an error.
func TestEnumerateCallEmpty(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, &fakeEnum{refs: nil}, []string{"games"})
	out, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{"dimension": "games", "value": "Nope"})
	require.NoError(t, err)
	assert.Contains(t, out, "No documents found")
}

// An undeclared dimension or a missing argument is a loud error, not a silent
// empty (the model gets the error back and can adapt).
func TestEnumerateCallRejectsBadArgs(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, &fakeEnum{}, []string{"games"})

	_, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{"dimension": "publishers", "value": "x"})
	assert.ErrorContains(t, err, "unknown dimension")

	_, err = ts.Call(context.Background(), "kb_enumerate", map[string]any{"dimension": "games"})
	assert.ErrorContains(t, err, "required")
}

// A non-enumerate tool call delegates to the base tool set.
func TestEnumerateDelegatesToBase(t *testing.T) {
	base := &fakeBase{}
	ts := tools.WithEnumerate(base, &fakeEnum{}, []string{"games"})
	out, err := ts.Call(context.Background(), "search", map[string]any{"q": "x"})
	require.NoError(t, err)
	assert.Equal(t, "base:search", out)
	assert.Equal(t, "search", base.called, "the base handled its own tool")
}

// kb_enumerate composes facets: the 'and' filters intersect with the primary
// predicate, so only documents matching ALL are returned, in the primary's order
// (ADR 0020). Uses a real memory store so the intersection is genuinely exercised.
func TestEnumerateMultiPredicateIntersects(t *testing.T) {
	m := store.NewMemory(nil, 0)
	require.NoError(t, m.Index(context.Background(), []store.Doc{
		{Ref: store.DocRef{Path: "g1.md", Title: "G1"}, Dimensions: map[string][]string{"category": {"Trains"}, "players": {"2"}}},
		{Ref: store.DocRef{Path: "g2.md", Title: "G2"}, Dimensions: map[string][]string{"category": {"Trains"}, "players": {"4"}}},
		{Ref: store.DocRef{Path: "g3.md", Title: "G3"}, Dimensions: map[string][]string{"category": {"Trains"}, "players": {"2"}}},
	}))
	ts := tools.WithEnumerate(&fakeBase{}, m, []string{"category", "players"})

	out, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{
		"dimension": "category", "value": "Trains",
		"and": []any{map[string]any{"dimension": "players", "value": "2"}},
	})
	require.NoError(t, err)
	assert.Contains(t, out, "2 document(s)")
	assert.Contains(t, out, `category = "Trains" AND players = "2"`, "the compound predicate is described")
	assert.Contains(t, out, "g1.md")
	assert.Contains(t, out, "g3.md")
	assert.NotContains(t, out, "g2.md", "the 4-player Trains game is excluded by the AND")
}

// An 'and' filter naming an undeclared dimension is a loud error (validated before
// any lookup).
func TestEnumerateMultiPredicateRejectsUnknownDim(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, &fakeEnum{}, []string{"category"})
	_, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{
		"dimension": "category", "value": "Trains",
		"and": []any{map[string]any{"dimension": "players", "value": "2"}},
	})
	assert.ErrorContains(t, err, "unknown dimension")
}

// A malformed 'and' — not a list, or a filter missing a field — is a loud error.
func TestEnumerateMultiPredicateRejectsMalformed(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, &fakeEnum{}, []string{"category", "players"})

	_, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{
		"dimension": "category", "value": "Trains", "and": "players:2",
	})
	assert.ErrorContains(t, err, "'and' must be a list")

	_, err = ts.Call(context.Background(), "kb_enumerate", map[string]any{
		"dimension": "category", "value": "Trains",
		"and": []any{map[string]any{"dimension": "players"}},
	})
	assert.ErrorContains(t, err, "both dimension and value")
}

// The schema and description advertise the compound 'and' filter.
func TestEnumerateAdvertisesAndFilter(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, &fakeEnum{}, []string{"category", "players"})
	def, has := defByName(ts.Defs(), "kb_enumerate")
	require.True(t, has)
	assert.Contains(t, string(def.Schema), "and", "compound 'and' filter in schema")
	assert.Contains(t, def.Description, "and", "description mentions compound facets")
}
