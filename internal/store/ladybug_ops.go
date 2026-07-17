//go:build ladybug

package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// The graph is two decoupled subgraphs: Chunk nodes carry the vector + FTS indexes
// (Semantic/Keyword query them directly, no edges); Doc/Value/Alias nodes with
// HAS_VALUE edges answer Enumerate as a one-hop traversal. Only Chunk embeddings
// are content-hash cached (the expensive part, #86); the structured subgraph is
// cheap to rebuild in full each index.

// chunkTableExists reports whether the Chunk node table has been created.
func (l *Ladybug) chunkTableExists() bool {
	r, err := l.conn.Query("CALL SHOW_TABLES() RETURN name;")
	if err != nil {
		return false
	}
	defer r.Close()
	for r.HasNext() {
		t, err := r.Next()
		if err != nil {
			return false
		}
		if v, err := t.GetValue(0); err == nil {
			if s, ok := v.(string); ok && s == "Chunk" {
				return true
			}
		}
	}
	return false
}

// ensureChunkTable creates the Chunk table (carrying the fixed-size embedding
// array) on the first index, once the embedding dimension is known. On restart the
// table already exists and this is a no-op.
func (l *Ladybug) ensureChunkTable() error {
	if l.chunkTableExists() {
		return nil
	}
	if l.dim == 0 {
		// Keyword-only (no embedder) or nothing to embed and no prior table: create a
		// zero-embedding table so Keyword still works.
		return l.exec("CREATE NODE TABLE IF NOT EXISTS Chunk(hash STRING, source STRING, text STRING, PRIMARY KEY(hash));")
	}
	return l.exec(fmt.Sprintf(
		"CREATE NODE TABLE IF NOT EXISTS Chunk(hash STRING, source STRING, text STRING, embedding FLOAT[%d], PRIMARY KEY(hash));",
		l.dim))
}

// upsertChunk stores one chunk (a new content hash) with its embedding.
func (l *Ladybug) upsertChunk(source, text, hash string, vec []float32) error {
	q := fmt.Sprintf("MERGE (c:Chunk {hash: %s}) SET c.source = %s, c.text = %s",
		cypherString(hash), cypherString(source), cypherString(text))
	if len(vec) > 0 {
		q += fmt.Sprintf(", c.embedding = %s", floatArrayLiteral(vec))
	}
	return l.exec(q + ";")
}

// pruneChunks deletes stored chunks whose hash is no longer present in the vault.
func (l *Ladybug) pruneChunks(keep map[string]bool) error {
	hashes := make([]string, 0, len(keep))
	for h := range keep {
		hashes = append(hashes, cypherString(h))
	}
	if len(hashes) == 0 {
		return l.exec("MATCH (c:Chunk) DELETE c;")
	}
	return l.exec("MATCH (c:Chunk) WHERE NOT c.hash IN [" + strings.Join(hashes, ",") + "] DELETE c;")
}

