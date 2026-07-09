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

	"github.com/yaad-index/yaad-grove/internal/core"
)

// FullText retrieves by scanning markdown files under a vault root. It
// implements core.Retriever.
type FullText struct {
	// VaultDir is the curated vault root (markdown + YAML frontmatter).
	VaultDir string
	// MaxChunks caps how many chunks a single retrieval returns.
	MaxChunks int
}

// New returns a FullText retriever over vaultDir.
func New(vaultDir string, maxChunks int) *FullText {
	return &FullText{VaultDir: vaultDir, MaxChunks: maxChunks}
}

// Retrieve returns up to MaxChunks vault chunks relevant to query.
//
// Scaffold: no scan yet. Phase 1 will index the markdown files and rank by a
// simple term-frequency match, returning chunks with their source path.
func (f *FullText) Retrieve(ctx context.Context, query string) ([]core.Chunk, error) {
	return nil, core.ErrNotImplemented
}

// compile-time assertion that FullText satisfies core.Retriever.
var _ core.Retriever = (*FullText)(nil)
