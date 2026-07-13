// Package retrieval is the engine's grounding query step (ADR 0019): it reads the
// curated vault into indexable documents and, given a query, composes a Store's
// primitives (semantic + keyword) into the ranked chunks the engine grounds on.
// Query embedding and rank fusion live here, above the Store port — a backend
// exposes only primitives and returns score-free chunks (see package store).
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
	"strings"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/store"
)

// VaultDocs walks vaultDir and returns one store.Doc per curated markdown file,
// in scan order, each carrying that file's chunks — the indexing input a Store's
// Index consumes. Each *.md file (dot-dirs like .git/.obsidian skipped) has its
// frontmatter stripped and is split on markdown headings; a chunk's Source is the
// vault-relative slash path, plus "#heading" when it has one, and its Text is
// trimmed. A file with no headings is one chunk; a whitespace-only chunk is
// dropped. A missing or unreadable vault is an error; an empty vault returns no
// docs.
//
// Dimensions (the structured-lookup input) are left unset this increment; they are
// populated with the structured-lookup work (ADR 0019). Flattening every doc's
// Chunks in order reproduces the flat chunk stream the pre-store retrievers built
// on, so semantic and keyword indexing are over the identical units.
func VaultDocs(ctx context.Context, vaultDir string) ([]store.Doc, error) {
	var docs []store.Doc
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

		var chunks []core.Chunk
		for _, c := range chunkMarkdown(stripFrontmatter(string(content))) {
			source := rel
			if c.heading != "" {
				source = rel + "#" + c.heading
			}
			chunks = append(chunks, core.Chunk{Source: source, Text: strings.TrimSpace(c.text)})
		}
		if len(chunks) > 0 {
			docs = append(docs, store.Doc{Ref: store.DocRef{Path: rel}, Chunks: chunks})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("retrieval: scan %s: %w", vaultDir, err)
	}
	return docs, nil
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
