// Package store is the retrieval backend port (ADR 0019): the engine's grounding
// retrieval depends on the Store interface, and concrete backends are selected by
// config. It holds the port, the value types the port speaks in, and the default
// in-process `memory` backend. Persistent and graph backends (sqlite, ladybug,
// postgres) arrive as adapters in later increments, cgo ones isolated behind build
// tags so the default build stays pure-static.
//
// The split from package retrieval is where-indexed (this package) vs how-queried
// (the retrieval query step that composes these primitives): fusion and query
// embedding live above the port, in retrieval; a Store exposes only primitives and
// returns score-free chunks, so no backend has to leak similarity scores upward.
package store

import (
	"context"
	"errors"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// ErrEnumerateNotImplemented is returned by a backend whose Enumerate is not yet
// built. The primitive is part of the port from the start so the interface is
// stable across backends (adding a method later would touch every one), but its
// implementation lands with the structured-lookup work (ADR 0019); until then a
// call fails loudly rather than silently returning an empty set.
var ErrEnumerateNotImplemented = errors.New("store: Enumerate not implemented")

// Store is the retrieval backend port. The engine's query step holds only this;
// backends are config-selected adapters. All methods are read-mostly after Index.
// Semantic and Keyword return the top-k chunks for their mode, score-free — the
// per-leg similarity floor is applied inside the backend (it is that backend's
// business, applied natively where a server-side backend can), never hoisted into
// the caller.
type Store interface {
	// Index (re)builds the store from the vault's documents: chunk text, their
	// embeddings, and (later) the declared structured dimensions. A persistent
	// backend skips re-embedding unchanged content; the memory backend rebuilds in
	// full. It is the home of the content-hash embedding cache.
	Index(ctx context.Context, docs []Doc) error

	// Semantic returns up to k chunks by vector similarity to the query embedding,
	// already filtered by the backend's similarity floor and ranked. An unembedded
	// backend (no embedder configured) returns no chunks.
	Semantic(ctx context.Context, queryEmbedding []float32, k int) ([]core.Chunk, error)

	// Keyword returns up to k chunks by lexical (full-text) match, ranked.
	Keyword(ctx context.Context, query string, k int) ([]core.Chunk, error)

	// Enumerate returns EVERY document matching a structured predicate over a
	// declared dimension — the complete authoritative set, not top-k. It is the
	// primitive that answers "which documents have X in dimension D" exactly. Not
	// yet implemented (see ErrEnumerateNotImplemented).
	Enumerate(ctx context.Context, dimension, value string) ([]DocRef, error)

	// Dimensions returns, for each declared dimension, its distinct values by
	// DISPLAY form (the first-seen raw spelling behind the normalized match key),
	// sorted. It is the value vocabulary a model needs to choose a valid Enumerate
	// value (ADR 0020) — a discovery affordance only; Enumerate itself is never
	// bounded by it. Empty before the first Index.
	Dimensions(ctx context.Context) (map[string][]string, error)

	// Close releases any resources the backend holds. The memory backend has none.
	Close() error
}

// DocRef identifies a source note in the vault — the unit Enumerate returns and
// that a chunk traces back to. Path is the vault-relative markdown path; Title is
// the note's display title (its frontmatter title, empty if none), so an Enumerate
// result can name each document compactly without its body.
type DocRef struct {
	Path  string
	Title string
}

// Doc is a source note handed to Index: its retrievable chunks, the frontmatter
// dimensions the instance declared queryable (e.g. games, hosts → their values),
// and any alias surface forms this note's entity is also known by.
//
// The note's canonical name is Ref.Title — the string OTHER notes use to reference
// this entity in their dimension lists. KB contract (ADR 0019): Ref.Title must
// normalize to exactly that referenced string, or the alias won't register against
// it (the engine can't reconcile a title of "Acme Rail (game)" with a
// games: [Acme Rail] entry — the KB author owns that consistency). Aliases are
// alternate surface forms
// (transliterations, cross-script spellings) that also resolve to this entity; they
// are additive — a note with none still resolves under its canonical name. Semantic
// and keyword indexing ignore Dimensions/Aliases.
type Doc struct {
	Ref        DocRef
	Chunks     []core.Chunk
	Dimensions map[string][]string
	Aliases    []string
}
