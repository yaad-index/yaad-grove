package retrieval_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/retrieval"
	"github.com/yaad-index/yaad-grove/internal/store"
)

func vault() string { return filepath.Join("testdata", "vault") }

// flatten returns every doc's chunks in doc order — the flat stream the store
// indexes over.
func flatten(docs []store.Doc) []core.Chunk {
	var out []core.Chunk
	for _, d := range docs {
		out = append(out, d.Chunks...)
	}
	return out
}

// VaultDocs scans recursively, skips dot-dirs, strips frontmatter, splits on
// headings, and yields vault-relative slash sources with a #heading anchor.
func TestVaultDocsScansAndChunks(t *testing.T) {
	docs, err := retrieval.VaultDocs(context.Background(), vault())
	require.NoError(t, err)
	require.NotEmpty(t, docs)
	chunks := flatten(docs)
	require.NotEmpty(t, chunks)

	var foundNested, foundHeadingAnchor bool
	for _, c := range chunks {
		assert.False(t, filepath.IsAbs(c.Source), "source %q must be relative", c.Source)
		assert.NotContains(t, c.Source, "testdata", "source is vault-relative")
		assert.NotContains(t, c.Source, ".obsidian", "dot-dirs are skipped")
		assert.NotContains(t, c.Text, "zzqqxx", "a frontmatter-only term is stripped from the index")
		if strings.HasPrefix(c.Source, "notes/deep.md") {
			foundNested = true
		}
		if strings.Contains(c.Source, "install.md#") {
			foundHeadingAnchor = true
		}
	}
	assert.True(t, foundNested, "a nested file is scanned (recursive)")
	assert.True(t, foundHeadingAnchor, "heading chunks carry a path#heading source")
}

// Chunks are grouped per source note: a headed file yields several chunks under
// one Doc, a heading-less file yields a single anchor-free chunk.
func TestVaultDocsGroupsPerNote(t *testing.T) {
	docs, err := retrieval.VaultDocs(context.Background(), vault())
	require.NoError(t, err)

	byPath := map[string]store.Doc{}
	for _, d := range docs {
		byPath[d.Ref.Path] = d
	}

	install, ok := byPath["install.md"]
	require.True(t, ok, "install.md is a doc")
	assert.Greater(t, len(install.Chunks), 1, "a headed file splits into several chunks")

	deep, ok := byPath["notes/deep.md"]
	require.True(t, ok, "notes/deep.md is a doc")
	require.Len(t, deep.Chunks, 1, "a heading-less file is one chunk")
	assert.Equal(t, "notes/deep.md", deep.Chunks[0].Source, "no #anchor without a heading")
}

// A missing vault dir is an error; an empty (but existing) corpus is not.
func TestVaultDocsDirCases(t *testing.T) {
	_, err := retrieval.VaultDocs(context.Background(), filepath.Join(t.TempDir(), "nope"))
	assert.Error(t, err, "missing vault dir is an error")

	docs, err := retrieval.VaultDocs(context.Background(), t.TempDir()) // exists, no .md files
	require.NoError(t, err, "an empty corpus is not an error")
	assert.Empty(t, docs)
}

// Output is deterministic for a given corpus (no walk-order leak).
func TestVaultDocsDeterministic(t *testing.T) {
	a, err := retrieval.VaultDocs(context.Background(), vault())
	require.NoError(t, err)
	b, err := retrieval.VaultDocs(context.Background(), vault())
	require.NoError(t, err)
	assert.Equal(t, a, b)
}
