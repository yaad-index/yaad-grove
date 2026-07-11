// Package retrieval grounds answers in the curated vault. Phase 1 is plain
// full-text search over a small markdown corpus; an embedding-backed store can
// replace it behind core.Retriever with no engine change (ADR 0001).
//
// The vault it reads is the *curated* one only — never the quarantined log of
// community chatter. That isolation is what keeps the "never out of bounds"
// promise on the data side (ADR 0001, growth loop).
package retrieval

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// defaultMaxChunks caps a retrieval when MaxChunks is unset (<= 0): retrieval
// feeds a bounded prompt, so it is capped rather than unbounded by default.
const defaultMaxChunks = 8

// FullText retrieves by scanning markdown files under a vault root. It
// implements core.Retriever.
type FullText struct {
	// VaultDir is the curated vault root (markdown + YAML frontmatter).
	VaultDir string
	// MaxChunks caps how many chunks a single retrieval returns; <= 0 uses
	// defaultMaxChunks.
	MaxChunks int
}

// New returns a FullText retriever over vaultDir.
func New(vaultDir string, maxChunks int) *FullText {
	return &FullText{VaultDir: vaultDir, MaxChunks: maxChunks}
}

// Retrieve returns up to MaxChunks vault chunks relevant to query, ranked by a
// deterministic term-frequency match.
//
// It scans VaultDir recursively for *.md files (skipping dot-directories like
// .git/.obsidian), splits each into chunks on markdown headings (a chunk is a
// heading and its body; a file with no headings is one chunk), strips YAML
// frontmatter from the indexed text, and scores each chunk by the summed
// case-insensitive frequency of the query terms. Ties break by source path then
// scan order, so output is reproducible for a given corpus+query. An empty query,
// no matches, or an empty corpus returns no chunks (not an error); a missing or
// unreadable VaultDir is an error.
func (f *FullText) Retrieve(ctx context.Context, query string) ([]core.Chunk, error) {
	queryTerms := tokenize(query)

	chunks, err := vaultChunks(ctx, f.VaultDir)
	if err != nil {
		return nil, err
	}

	var scored []scoredChunk
	for order, c := range chunks {
		if s := score(queryTerms, c.Text); s > 0 {
			scored = append(scored, scoredChunk{chunk: c, score: s, order: order})
		}
	}

	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		if scored[i].chunk.Source != scored[j].chunk.Source {
			return scored[i].chunk.Source < scored[j].chunk.Source
		}
		return scored[i].order < scored[j].order
	})

	limit := f.MaxChunks
	if limit <= 0 {
		limit = defaultMaxChunks
	}
	if len(scored) > limit {
		scored = scored[:limit]
	}

	out := make([]core.Chunk, len(scored))
	for i, sc := range scored {
		out[i] = sc.chunk
	}
	return out, nil
}

// vaultChunks walks vaultDir and returns every curated chunk in scan order — the
// shared chunking both the keyword and the semantic retriever build on (ADR
// 0017), so query- and chunk-embedding are over the same units. Each *.md file
// (dot-dirs skipped) has its frontmatter stripped and is split on headings; a
// chunk's Source is the file path, plus "#heading" when it has one, and its Text
// is trimmed. A missing/unreadable vault is an error; an empty vault returns no
// chunks.
func vaultChunks(ctx context.Context, vaultDir string) ([]core.Chunk, error) {
	var chunks []core.Chunk
	err := filepath.WalkDir(vaultDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if d.IsDir() {
			if path != vaultDir && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir // skip dot-dirs (.git, .obsidian, ...)
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(d.Name()), ".md") {
			return nil // markdown only
		}
		content, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(vaultDir, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)

		for _, c := range chunkMarkdown(stripFrontmatter(string(content))) {
			source := rel
			if c.heading != "" {
				source = rel + "#" + c.heading
			}
			chunks = append(chunks, core.Chunk{Source: source, Text: strings.TrimSpace(c.text)})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("retrieval: scan %s: %w", vaultDir, err)
	}
	return chunks, nil
}

// scoredChunk pairs a chunk with its match score and scan order (for a stable,
// deterministic tie-break).
type scoredChunk struct {
	chunk core.Chunk
	score int
	order int
}

// mdChunk is a heading and its body (heading is "" for a file's preamble or a
// file with no headings).
type mdChunk struct {
	heading string
	text    string
}

// chunkMarkdown splits body into chunks at markdown headings: each heading starts
// a new chunk carrying the heading line and the text until the next heading. Text
// before the first heading is its own chunk; a body with no headings is one
// chunk. Whitespace-only chunks are dropped.
func chunkMarkdown(body string) []mdChunk {
	var chunks []mdChunk
	var cur mdChunk
	var b strings.Builder
	flush := func() {
		cur.text = b.String()
		if strings.TrimSpace(cur.text) != "" {
			chunks = append(chunks, cur)
		}
		cur = mdChunk{}
		b.Reset()
	}
	for _, line := range strings.Split(body, "\n") {
		if title, ok := headingTitle(line); ok {
			flush()
			cur.heading = title
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	flush()
	return chunks
}

// headingTitle returns the title of an ATX markdown heading line (1-6 leading
// '#' then a space), or ok=false. "#tag" (no space) is not a heading.
func headingTitle(line string) (string, bool) {
	t := strings.TrimSpace(line)
	n := 0
	for n < len(t) && t[n] == '#' {
		n++
	}
	if n == 0 || n > 6 || n >= len(t) || (t[n] != ' ' && t[n] != '\t') {
		return "", false
	}
	title := strings.TrimSpace(t[n:])
	if title == "" {
		return "", false
	}
	return title, true
}

// stripFrontmatter removes a leading YAML frontmatter block ("---" line, content,
// "---" line) so metadata does not pollute ranking. If there is no frontmatter or
// it is unterminated, the whole content is returned as body (never an error).
func stripFrontmatter(content string) string {
	nl := strings.IndexByte(content, '\n')
	if nl < 0 || strings.TrimRight(content[:nl], "\r") != "---" {
		return content
	}
	lines := strings.Split(content[nl+1:], "\n")
	for i, ln := range lines {
		if strings.TrimRight(ln, "\r") == "---" {
			return strings.Join(lines[i+1:], "\n")
		}
	}
	return content // unterminated → treat all as body
}

// tokenize lowercases and splits on non-alphanumeric runes.
func tokenize(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// score sums, over the query terms, how often each appears in text (case-
// insensitive term frequency). Zero query terms scores zero.
func score(queryTerms []string, text string) int {
	if len(queryTerms) == 0 {
		return 0
	}
	freq := map[string]int{}
	for _, tok := range tokenize(text) {
		freq[tok]++
	}
	total := 0
	for _, q := range queryTerms {
		total += freq[q]
	}
	return total
}

// compile-time assertion that FullText satisfies core.Retriever.
var _ core.Retriever = (*FullText)(nil)
