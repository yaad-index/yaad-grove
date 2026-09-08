package retrieval_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/retrieval"
	"github.com/yaad-index/yaad-grove/internal/store"
	"github.com/yaad-index/yaad-grove/internal/tools"
)

// End to end over a real on-disk vault: the "one entity reviewed across multiple
// documents" shape this whole increment exists to answer. A fictional game
// (rule-9 slug) is referenced by two episode notes; the game's own note declares a
// cross-script alias. VaultDocs → Memory.Index → Enumerate returns the COMPLETE set
// by both the canonical name and the alias — the completeness top-k retrieval
// structurally can't deliver. This is the integration seam where a wiring bug
// (frontmatter parse, dimension index, alias map, normalization) would hide.
func TestEnumerateEndToEnd(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	// The entity note: canonical title + a Persian transliteration alias.
	write("games/acme-rail.md", "---\ntitle: Acme Rail\nname_fa: اکمی ریل\n---\n# Acme Rail\nA cube-rails game.\n")
	// Two episodes review it (in different casing/hyphenation), one does not.
	write("episodes/ep-01.md", "---\ntitle: Episode 1\ngames: [Acme Rail]\n---\n# Episode 1\nWe review it.\n")
	write("episodes/ep-02.md", "---\ntitle: Episode 2\ngames: [acme-rail, Widget Wars]\n---\n# Episode 2\nMore, plus another.\n")
	write("episodes/ep-03.md", "---\ntitle: Episode 3\ngames: [Widget Wars]\n---\n# Episode 3\nA different game.\n")

	docs, err := retrieval.VaultDocs(context.Background(), dir, []string{"games"}, nil)
	require.NoError(t, err)

	mem := store.NewMemory(nil, 0) // a keyword-only backend is enough for enumerate
	require.NoError(t, mem.Index(context.Background(), docs))

	// The canonical query returns exactly the two reviewing episodes — complete, and
	// matching across the ep-02 hyphenated spelling via normalization.
	byName, err := mem.Enumerate(context.Background(), "games", "Acme Rail")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"episodes/ep-01.md", "episodes/ep-02.md"}, refPaths(byName),
		"the complete set of docs reviewing the entity, spelling-insensitive")

	// The cross-script alias resolves to the same complete set — the motivating case.
	byAlias, err := mem.Enumerate(context.Background(), "games", "اکمی ریل")
	require.NoError(t, err)
	assert.ElementsMatch(t, refPaths(byName), refPaths(byAlias), "the alias reaches the same entity")
}

// Faceted recall on a dimension VALUE (#135), end to end. Two resolution gaps the
// motivating "train games / قطاری" case needs closed: (1) a cross-script value —
// a category note carries the transliteration alias, so querying the facet in
// Persian reaches the canonical value; (2) a punctuated value — the normalizer
// folds "/" so "Route/Network Building" is reachable spelled plainly. Both resolve
// to the complete set over a real on-disk vault.
func TestEnumerateFacetedValueResolution(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	// A category note is the facet entity: canonical value + cross-script alias.
	write("facets/trains.md", "---\ntitle: Trains\nname_fa: قطاری\n---\n# Trains\n")
	// Games tagged with the facet value (one via a punctuated category too).
	write("games/g1.md", "---\ntitle: G1\ncategory: [Trains]\n---\n# G1\n")
	write("games/g2.md", "---\ntitle: G2\ncategory: [Trains, \"Route/Network Building\"]\n---\n# G2\n")
	write("games/g3.md", "---\ntitle: G3\ncategory: [\"Route/Network Building\"]\n---\n# G3\n")

	docs, err := retrieval.VaultDocs(context.Background(), dir, []string{"category"}, nil)
	require.NoError(t, err)
	mem := store.NewMemory(nil, 0)
	require.NoError(t, mem.Index(context.Background(), docs))

	// Canonical facet value → the complete set carrying it.
	byCanon, err := mem.Enumerate(context.Background(), "category", "Trains")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"games/g1.md", "games/g2.md"}, refPaths(byCanon), "complete set for the canonical value")

	// The cross-script spelling of the facet value resolves to the same set.
	byCrossScript, err := mem.Enumerate(context.Background(), "category", "قطاری")
	require.NoError(t, err)
	assert.ElementsMatch(t, refPaths(byCanon), refPaths(byCrossScript), "the Persian facet spelling reaches the canonical value")

	// A punctuated value is reachable spelled without its punctuation (the fold is
	// symmetric, so the slash form and the plain form share one key).
	byPlain, err := mem.Enumerate(context.Background(), "category", "route network building")
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"games/g2.md", "games/g3.md"}, refPaths(byPlain), "punctuation-folded value resolves the complete set")
}

func refPaths(refs []store.DocRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Path
	}
	return out
}

// End to end for ordered recall (ADR 0022): a real vault, read by the real
// frontmatter parser, indexed by the real store, queried through the real tool.
//
// This is the seam a wiring bug hides in — each layer can be right on its own
// while the parse, the index and the cap disagree about which document is newest.
// The vault is built so that the newest episode overall is NOT the newest one
// about the game, so an answer that skips the intersection is visibly wrong rather
// than plausibly right.
func TestOrderedRecallEndToEnd(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	write("episodes/ep-01.md", "---\ntitle: Episode 1\nepisode: 1\ngames: [acme-rail]\n---\n# One\nWe review it.\n")
	write("episodes/ep-07.md", "---\ntitle: Episode 7\nepisode: 7\ngames: [acme-rail]\n---\n# Seven\nAgain.\n")
	// The newest episode overall — and it is about a different game.
	write("episodes/ep-09.md", "---\ntitle: Episode 9\nepisode: 9\ngames: [widget-wars]\n---\n# Nine\nSomething else.\n")
	// Carries no episode number at all: it must never be "the latest".
	write("episodes/extra.md", "---\ntitle: Bonus\ngames: [acme-rail]\n---\n# Bonus\nUnnumbered.\n")

	docs, err := retrieval.VaultDocs(context.Background(), dir, []string{"games"}, []string{"episode"})
	require.NoError(t, err)

	mem := store.NewMemory(nil, 0)
	require.NoError(t, mem.Index(context.Background(), docs))
	ts := tools.WithEnumerate(nil, mem, []string{"games"}, []string{"episode"})

	// "The latest episode about acme-rail" is 7 — not 9, which is newer but about
	// another game, and not the unnumbered bonus.
	out, err := ts.Call(context.Background(), "kb_enumerate", map[string]any{
		"dimension": "games", "value": "acme-rail",
		"sort": "episode", "limit": float64(1),
	})
	require.NoError(t, err)
	assert.Contains(t, out, "Episode 7")
	assert.NotContains(t, out, "Episode 9", "newer, but not about this game")
	assert.NotContains(t, out, "Bonus", "no episode number means it cannot be the latest")

	// Without the facet, the latest is 9 — so the facet is genuinely doing work
	// above, rather than the answer being the global maximum by coincidence.
	out, err = ts.Call(context.Background(), "kb_enumerate", map[string]any{
		"sort": "episode", "limit": float64(1),
	})
	require.NoError(t, err)
	assert.Contains(t, out, "Episode 9")
}
