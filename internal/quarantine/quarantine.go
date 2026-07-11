// Package quarantine is the consent-gated log of community messages, kept
// OUTSIDE the answering vault (ADR 0004). It is the data-side mirror of the
// answering-side grounding guarantee: the answering bot never reads this log — it
// grounds only on the curated vault — so logged chatter can never leak into an
// answer. Only consented messages (ADR 0002) are appended; a later, admin-in-the-
// loop curation pass reads the backlog to propose vault edits.
//
// The package is deliberately core-free (plain-typed Entry) and has no read path
// the engine could use: the isolation is structural, not just conventional.
package quarantine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Entry is one logged community message — the minimal record a later curation
// pass needs, and nothing the answering side ever reads.
type Entry struct {
	Time    time.Time `json:"time"`
	UserID  string    `json:"user_id"`
	Surface string    `json:"surface"`
	Text    string    `json:"text"`
}

// Log appends consented community messages to the quarantined store.
type Log interface {
	Append(ctx context.Context, e Entry) error
}

// MemoryLog is an in-process Log for tests and non-persistent instances.
type MemoryLog struct {
	mu      sync.Mutex
	entries []Entry
}

// Append records e.
func (l *MemoryLog) Append(_ context.Context, e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, e)
	return nil
}

// Entries returns a copy of the logged entries.
func (l *MemoryLog) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}

var _ Log = (*MemoryLog)(nil)

// FileLog appends entries as JSON lines to a file (append-only) — the durable
// quarantined store. One JSON object per line: greppable and streamable for a
// batch curation pass.
type FileLog struct {
	mu sync.Mutex
	f  *os.File
}

// OpenFile opens (creating if needed) the quarantine log at path for appending.
func OpenFile(path string) (*FileLog, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("quarantine: open %s: %w", path, err)
	}
	return &FileLog{f: f}, nil
}

// Append writes e as one JSON line. The whole line (JSON + newline) is marshaled
// first and written in a single Write under the mutex, so concurrent handler
// goroutines can never interleave a line regardless of its size.
func (l *FileLog) Append(_ context.Context, e Entry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("quarantine: marshal entry: %w", err)
	}
	line = append(line, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.f.Write(line); err != nil {
		return fmt.Errorf("quarantine: append: %w", err)
	}
	return nil
}

// Close releases the underlying file.
func (l *FileLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}

var _ Log = (*FileLog)(nil)