// rebuildStructured drops and rebuilds the Doc/Value/Alias subgraph from docs. It
// is cheap (no embedding) so a full rebuild is simple and correct. The rows are
// written with UNWIND over an inline list — a handful of queries total, not one per
// doc/value: a per-row loop is thousands of Cypher round-trips that make a real
// vault's index take tens of seconds / hang (#132).
func (l *Ladybug) rebuildStructured(docs []Doc) error {
	for _, del := range []string{"MATCH (d:Doc) DETACH DELETE d;", "MATCH (v:Value) DETACH DELETE v;", "MATCH (a:Alias) DELETE a;"} {
		if err := l.exec(del); err != nil {
			return err
		}
	}

	// Collect distinct rows across all docs.
	var docRows, aliasRows, valueRows, edgeRows []string
	valueSeen := map[string]bool{}
	for _, d := range docs {
		docRows = append(docRows, mapLiteral("path", d.Ref.Path, "title", d.Ref.Title))
		if canon := normalizeKey(d.Ref.Title); canon != "" {
			for _, a := range d.Aliases {
				if ak := normalizeKey(a); ak != "" {
					aliasRows = append(aliasRows, mapLiteral("nk", ak, "canon", canon))
				}
			}
		}
		for dim, values := range d.Dimensions {
			seen := map[string]bool{}
			for _, v := range values {
				nk := normalizeKey(v)
				if nk == "" || seen[nk] {
					continue
				}
				seen[nk] = true
				id := dim + "|" + nk
				if !valueSeen[id] {
					valueSeen[id] = true
					valueRows = append(valueRows, mapLiteral("id", id, "dim", dim, "nk", nk, "disp", strings.TrimSpace(v)))
				}
				edgeRows = append(edgeRows, mapLiteral("path", d.Ref.Path, "id", id))
			}
		}
	}

	// One UNWIND query per category. Docs and Values before edges (the edge MATCH
	// depends on both nodes existing).
	batches := []struct {
		rows  []string
		query string
	}{
		{docRows, "UNWIND %s AS r MERGE (d:Doc {path: r.path}) SET d.title = r.title;"},
		{aliasRows, "UNWIND %s AS r MERGE (a:Alias {nk: r.nk}) SET a.canon = r.canon;"},
		{valueRows, "UNWIND %s AS r MERGE (v:Value {id: r.id}) SET v.dim = r.dim, v.nk = r.nk, v.disp = r.disp;"},
		{edgeRows, "UNWIND %s AS r MATCH (d:Doc {path: r.path}), (v:Value {id: r.id}) MERGE (d)-[:HAS_VALUE]->(v);"},
	}
	for _, b := range batches {
		if len(b.rows) == 0 {
			continue
		}
		if err := l.exec(fmt.Sprintf(b.query, "["+strings.Join(b.rows, ",")+"]")); err != nil {
			return err
		}
	}
	return nil
}

// mapLiteral builds a Cypher map literal {k: 'v', ...} from key/value string pairs,
// escaping each value.
func mapLiteral(kv ...string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(kv[i])
		b.WriteByte(':')
		b.WriteString(cypherString(kv[i+1]))
	}
	b.WriteByte('}')
	return b.String()
}

// rebuildIndexes drops and recreates the vector (HNSW, cosine) and FTS (BM25)
// indexes over the current chunk set. Rebuilding is cheap — it re-uses the stored
// embeddings, no embedding-API calls — and Kuzu-family vector indexes don't update
// incrementally, so a rebuild is the correct way to reflect the delta.
func (l *Ladybug) rebuildIndexes() error {
	// No chunks or no embeddings → nothing to index (Keyword can still run on text).
	_, _ = l.conn.Query("CALL DROP_FTS_INDEX('Chunk', 'chunk_fts');")
	_ = l.exec("CALL CREATE_FTS_INDEX('Chunk', 'chunk_fts', ['text']);")
	if l.dim > 0 {
		_, _ = l.conn.Query("CALL DROP_VECTOR_INDEX('Chunk', 'chunk_vec');")
		if err := l.exec("CALL CREATE_VECTOR_INDEX('Chunk', 'chunk_vec', 'embedding', metric := 'cosine');"); err != nil {
			return fmt.Errorf("store: ladybug vector index: %w", err)
		}
	}
	return nil
}

// Semantic returns up to k chunks by HNSW cosine KNN, filtered by the similarity
// floor. The vector index returns cosine *distance* (1 - similarity), so the floor
// is applied as similarity = 1 - distance.
func (l *Ladybug) Semantic(_ context.Context, queryEmbedding []float32, k int) ([]core.Chunk, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(queryEmbedding) == 0 || l.dim == 0 {
		return nil, nil
	}
	q := fmt.Sprintf(
		"CALL QUERY_VECTOR_INDEX('Chunk', 'chunk_vec', %s, %d) RETURN node.source, node.text, distance ORDER BY distance;",
		floatArrayLiteral(queryEmbedding), k)
	r, err := l.conn.Query(q)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []core.Chunk
	for r.HasNext() {
		row, err := r.Next()
		if err != nil {
			return nil, err
		}
		vals, err := row.GetAsSlice()
		if err != nil || len(vals) < 3 {
			continue
		}
		sim := 1 - toFloat(vals[2])
		if l.threshold > 0 && sim < l.threshold {
			continue
		}
		out = append(out, core.Chunk{Source: asString(vals[0]), Text: asString(vals[1])})
	}
	return out, nil
}

