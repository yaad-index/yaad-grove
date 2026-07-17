//go:build ladybug

// Package store's ladybug backend (ADR 0019): a persistent, graph-native Store
// over the embedded LadybugDB (ex-Kuzu). The KB is modelled as a graph — Doc and
// Chunk and dimension-Value nodes, with HAS_CHUNK / HAS_VALUE edges — so Enumerate
// is a one-hop traversal, Semantic is an HNSW vector-index KNN, and Keyword is a
// BM25 full-text search, all native to the engine. The index persists on disk, so
// a restart embeds only new/changed chunks (keyed by content hash) rather than the
// whole vault (#86). cgo, isolated behind the `ladybug` build tag — the default
// build stays pure-Go.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	ladybug "github.com/LadybugDB/go-ladybug"

	"github.com/yaad-index/yaad-grove/internal/embed"
)

//go:generate sh -c "curl -fsSL https://raw.githubusercontent.com/LadybugDB/ladybug/refs/heads/main/scripts/download-liblbug.sh | LBUG_TARGET_DIR=$(git rev-parse --show-toplevel)/lib-ladybug bash"

// Ladybug is the persistent graph backend. It holds an open embedded database; the
// embedder + similarity floor mirror the memory backend so query behavior matches.
//
// LadybugDB connections are NOT safe for concurrent use, so every Store method that
// touches the connection holds mu — this serializes a live reindex (Index, driven
// by SIGHUP) against the Telegram query goroutines (Semantic/Keyword/Enumerate).
// It is the mutex counterpart to the memory backend's atomic-pointer swap. Internal
// helpers assume the caller already holds mu and never re-lock.
type Ladybug struct {
	mu        sync.Mutex
	db        *ladybug.Database
	conn      *ladybug.Connection
	embedder  embed.Embedder
	threshold float32
	dim       int // embedding dimension, fixed at first index
}

// NewLadybug opens (or creates) the database at path and loads the vector + FTS
// extensions (baked into the image at build time; ADR 0019). embedder may be nil
// for a keyword-only instance. threshold is the semantic similarity floor.
// queryTimeoutMS bounds any single Cypher query (milliseconds) so a stall fails
// loud instead of hanging forever (#132). Well above a normal index/query.
const queryTimeoutMS = 300000

func NewLadybug(path string, embedder embed.Embedder, threshold float32) (Store, error) {
	db, err := ladybug.OpenDatabase(path, ladybug.DefaultSystemConfig())
	if err != nil {
		return nil, fmt.Errorf("store: open ladybug at %s: %w", path, err)
	}
	conn, err := ladybug.OpenConnection(db)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ladybug connection: %w", err)
	}
	// A per-query timeout so a pathological query fails loud rather than hanging the
	// process forever (#132). Generous — a normal index/query is well under it, so it
	// only ever fires on a genuine stall.
	conn.SetTimeout(queryTimeoutMS)
	l := &Ladybug{db: db, conn: conn, embedder: embedder, threshold: threshold}
	if err := l.loadExtensions(); err != nil {
		l.Close()
		return nil, err
	}
	if err := l.ensureBaseSchema(); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// loadExtensions makes the vector + FTS extensions available. It tries LOAD first
// (they are baked into the image at build time for offline runtime); if that fails
// it falls back to INSTALL — which needs network, so it only succeeds in dev / CI /
// image-build, not a locked runtime. A locked runtime without baked extensions gets
// a clear error telling the operator to bake them (ADR 0019).
func (l *Ladybug) loadExtensions() error {
	for _, ext := range []string{"vector", "fts"} {
		if err := l.exec("LOAD EXTENSION " + ext + ";"); err != nil {
			if ierr := l.exec("INSTALL " + ext + ";"); ierr != nil {
				return fmt.Errorf("store: ladybug %s extension not baked in and INSTALL failed (bake it at image-build time): %w", ext, ierr)
			}
			if lerr := l.exec("LOAD EXTENSION " + ext + ";"); lerr != nil {
				return fmt.Errorf("store: ladybug load %s after install: %w", ext, lerr)
			}
		}
	}
	return nil
}

// exec runs a Cypher statement discarding its result.
func (l *Ladybug) exec(q string) error {
	r, err := l.conn.Query(q)
	if err != nil {
		return err
	}
	r.Close()
	return nil
}

// ensureBaseSchema creates the node/rel tables that don't depend on the embedding
// dimension. The Chunk table (which carries the fixed-size embedding array) is
// created lazily on the first index, once the dimension is known.
func (l *Ladybug) ensureBaseSchema() error {
	stmts := []string{
		"CREATE NODE TABLE IF NOT EXISTS Doc(path STRING, title STRING, PRIMARY KEY(path));",
		"CREATE NODE TABLE IF NOT EXISTS Value(id STRING, dim STRING, nk STRING, disp STRING, PRIMARY KEY(id));",
		"CREATE NODE TABLE IF NOT EXISTS Alias(nk STRING, canon STRING, PRIMARY KEY(nk));",
		"CREATE REL TABLE IF NOT EXISTS HAS_VALUE(FROM Doc TO Value);",
	}
	for _, s := range stmts {
		if err := l.exec(s); err != nil {
			return fmt.Errorf("store: ladybug schema: %w", err)
		}
	}
	return nil
}

