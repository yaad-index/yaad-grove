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
	"strconv"
	"strings"
	"time"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// ErrEnumerateNotImplemented is returned by a backend whose Enumerate is not yet
// built. The primitive is part of the port from the start so the interface is
// stable across backends (adding a method later would touch every one), but its
// implementation lands with the structured-lookup work (ADR 0019); until then a
// call fails loudly rather than silently returning an empty set.
var ErrEnumerateNotImplemented = errors.New("store: Enumerate not implemented")

// ErrOrderedNotImplemented is the Ordered counterpart of
// ErrEnumerateNotImplemented: a backend that has not built ordered recall fails
// loudly rather than returning an empty set, which a caller could not tell apart
// from "no document carries that field".
var ErrOrderedNotImplemented = errors.New("store: Ordered not implemented")

// Direction is the sort order for ordered recall. A bare "the latest" is
// Descending with limit 1 (ADR 0022, decided by the maintainer).
type Direction string

const (
	// Ascending sorts smallest/oldest first — "the first", "the oldest".
	Ascending Direction = "asc"
	// Descending sorts largest/newest first — "the latest", "the newest".
	Descending Direction = "desc"
)

// OrderKind is how a declared orderable field's values were read. It exists to
// keep one field's values comparable with each other: a date read as a Unix
// second and a sequence number read as itself are both float64 and would sort
// together into nonsense, so a field's kind is fixed by its first usable value
// and later values of a different kind are refused rather than mixed.
type OrderKind int

const (
	// OrderNone means the value could not be read as either kind.
	OrderNone OrderKind = iota
	// OrderNumber is a plain number — a sequence or episode number.
	OrderNumber
	// OrderDate is a date or timestamp, ordered by its Unix second.
	OrderDate
)

// orderedDateLayouts are the accepted written date forms, tried in order. A
// frontmatter date usually arrives already typed (the YAML parser resolves an
// unquoted 2026-01-05 to a time.Time), so these cover the QUOTED spellings, which
// stay strings and would otherwise be silently unorderable.
var orderedDateLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05",
	"2006-01-02",
	"2006/01/02",
}

// ParseOrderedValue reads one frontmatter value as an orderable key, returning the
// sort key, which kind it was read as, and whether it is usable at all.
//
// It takes the value as decoded rather than as a string on purpose: the YAML
// parser resolves an unquoted date to a time.Time and a bare 47 to an int, so
// stringifying first would push a date through "2026-01-05 00:00:00 +0000 UTC" and
// make the caller parse back out of Go's own formatting.
//
// A bool is deliberately NOT orderable: two values is a facet, not an ordinal, and
// ordering by it would answer "the latest" with an arbitrary member of the true
// half.
func ParseOrderedValue(v any) (float64, OrderKind, bool) {
	switch t := v.(type) {
	case nil:
		return 0, OrderNone, false
	case time.Time:
		return float64(t.Unix()), OrderDate, true
	case int:
		return float64(t), OrderNumber, true
	case int64:
		return float64(t), OrderNumber, true
	case float64:
		return t, OrderNumber, true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, OrderNone, false
		}
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return n, OrderNumber, true
		}
		for _, layout := range orderedDateLayouts {
			if ts, err := time.Parse(layout, s); err == nil {
				return float64(ts.Unix()), OrderDate, true
			}
		}
		return 0, OrderNone, false
	default:
		return 0, OrderNone, false
	}
}

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

	// Ordered returns the documents carrying a declared orderable field, sorted by
	// that field (ADR 0022) — the ordering primitive Enumerate lacks, so "the
	// latest / the newest / the Nth" is answered over the WHOLE indexed set rather
	// than over whatever the similarity sample happened to include.
	//
	// Only documents with a usable value for field appear. A document that does not
	// carry it, or carries something that could not be read as a number or a date,
	// is ABSENT — never sorted as zero. Treating a missing date as the epoch makes
	// "the oldest one" return the undated documents, which reads as an answer and
	// is not one.
	//
	// limit <= 0 means uncapped.
	//
	// ⚠️ A caller that intersects this result with anything else MUST pass limit 0
	// and cap afterwards. Capping here and filtering after returns nothing whenever
	// the top-ranked documents fall outside the filter, while matching documents
	// exist — which is the partial-view defect this ADR exists to remove, rebuilt
	// one layer down. See the enumerate tool, which caps last for exactly this
	// reason.
	Ordered(ctx context.Context, field string, dir Direction, limit int) ([]DocRef, error)

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

	// Ordered holds this note's usable orderable-field values (ADR 0022), keyed by
	// declared field name, already read into a comparable sort key. A field the note
	// does not carry is simply absent — the store never invents a value for it.
	Ordered map[string]float64

	// OrderedSkipped names the declared orderable fields this note DOES carry but
	// whose value could not be used — unreadable as a number or date, or of a
	// different kind than the field's other values. It exists so the count is
	// reportable at startup: without it, a vault whose dates are all misspelled
	// indexes zero orderable values and looks exactly like a vault that is ordered
	// correctly. Tolerating the failure silently would delete the only evidence.
	OrderedSkipped []string
}
