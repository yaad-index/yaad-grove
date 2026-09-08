package tools_test

import (
	"context"
	"encoding/json"
	"sort"
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

	// keys is field → path → sort key. Ordered sorts BY THE KEY rather than
	// returning a list in the order a test wrote it, so an assertion about "the
	// latest" is answered by the data and cannot pass by fixture order. A double
	// that replies by position lets a test point at the wrong document and still go
	// green.
	keys map[string]map[string]float64
	// ordCalls records every Ordered call, so a test can assert what the tool ASKED
	// for — the limit in particular, which must not be pushed down alongside a facet.
	ordCalls []ordCall
	// enumCalls counts Enumerate calls, to show the facet legs still run.
	enumCalls int
	// all is every document in the knowledge base. It is deliberately SEPARATE from
	// refs (what Enumerate returns for a facet), because the case worth testing is
	// the one where the globally highest-ordered document does NOT match the facet.
	// Ordering over the facet result instead would make that case inexpressible.
	all []store.DocRef
}

type ordCall struct {
	field string
	dir   store.Direction
	limit int
}

func (f *fakeEnum) Enumerate(_ context.Context, dimension, value string) ([]store.DocRef, error) {
	f.dim, f.val = dimension, value
	f.enumCalls++
	return f.refs, nil
}

func (f *fakeEnum) Dimensions(context.Context) (map[string][]string, error) {
	return f.vocab, nil
}

// Ordered returns the docs carrying a key for field, sorted by that key — the
// same contract the real backends implement, including the absence of documents
// without a value.
func (f *fakeEnum) Ordered(_ context.Context, field string, dir store.Direction, limit int) ([]store.DocRef, error) {
	f.ordCalls = append(f.ordCalls, ordCall{field: field, dir: dir, limit: limit})
	byPath := f.keys[field]
	if byPath == nil {
		return nil, nil
	}
	universe := f.all
	if universe == nil {
		universe = f.refs
	}
	var out []store.DocRef
	for _, r := range universe {
		if _, ok := byPath[r.Path]; ok {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ki, kj := byPath[out[i].Path], byPath[out[j].Path]
		if ki != kj {
			if dir == store.Ascending {
				return ki < kj
			}
			return ki > kj
		}
		return out[i].Path < out[j].Path
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
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
	got := tools.WithEnumerate(base, &fakeEnum{}, nil, nil)
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
	got := tools.WithEnumerate(base, &fakeEnum{}, []string{"games", "hosts"}, nil)

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
	ts := tools.WithEnumerate(&fakeBase{}, enum, []string{"games"}, nil)

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
	ts := tools.WithEnumerate(&fakeBase{}, &fakeEnum{refs: nil}, []string{"games"}, nil)
	out, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{"dimension": "games", "value": "Nope"})
	require.NoError(t, err)
	assert.Contains(t, out, "No documents found")
}

// An undeclared dimension or a missing argument is a loud error, not a silent
// empty (the model gets the error back and can adapt).
func TestEnumerateCallRejectsBadArgs(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, &fakeEnum{}, []string{"games"}, nil)

	_, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{"dimension": "publishers", "value": "x"})
	assert.ErrorContains(t, err, "unknown dimension")

	_, err = ts.Call(context.Background(), "kb_enumerate", map[string]any{"dimension": "games"})
	assert.ErrorContains(t, err, "required")
}

// A non-enumerate tool call delegates to the base tool set.
func TestEnumerateDelegatesToBase(t *testing.T) {
	base := &fakeBase{}
	ts := tools.WithEnumerate(base, &fakeEnum{}, []string{"games"}, nil)
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
	ts := tools.WithEnumerate(&fakeBase{}, m, []string{"category", "players"}, nil)

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
	ts := tools.WithEnumerate(&fakeBase{}, &fakeEnum{}, []string{"category"}, nil)
	_, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{
		"dimension": "category", "value": "Trains",
		"and": []any{map[string]any{"dimension": "players", "value": "2"}},
	})
	assert.ErrorContains(t, err, "unknown dimension")
}

// A malformed 'and' — not a list, or a filter missing a field — is a loud error.
func TestEnumerateMultiPredicateRejectsMalformed(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, &fakeEnum{}, []string{"category", "players"}, nil)

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
	ts := tools.WithEnumerate(&fakeBase{}, &fakeEnum{}, []string{"category", "players"}, nil)
	def, has := defByName(ts.Defs(), "kb_enumerate")
	require.True(t, has)
	assert.Contains(t, string(def.Schema), "and", "compound 'and' filter in schema")
	assert.Contains(t, def.Description, "and", "description mentions compound facets")
}

// --- ordered recall (ADR 0022) ---