// chunkHash is the content key for a chunk — sha256 of source+text, so a chunk is
// re-embedded only when its text (or its location) actually changes (#86).
func chunkHash(source, text string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + text))
	return hex.EncodeToString(sum[:])
}

// Index rebuilds the graph from docs, embedding only chunks whose content hash is
// not already stored (the #86 delta). It then rebuilds the structured (Value/Alias)
// graph and the vector + FTS indexes over the current chunk set.
func (l *Ladybug) Index(ctx context.Context, docs []Doc) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Collect the current chunk set and which hashes are new.
	var all []pending
	haveHash := map[string]bool{}
	for _, d := range docs {
		for _, c := range d.Chunks {
			h := chunkHash(c.Source, c.Text)
			all = append(all, pending{c.Source, c.Text, h})
			haveHash[h] = true
		}
	}

	stored, err := l.storedHashes()
	if err != nil {
		return err
	}

	// Embed only the chunks whose hash isn't already stored.
	var toEmbed []pending
	var texts []string
	for _, p := range all {
		if !stored[p.hash] {
			toEmbed = append(toEmbed, p)
			texts = append(texts, p.text)
		}
	}
	vectors, err := l.embedTexts(ctx, texts)
	if err != nil {
		return err
	}

	// The Chunk table is created on the first index (when the embedding dimension is
	// known); on restart it already exists and is reused. DDL stays outside the
	// transaction below.
	if err := l.ensureChunkTable(); err != nil {
		return err
	}

	// Upsert new chunks, drop chunks no longer present, and rebuild the structured
	// graph in ONE explicit transaction. In auto-commit mode LadybugDB runs each of
	// the thousands of small writes as its own transaction + checkpoint, which is
	// pathologically slow and hangs at real vault scale (#132); a single transaction
	// collapses them into one commit. The index DDL (rebuildIndexes) stays outside —
	// it is not transactional.
	if err := l.exec("BEGIN TRANSACTION;"); err != nil {
		return err
	}
	if err := l.writeGraph(toEmbed, vectors, docs, haveHash); err != nil {
		_, _ = l.conn.Query("ROLLBACK;")
		return err
	}
	if err := l.exec("COMMIT;"); err != nil {
		return err
	}
	return l.rebuildIndexes()
}

// pending is a chunk staged for indexing: its source, text, and content hash.
type pending struct{ source, text, hash string }

// writeGraph performs the transactional DML of an index: upsert the new chunks,
// prune the removed ones, and rebuild the Doc/Value/Alias subgraph. The caller
// wraps it in a transaction.
func (l *Ladybug) writeGraph(toEmbed []pending, vectors [][]float32, docs []Doc, keep map[string]bool) error {
	for i, p := range toEmbed {
		if err := l.upsertChunk(p.source, p.text, p.hash, vectors[i]); err != nil {
			return err
		}
	}
	if err := l.pruneChunks(keep); err != nil {
		return err
	}
	return l.rebuildStructured(docs)
}

// storedHashes returns the set of chunk hashes already in the database.
func (l *Ladybug) storedHashes() (map[string]bool, error) {
	out := map[string]bool{}
	if l.dim == 0 && !l.chunkTableExists() {
		return out, nil // no chunk table yet → nothing stored
	}
	r, err := l.conn.Query("MATCH (c:Chunk) RETURN c.hash;")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	for r.HasNext() {
		t, err := r.Next()
		if err != nil {
			return nil, err
		}
		v, err := t.GetValue(0)
		if err != nil {
			return nil, err
		}
		if s, ok := v.(string); ok {
			out[s] = true
		}
	}
	return out, nil
}

// embedTexts embeds the given chunk texts and records the embedding dimension.
func (l *Ladybug) embedTexts(ctx context.Context, texts []string) ([][]float32, error) {
	if l.embedder == nil || len(texts) == 0 {
		return nil, nil
	}
	vecs, err := l.embedder.Embed(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("store: ladybug embed delta: %w", err)
	}
	if len(vecs) != len(texts) {
		return nil, fmt.Errorf("store: ladybug embedded %d of %d chunks", len(vecs), len(texts))
	}
	if len(vecs) > 0 {
		l.dim = len(vecs[0])
	}
	return vecs, nil
}

// Close releases the connection and database.
func (l *Ladybug) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		l.conn.Close()
	}
	if l.db != nil {
		l.db.Close()
	}
	return nil
}

// cypherString escapes a Go string for embedding in a single-quoted Cypher literal.
func cypherString(s string) string {
	return "'" + strings.NewReplacer("\\", "\\\\", "'", "\\'").Replace(s) + "'"
}

// compile-time assertion that Ladybug satisfies the Store port.
var _ Store = (*Ladybug)(nil)
