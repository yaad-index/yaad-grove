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

	docs, err := retrieval.VaultDocs(context.Background(), dir, []string{"games"})
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

func refPaths(refs []store.DocRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Path
	}
	return out
}
