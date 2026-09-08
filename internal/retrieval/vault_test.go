package retrieval_test

import (
	"context"
	"os"
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
	docs, err := retrieval.VaultDocs(context.Background(), vault(), nil, nil)
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
	docs, err := retrieval.VaultDocs(context.Background(), vault(), nil, nil)
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

// Frontmatter is parsed (ADR 0019): the note's title becomes its canonical name,
// the declared dimension fields become queryable values (scalar or list), and
// `aliases` + any `name_<lang>` field become alias surface forms. A field not in
// the declared set is ignored.
func TestVaultDocsExtractsFrontmatter(t *testing.T) {
	dir := t.TempDir()
	note := "---\n" +
		"title: Acme Rail\n" +
		"games: [Acme Rail, Widget Wars]\n" +
		"host: Dana\n" +
		"aliases: [acme-rail]\n" +
		"name_fa: اکمی ریل\n" +
		"secret: ignored\n" +
		"---\n# Body\ncontent here\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "n.md"), []byte(note), 0o600))

	docs, err := retrieval.VaultDocs(context.Background(), dir, []string{"games", "host"}, nil)
	require.NoError(t, err)
	require.Len(t, docs, 1)
	d := docs[0]

	assert.Equal(t, "Acme Rail", d.Ref.Title, "title is the canonical name")
	assert.Equal(t, []string{"Acme Rail", "Widget Wars"}, d.Dimensions["games"], "a list dimension")
	assert.Equal(t, []string{"Dana"}, d.Dimensions["host"], "a scalar dimension becomes a one-element slice")
	assert.NotContains(t, d.Dimensions, "secret", "an undeclared field is not indexed")
	assert.ElementsMatch(t, []string{"acme-rail", "اکمی ریل"}, d.Aliases, "aliases list + name_<lang> both collected")
	assert.NotContains(t, d.Chunks[0].Text, "Acme Rail", "frontmatter is stripped from chunk text")
}

// Malformed frontmatter fails loudly (a KB typo shouldn't silently drop a note's
// structured data).
func TestVaultDocsMalformedFrontmatterErrors(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.md"),
		[]byte("---\ngames: [unterminated\n---\n# Body\nx\n"), 0o600))
	_, err := retrieval.VaultDocs(context.Background(), dir, []string{"games"}, nil)
	assert.Error(t, err, "malformed frontmatter is a startup error")
}

// A missing vault dir is an error; an empty (but existing) corpus is not.
func TestVaultDocsDirCases(t *testing.T) {
	_, err := retrieval.VaultDocs(context.Background(), filepath.Join(t.TempDir(), "nope"), nil, nil)
	assert.Error(t, err, "missing vault dir is an error")

	docs, err := retrieval.VaultDocs(context.Background(), t.TempDir(), nil, nil) // exists, no .md files
	require.NoError(t, err, "an empty corpus is not an error")
	assert.Empty(t, docs)
}

// Output is deterministic for a given corpus (no walk-order leak).
func TestVaultDocsDeterministic(t *testing.T) {
	a, err := retrieval.VaultDocs(context.Background(), vault(), nil, nil)
	require.NoError(t, err)
	b, err := retrieval.VaultDocs(context.Background(), vault(), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, a, b)
}

// --- orderable fields (ADR 0022) ---

// orderedVault writes a vault covering every shape a declared orderable field
// arrives in: a plain number, an unquoted date (which YAML hands back as a
// time.Time), a quoted date (which stays a string), an unreadable value, and a
// note that simply does not carry the field.
func orderedVault(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"n1.md":      "---\ntitle: One\nepisode: 10\n---\nbody one\n",
		"n2.md":      "---\ntitle: Two\nepisode: 40\n---\nbody two\n",
		"n3.md":      "---\ntitle: Three\nepisode: \"7\"\n---\nbody three\n",
		"bad.md":     "---\ntitle: Bad\nepisode: not a number\n---\nbody bad\n",
		"missing.md": "---\ntitle: Missing\n---\nbody missing\n",
		"dated.md":   "---\ntitle: Dated\npublished: 2026-01-05\n---\nbody dated\n",
		"quoted.md":  "---\ntitle: Quoted\npublished: \"2026-02-05\"\n---\nbody quoted\n",
	}
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600))
	}
	return dir
}

func docByPath(t *testing.T, docs []store.Doc, path string) store.Doc {
	t.Helper()
	for _, d := range docs {
		if d.Ref.Path == path {
			return d
		}
	}
	t.Fatalf("no doc %q", path)
	return store.Doc{}
}