// Keyword returns up to k chunks by BM25 full-text score.
func (l *Ladybug) Keyword(_ context.Context, query string, k int) ([]core.Chunk, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	q := fmt.Sprintf(
		"CALL QUERY_FTS_INDEX('Chunk', 'chunk_fts', %s) RETURN node.source, node.text, score ORDER BY score DESC LIMIT %d;",
		cypherString(query), k)
	r, err := l.conn.Query(q)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []core.Chunk
	for r.HasNext() {
		row, err := r.Next()
		if err != nil {
			return nil, err
		}
		vals, err := row.GetAsSlice()
		if err != nil || len(vals) < 2 {
			continue
		}
		out = append(out, core.Chunk{Source: asString(vals[0]), Text: asString(vals[1])})
	}
	return out, nil
}

// Enumerate returns every Doc whose dimension carries value — a one-hop
// Doc-[:HAS_VALUE]->Value traversal, after normalizing value and resolving it
// through the Alias subgraph.
func (l *Ladybug) Enumerate(_ context.Context, dimension, value string) ([]DocRef, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	nk := normalizeKey(value)
	if nk == "" {
		return nil, nil
	}
	if canon, err := l.resolveAlias(nk); err == nil && canon != "" { // resolveAlias runs under mu
		nk = canon
	}
	q := fmt.Sprintf(
		"MATCH (d:Doc)-[:HAS_VALUE]->(v:Value {dim: %s, nk: %s}) RETURN d.path, d.title;",
		cypherString(dimension), cypherString(nk))
	r, err := l.conn.Query(q)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []DocRef
	for r.HasNext() {
		row, err := r.Next()
		if err != nil {
			return nil, err
		}
		vals, err := row.GetAsSlice()
		if err != nil || len(vals) < 2 {
			continue
		}
		out = append(out, DocRef{Path: asString(vals[0]), Title: asString(vals[1])})
	}
	return out, nil
}

// Dimensions returns each dimension's distinct values by display form, sorted
// (ADR 0020) — the value vocabulary for kb_dimensions. It reads v.disp (the
// first-seen raw spelling behind each value node), the graph counterpart of the
// memory backend's display index.
func (l *Ladybug) Dimensions(_ context.Context) (map[string][]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r, err := l.conn.Query("MATCH (v:Value) RETURN v.dim, v.disp;")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	seen := map[string]map[string]bool{}
	out := map[string][]string{}
	for r.HasNext() {
		row, err := r.Next()
		if err != nil {
			return nil, err
		}
		vals, err := row.GetAsSlice()
		if err != nil || len(vals) < 2 {
			continue
		}
		dim, disp := asString(vals[0]), asString(vals[1])
		if dim == "" || disp == "" {
			continue
		}
		if seen[dim] == nil {
			seen[dim] = map[string]bool{}
		}
		if seen[dim][disp] {
			continue
		}
		seen[dim][disp] = true
		out[dim] = append(out[dim], disp)
	}
	for dim := range out {
		sort.Strings(out[dim])
	}
	return out, nil
}

// resolveAlias returns the canonical normalized key for a surface form, or "" if
// the form is not an alias (it resolves to itself).
func (l *Ladybug) resolveAlias(nk string) (string, error) {
	r, err := l.conn.Query(fmt.Sprintf("MATCH (a:Alias {nk: %s}) RETURN a.canon;", cypherString(nk)))
	if err != nil {
		return "", err
	}
	defer r.Close()
	if r.HasNext() {
		t, err := r.Next()
		if err != nil {
			return "", err
		}
		if v, err := t.GetValue(0); err == nil {
			return asString(v), nil
		}
	}
	return "", nil
}

// floatArrayLiteral renders an embedding as a CAST'd fixed-size FLOAT array literal.
func floatArrayLiteral(vec []float32) string {
	parts := make([]string, len(vec))
	for i, f := range vec {
		parts[i] = strconv.FormatFloat(float64(f), 'g', -1, 32)
	}
	return fmt.Sprintf("CAST([%s] AS FLOAT[%d])", strings.Join(parts, ","), len(vec))
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func toFloat(v any) float32 {
	switch f := v.(type) {
	case float64:
		return float32(f)
	case float32:
		return f
	}
	return 0
}
