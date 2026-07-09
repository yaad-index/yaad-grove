package retrieval_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/retrieval"
)

func vault() string { return filepath.Join("testdata", "vault") }

// A clearly-more-relevant file ranks above a weakly-matching one; a term only in
// frontmatter never matches; and a dot-dir file is skipped even when it matches
// heavily.
func TestRetrieveRanking(t *testing.T) {
	f := retrieval.New(vault(), 10)
	chunks, err := f.Retrieve(context.Background(), "install installation")
	require.NoError(t, err)
	require.NotEmpty(t, chunks)

	// install.md (many mentions) ranks above faq.md (one mention).
	assert.True(t, strings.HasPrefix(chunks[0].Source, "install.md"),
		"top chunk is from install.md, got %q", chunks[0].Source)

	// The .obsidian dot-dir file, though it matches heavily, is never scanned.
	for _, c := range chunks {
		assert.NotContains(t, c.Source, ".obsidian", "dot-dirs are skipped")
	}

	// A term that lives only in YAML frontmatter is stripped from the index.
	fm, err := f.Retrieve(context.Background(), "zzqqxx")
	require.NoError(t, err)
	assert.Empty(t, fm, "a frontmatter-only term is not indexed")
}

// MaxChunks caps the result.
func TestMaxChunksCap(t *testing.T) {
	f := retrieval.New(vault(), 1)
	chunks, err := f.Retrieve(context.Background(), "install installation widget reset")
	require.NoError(t, err)
	assert.Len(t, chunks, 1)
}

// Source is a relative slash path (never an absolute host path), with a #heading
// anchor for heading chunks; nested files are found (recursive scan).
func TestSourceRelativeAndRecursive(t *testing.T) {
	f := retrieval.New(vault(), 10)
	chunks, err := f.Retrieve(context.Background(), "install widget reset gadgets")
	require.NoError(t, err)
	require.NotEmpty(t, chunks)

	foundNested := false
	for _, c := range chunks {
		assert.False(t, filepath.IsAbs(c.Source), "source %q must be relative", c.Source)
		assert.NotContains(t, c.Source, "testdata", "source is vault-relative")
		if strings.HasPrefix(c.Source, "notes/deep.md") {
			foundNested = true
		}
	}
	assert.True(t, foundNested, "a nested file is retrieved (recursive scan)")

	// A heading chunk carries a path#heading source.
	hc, err := f.Retrieve(context.Background(), "installation steps")
	require.NoError(t, err)
	var sources []string
	for _, c := range hc {
		sources = append(sources, c.Source)
	}
	assert.Contains(t, strings.Join(sources, " "), "install.md#", "heading chunks carry a #anchor")
}

// An empty query and a no-match query both return no chunks without error.
func TestEmptyAndNoMatch(t *testing.T) {
	f := retrieval.New(vault(), 10)
	for _, q := range []string{"", "   ", "nonexistentterm12345"} {
		chunks, err := f.Retrieve(context.Background(), q)
		require.NoError(t, err, "query %q", q)
		assert.Empty(t, chunks, "query %q", q)
	}
}

// A missing VaultDir is an error; an empty (but existing) corpus is not.
func TestVaultDirCases(t *testing.T) {
	missing := retrieval.New(filepath.Join(t.TempDir(), "nope"), 10)
	_, err := missing.Retrieve(context.Background(), "install")
	assert.Error(t, err, "missing vault dir is an error")

	empty := retrieval.New(t.TempDir(), 10) // exists, no .md files
	chunks, err := empty.Retrieve(context.Background(), "install")
	require.NoError(t, err, "an empty corpus is not an error")
	assert.Empty(t, chunks)
}

// Output is deterministic for a given corpus+query (no map-order leak).
func TestDeterministic(t *testing.T) {
	f := retrieval.New(vault(), 10)
	a, err := f.Retrieve(context.Background(), "install widget reset")
	require.NoError(t, err)
	b, err := f.Retrieve(context.Background(), "install widget reset")
	require.NoError(t, err)
	assert.Equal(t, a, b)
}