// episodes is a small knowledge base where the ordering and the facet DISAGREE:
// ep50 is the newest overall but is not a train episode, so any implementation
// that sorts and caps before filtering will miss ep40 and answer with nothing.
func episodes() *fakeEnum {
	all := []store.DocRef{
		{Path: "ep10.md", Title: "Episode 10"},
		{Path: "ep40.md", Title: "Episode 40"},
		{Path: "ep50.md", Title: "Episode 50"},
	}
	return &fakeEnum{
		all: all,
		// The facet "trains" matches ep10 and ep40 only — NOT the newest, ep50.
		refs: []store.DocRef{{Path: "ep10.md", Title: "Episode 10"}, {Path: "ep40.md", Title: "Episode 40"}},
		keys: map[string]map[string]float64{
			"episode": {"ep10.md": 10, "ep40.md": 40, "ep50.md": 50},
		},
	}
}

// THE load-bearing case. "The latest train episode" must be ep40 — the newest
// document that actually matches — not empty, and not the globally newest.
//
// Capping to 1 before intersecting yields ep50, which the facet then removes,
// leaving nothing while a correct answer exists further down the order. That is
// the partial-view defect ADR 0022 removes, rebuilt one layer below it.
func TestOrderedWithAFacetCapsAfterIntersecting(t *testing.T) {
	enum := episodes()
	ts := tools.WithEnumerate(&fakeBase{}, enum, []string{"category"}, []string{"episode"})

	out, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{
		"dimension": "category", "value": "trains",
		"sort": "episode", "limit": float64(1),
	})
	require.NoError(t, err)

	assert.Contains(t, out, "Episode 40", "the newest MATCHING document")
	assert.NotContains(t, out, "Episode 50", "the newest overall does not match the facet")
	assert.NotContains(t, out, "Episode 10", "capped to one")
}

// The mechanism behind the case above: with a facet in play the tool must ask the
// store for the WHOLE order (limit 0) and cap afterwards itself. Asserting the
// argument pins the reason, so a future refactor that reintroduces the pushdown
// fails here and names why, rather than only failing the case above.
func TestOrderedNeverPushesTheLimitDownAlongsideAFacet(t *testing.T) {
	enum := episodes()
	ts := tools.WithEnumerate(&fakeBase{}, enum, []string{"category"}, []string{"episode"})

	_, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{
		"dimension": "category", "value": "trains",
		"sort": "episode", "limit": float64(1),
	})
	require.NoError(t, err)

	require.Len(t, enum.ordCalls, 1)
	assert.Equal(t, 0, enum.ordCalls[0].limit, "the store must be asked for the complete order")
	assert.Positive(t, enum.enumCalls, "the facet leg still runs")
}

// With no facet there is nothing to intersect, so the cap is safe to push into the
// store — and should be, so a large collection does not travel just to be trimmed.
func TestOrderedWithoutAFacetPushesTheLimitDown(t *testing.T) {
	enum := episodes()
	ts := tools.WithEnumerate(&fakeBase{}, enum, nil, []string{"episode"})

	out, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{
		"sort": "episode", "limit": float64(1),
	})
	require.NoError(t, err)

	require.Len(t, enum.ordCalls, 1)
	assert.Equal(t, 1, enum.ordCalls[0].limit)
	assert.Zero(t, enum.enumCalls, "no facet means no enumerate leg")
	assert.Contains(t, out, "Episode 50", "the newest in the whole collection")
}

// A bare "what is the latest" carries no facet at all. Extending kb_enumerate
// rather than adding a second tool means dimension/value must become conditional;
// this is the case that would break if they stayed required.
func TestSortingMakesTheFacetArgumentsOptional(t *testing.T) {
	enum := episodes()
	ts := tools.WithEnumerate(&fakeBase{}, enum, []string{"category"}, []string{"episode"})

	out, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{"sort": "episode"})
	require.NoError(t, err)
	assert.Contains(t, out, "Episode 50")

	def, has := defByName(ts.Defs(), "kb_enumerate")
	require.True(t, has)
	// Checked on the parsed TOP level: the nested 'and' filters still require both
	// of their fields, so a substring search over the whole schema would match that
	// and pass for the wrong reason.
	var schema map[string]any
	require.NoError(t, json.Unmarshal(def.Schema, &schema))
	assert.NotContains(t, schema, "required", "no top-level argument is required once sorting exists")
}

// Without a sort they stay required: relaxing them unconditionally would turn a
// malformed filter into a silent request for the whole collection.
func TestWithoutSortingTheFacetArgumentsStayRequired(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, episodes(), []string{"category"}, []string{"episode"})

	_, err := ts.Call(context.Background(), "kb_enumerate", nil)
	assert.ErrorContains(t, err, "both dimension and value")
}

// Half a filter is malformed whether or not a sort is present. Treating it as "no
// filter" would answer a wider question than the one asked.
func TestHalfAFilterIsAnErrorEvenWhenSorting(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, episodes(), []string{"category"}, []string{"episode"})

	_, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{
		"dimension": "category", "sort": "episode",
	})
	assert.ErrorContains(t, err, "both dimension and value")
}

