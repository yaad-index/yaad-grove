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

	"gopkg.in/yaml.v3"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/store"
)

// VaultDocs walks vaultDir and returns one store.Doc per curated markdown file,
// in scan order, each carrying that file's chunks plus its structured metadata —
// the indexing input a Store's Index consumes. Each *.md file (dot-dirs like
// .git/.obsidian skipped) has its frontmatter split off and is chunked on markdown
// headings; a chunk's Source is the vault-relative slash path, plus "#heading" when
// it has one, and its Text is trimmed. A file with no headings is one chunk; a
// whitespace-only chunk is dropped. A missing or unreadable vault is an error; an
// empty vault returns no docs.
//
// The frontmatter is parsed (not just stripped): the note's `title` becomes its
// canonical name, each field named in dimensions becomes a queryable dimension
// (ADR 0019), and `aliases` + any `name_<lang>` field become alias surface forms.
// dimensions empty means no structured lookup — only chunks are populated.
// Flattening every doc's Chunks in order reproduces the flat chunk stream the
// pre-store retrievers built on, so semantic and keyword indexing are over the
// identical units.
func VaultDocs(ctx context.Context, vaultDir string, dimensions []string) ([]store.Doc, error) {
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

		front, body, ferr := parseFrontmatter(string(content))
		if ferr != nil {
			return fmt.Errorf("frontmatter in %s: %w", rel, ferr)
		}

		var chunks []core.Chunk
		for _, c := range chunkMarkdown(body) {
			source := rel
			if c.heading != "" {
				source = rel + "#" + c.heading
			}
			chunks = append(chunks, core.Chunk{Source: source, Text: strings.TrimSpace(c.text)})
		}
		if len(chunks) == 0 {
			return nil
		}
		docs = append(docs, store.Doc{
			Ref:        store.DocRef{Path: rel, Title: frontString(front, "title")},
			Chunks:     chunks,
			Dimensions: frontDimensions(front, dimensions),
			Aliases:    frontAliases(front),
		})
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

// parseFrontmatter splits a leading YAML frontmatter block ("---" line, content,
// "---" line) from the markdown body and parses it, so metadata does not pollute
// ranking AND is available for structured lookup (ADR 0019). No frontmatter, or an
// unterminated block, yields a nil map and the whole content as body (never an
// error). A present-but-malformed block is an error so a KB typo fails loudly at
// startup rather than silently dropping a note's dimensions.
func parseFrontmatter(content string) (map[string]any, string, error) {
	nl := strings.IndexByte(content, '\n')
	if nl < 0 || strings.TrimRight(content[:nl], "\r") != "---" {
		return nil, content, nil
	}
	lines := strings.Split(content[nl+1:], "\n")
	for i, ln := range lines {
		if strings.TrimRight(ln, "\r") == "---" {
			block := strings.Join(lines[:i], "\n")
			body := strings.Join(lines[i+1:], "\n")
			var front map[string]any
			if err := yaml.Unmarshal([]byte(block), &front); err != nil {
				return nil, body, err
			}
			return front, body, nil
		}
	}
	return nil, content, nil // unterminated → treat all as body
}

// frontString returns the string value of a frontmatter field, or "" if absent or
// not a scalar.
func frontString(front map[string]any, key string) string {
	v, ok := front[key]
	if !ok {
		return ""
	}
	return scalarString(v)
}

// frontDimensions extracts the declared dimension fields into value slices; a
// field absent from a note is simply omitted from that note's map.
func frontDimensions(front map[string]any, dimensions []string) map[string][]string {
	if len(dimensions) == 0 || front == nil {
		return nil
	}
	out := map[string][]string{}
	for _, dim := range dimensions {
		if vals := toStrings(front[dim]); len(vals) > 0 {
			out[dim] = vals
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// frontAliases collects a note's alias surface forms: the `aliases` list plus any
// `name_<lang>` field (e.g. name_fa), so an instance declares cross-script names
// in frontmatter (ADR 0019).
func frontAliases(front map[string]any) []string {
	if front == nil {
		return nil
	}
	aliases := toStrings(front["aliases"])
	for key, v := range front {
		if strings.HasPrefix(key, "name_") {
			if s := scalarString(v); s != "" {
				aliases = append(aliases, s)
			}
		}
	}
	if len(aliases) == 0 {
		return nil
	}
	return aliases
}

// toStrings coerces a frontmatter value into a string slice: a scalar becomes a
// one-element slice, a list becomes its scalar elements, anything empty becomes
// nil. Non-string scalars are stringified so a numeric or boolean value still
// indexes.
func toStrings(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s := scalarString(e); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		if s := scalarString(v); s != "" {
			return []string{s}
		}
		return nil
	}
}

// scalarString stringifies a YAML scalar, trimming surrounding whitespace; a
// non-scalar (map/list) or nil yields "".
func scalarString(v any) string {
	switch t := v.(type) {
	case nil, []any, map[string]any:
		return ""
	case string:
		return strings.TrimSpace(t)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", t))
	}
}