// Numbers are captured whether written bare or quoted.
func TestVaultDocsCapturesNumericOrderableFields(t *testing.T) {
	docs, err := retrieval.VaultDocs(context.Background(), orderedVault(t), nil, []string{"episode"})
	require.NoError(t, err)

	assert.Equal(t, map[string]float64{"episode": 10}, docByPath(t, docs, "n1.md").Ordered)
	assert.Equal(t, map[string]float64{"episode": 40}, docByPath(t, docs, "n2.md").Ordered)
	assert.Equal(t, map[string]float64{"episode": 7}, docByPath(t, docs, "n3.md").Ordered, "a quoted number still orders")
}

// An unquoted date reaches us as a time.Time and a quoted one as a string. Both
// must index, and the earlier date must sort below the later one — asserted as an
// ORDER rather than as two magic numbers, so the test says what it means.
func TestVaultDocsCapturesDatesQuotedOrNot(t *testing.T) {
	docs, err := retrieval.VaultDocs(context.Background(), orderedVault(t), nil, []string{"published"})
	require.NoError(t, err)

	dated := docByPath(t, docs, "dated.md").Ordered["published"]
	quoted := docByPath(t, docs, "quoted.md").Ordered["published"]
	require.NotZero(t, dated, "an unquoted YAML date must index")
	require.NotZero(t, quoted, "a quoted date must index too")
	assert.Less(t, dated, quoted, "January sorts before February")
}

// A note that does not carry the field has no value AND no skip: it is not a
// failure, it simply has nothing to order by.
func TestAFieldTheNoteDoesNotCarryIsNeitherValueNorFailure(t *testing.T) {
	docs, err := retrieval.VaultDocs(context.Background(), orderedVault(t), nil, []string{"episode"})
	require.NoError(t, err)

	missing := docByPath(t, docs, "missing.md")
	assert.Empty(t, missing.Ordered)
	assert.Empty(t, missing.OrderedSkipped, "absent is not the same as unreadable")
}

// A note that DOES carry the field but whose value cannot be read is recorded as
// skipped. That record is the only thing separating "this vault has no dates" from
// "every date in this vault is misspelled" — the counts are equal-looking
// otherwise, and both produce an empty order.
func TestAnUnreadableValueIsSkippedAndNamed(t *testing.T) {
	docs, err := retrieval.VaultDocs(context.Background(), orderedVault(t), nil, []string{"episode"})
	require.NoError(t, err)

	bad := docByPath(t, docs, "bad.md")
	assert.Empty(t, bad.Ordered)
	assert.Equal(t, []string{"episode"}, bad.OrderedSkipped)
}

// A field whose values mix kinds keeps its first-seen kind and refuses the rest: a
// date is keyed in Unix seconds and a number in its own units, so mixing them
// sorts every date above every number regardless of what either means.
func TestAFieldMixingDatesAndNumbersRefusesTheLateArrival(t *testing.T) {
	dir := t.TempDir()
	// a.md is read first (lexical walk order), so the field is a number from there on.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.md"), []byte("---\ntitle: A\nwhen: 3\n---\nbody\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.md"), []byte("---\ntitle: B\nwhen: 2026-01-05\n---\nbody\n"), 0o600))

	docs, err := retrieval.VaultDocs(context.Background(), dir, nil, []string{"when"})
	require.NoError(t, err)

	assert.Equal(t, map[string]float64{"when": 3}, docByPath(t, docs, "a.md").Ordered)
	assert.Empty(t, docByPath(t, docs, "b.md").Ordered, "a date cannot join a numeric field")
	assert.Equal(t, []string{"when"}, docByPath(t, docs, "b.md").OrderedSkipped, "and the refusal is recorded")
}

// Declaring no orderable fields leaves every document with no ordered values —
// today's behaviour, not an error.
func TestNoOrderableFieldsDeclaredIsNotAnError(t *testing.T) {
	docs, err := retrieval.VaultDocs(context.Background(), orderedVault(t), nil, nil)
	require.NoError(t, err)
	require.NotEmpty(t, docs)

	for _, d := range docs {
		assert.Empty(t, d.Ordered, d.Ref.Path)
		assert.Empty(t, d.OrderedSkipped, d.Ref.Path)
	}
}

// Dimensions and orderable fields are independent: declaring one does not disturb
// the other, and a field can legitimately be both.
func TestOrderableAndDimensionsCoexist(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.md"),
		[]byte("---\ntitle: A\nepisode: 12\ngames: [acme-rail]\n---\nbody\n"), 0o600))

	docs, err := retrieval.VaultDocs(context.Background(), dir, []string{"games"}, []string{"episode"})
	require.NoError(t, err)
	require.Len(t, docs, 1)

	assert.Equal(t, map[string][]string{"games": {"acme-rail"}}, docs[0].Dimensions)
	assert.Equal(t, map[string]float64{"episode": 12}, docs[0].Ordered)
}