// A bare "latest" is descending (decided by the maintainer), and asc is honoured
// when asked for. Both directions are checked: one of them passing proves nothing
// about the other, and a default that silently inverted would still satisfy a
// single-direction test.
func TestDirectionDefaultsToDescendingAndAscendingIsHonoured(t *testing.T) {
	enum := episodes()
	ts := tools.WithEnumerate(&fakeBase{}, enum, nil, []string{"episode"})

	out, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{"sort": "episode", "limit": float64(1)})
	require.NoError(t, err)
	assert.Contains(t, out, "Episode 50", "default is newest first")
	assert.Equal(t, store.Descending, enum.ordCalls[0].dir)

	out, err = ts.Call(context.Background(), "kb_enumerate", map[string]any{
		"sort": "episode", "direction": "asc", "limit": float64(1),
	})
	require.NoError(t, err)
	assert.Contains(t, out, "Episode 10", "ascending is oldest first")
	assert.Equal(t, store.Ascending, enum.ordCalls[1].dir)
}

// An unknown sort field is refused by name rather than quietly ignored: ignoring
// it would answer a recency question in document order and look like an answer.
func TestUnknownSortFieldIsRefused(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, episodes(), []string{"category"}, []string{"episode"})

	_, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{"sort": "published"})
	assert.ErrorContains(t, err, "unknown sort field")
}

// With no orderable fields declared, sorting is not advertised and a sort argument
// is refused — rather than silently returning an unordered set that reads as one.
func TestWithoutOrderableFieldsSortingIsUnavailable(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, episodes(), []string{"category"}, nil)

	def, has := defByName(ts.Defs(), "kb_enumerate")
	require.True(t, has)
	assert.NotContains(t, string(def.Schema), `"sort"`)
	assert.NotContains(t, def.Description, "the latest")

	_, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{
		"dimension": "category", "value": "trains", "sort": "episode",
	})
	assert.ErrorContains(t, err, "no orderable fields")
}

// Orderable fields alone are enough to expose the tool: "the latest one" is a
// legitimate question in a deployment that declares no facets at all. kb_dimensions
// stays unadvertised there, having no vocabulary to list.
func TestOrderableFieldsAloneAdvertiseTheTool(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, episodes(), nil, []string{"episode"})

	_, has := defByName(ts.Defs(), "kb_enumerate")
	assert.True(t, has, "ordered recall needs the tool even with no dimensions")
	_, hasDims := defByName(ts.Defs(), "kb_dimensions")
	assert.False(t, hasDims, "no dimensions means no vocabulary to list")
}

// A limit must be a positive whole number. Accepting 0 or a fraction would read as
// "uncapped" and return the whole collection to a question that asked for one.
func TestLimitMustBeAPositiveWholeNumber(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, episodes(), nil, []string{"episode"})

	for _, bad := range []any{float64(0), float64(-3), float64(1.5), "many", true} {
		_, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{"sort": "episode", "limit": bad})
		assert.Error(t, err, "limit %v must be refused", bad)
	}
	// A model that sends the number as a string still gets the cap it asked for.
	out, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{"sort": "episode", "limit": "1"})
	require.NoError(t, err)
	assert.Contains(t, out, "Episode 50")
	assert.NotContains(t, out, "Episode 40")
}

// An invalid direction is refused rather than falling back to the default: a
// caller asking for "oldest" and silently getting newest is answered wrongly.
func TestInvalidDirectionIsRefused(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, episodes(), nil, []string{"episode"})

	_, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{"sort": "episode", "direction": "sideways"})
	assert.ErrorContains(t, err, "direction must be")
}

// Documents carrying no value for the sort field are absent from an ordered
// answer, and the empty result says SO — "none carry a usable value" rather than
// "nothing found", because a document can match every facet and still be
// unorderable, and those are different facts.
func TestOrderedSaysWhenNothingCarriesTheField(t *testing.T) {
	enum := episodes()
	enum.keys = map[string]map[string]float64{"episode": {}}
	ts := tools.WithEnumerate(&fakeBase{}, enum, nil, []string{"episode"})

	out, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{"sort": "episode"})
	require.NoError(t, err)
	assert.Contains(t, out, "usable episode value")
}

// The advertised description carries the recency and ordinal phrasings — the
// in-repo half of the routing. It is asserted as text because no unit test can
// pin a live model's tool choice (ADR 0022's acceptance note).
func TestOrderedRoutingTextIsAdvertised(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, episodes(), []string{"category"}, []string{"episode"})
	def, has := defByName(ts.Defs(), "kb_enumerate")
	require.True(t, has)

	for _, phrase := range []string{"the latest", "the most recent", "the newest", "the oldest", "the Nth"} {
		assert.Contains(t, def.Description, phrase)
	}
	assert.Contains(t, def.Description, "episode", "the orderable fields are named")
}
