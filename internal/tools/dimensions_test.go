package tools_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/tools"
)

// kb_dimensions lists the declared dimensions and their values, in declared order,
// so the model can pick a spelling that exists before calling kb_enumerate.
func TestDimensionsCallLists(t *testing.T) {
	enum := &fakeEnum{vocab: map[string][]string{
		"games":    {"Acme Rail", "Widget Wars"},
		"category": {"Route/Network Building", "Trains"},
	}}
	ts := tools.WithEnumerate(&fakeBase{}, enum, []string{"games", "category"})

	out, err := ts.Call(context.Background(), "kb_dimensions", map[string]any{})
	require.NoError(t, err)
	// Declared order (games before category), values comma-joined.
	assert.Contains(t, out, "- games: Acme Rail, Widget Wars")
	assert.Contains(t, out, "- category: Route/Network Building, Trains")
	assert.Less(t, strings.Index(out, "games"), strings.Index(out, "category"), "declared order preserved")
}

// The optional dimension argument narrows the listing to one declared dimension.
func TestDimensionsCallFilterOne(t *testing.T) {
	enum := &fakeEnum{vocab: map[string][]string{
		"games":    {"Acme Rail"},
		"category": {"Trains"},
	}}
	ts := tools.WithEnumerate(&fakeBase{}, enum, []string{"games", "category"})

	out, err := ts.Call(context.Background(), "kb_dimensions", map[string]any{"dimension": "category"})
	require.NoError(t, err)
	assert.Contains(t, out, "category: Trains")
	assert.NotContains(t, out, "games", "the filter drops the other dimensions")
}

// A high-cardinality dimension is listed up to the cap, with the total count so the
// model knows the listing is partial (ADR 0020).
func TestDimensionsCapsHighCardinality(t *testing.T) {
	many := make([]string, 60)
	for i := range many {
		many[i] = fmt.Sprintf("v%02d", i) // sorted, distinct
	}
	enum := &fakeEnum{vocab: map[string][]string{"designer": many}}
	ts := tools.WithEnumerate(&fakeBase{}, enum, []string{"designer"})

	out, err := ts.Call(context.Background(), "kb_dimensions", map[string]any{})
	require.NoError(t, err)
	assert.Contains(t, out, "60 total", "the full count is reported")
	assert.Contains(t, out, "showing 50")
	assert.Contains(t, out, "v49", "the last shown value is present")
	assert.NotContains(t, out, "v50", "values past the cap are omitted")
}

// An empty vocabulary (nothing indexed yet) is stated, not an error.
func TestDimensionsEmptyVocab(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, &fakeEnum{vocab: nil}, []string{"games"})
	out, err := ts.Call(context.Background(), "kb_dimensions", map[string]any{})
	require.NoError(t, err)
	assert.Contains(t, out, "no values indexed")
}

// An undeclared dimension argument is a loud error, not a silent empty.
func TestDimensionsRejectsUnknownDim(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, &fakeEnum{}, []string{"games"})
	_, err := ts.Call(context.Background(), "kb_dimensions", map[string]any{"dimension": "publishers"})
	assert.ErrorContains(t, err, "unknown dimension")
}

// kb_dimensions advertises the declared dimensions and an optional dimension filter.
func TestDimensionsAdvertises(t *testing.T) {
	ts := tools.WithEnumerate(&fakeBase{}, &fakeEnum{}, []string{"games", "hosts"})
	def, has := defByName(ts.Defs(), "kb_dimensions")
	require.True(t, has)
	assert.Contains(t, def.Description, "games")
	assert.Contains(t, def.Description, "hosts")
	assert.Contains(t, string(def.Schema), "games", "the optional dimension arg is constrained to declared dimensions")
}
