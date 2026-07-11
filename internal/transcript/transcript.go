// Package transcript is the optional, durable, role-tagged record of a group
// conversation (ADR 0015): both human turns and the bot's own answers, kept in a
// store SEPARATE from the quarantine log (ADR 0004) and never read by the
// answering or curation path.
//
// It is an outward-facing audit record — for operator review and answer-quality
// evaluation — not an inward-facing source. Like the quarantine log it has no
// read path the engine or curation could use: the anti-drift isolation is
// structural, not conventional. It matters more here than for the quarantine log
// because the transcript records the bot's OWN answers; if curation could read
// them, the vault could drift toward what the bot has said rather than what the
// community knows.
//
// Layout (ADR 0015): a DirLog is a directory holding one append-only
// <chat-id>.jsonl per group chat. The chat id is sanitized to a filesystem-safe
// filename so a hostile or unusual id can never escape the directory.
package transcript

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Role is a transcript entry's author.
type Role string

const (
	// RoleHuman is a consented community member's turn.
	RoleHuman Role = "human"
	// RoleBot is the engine's own serve-path response — an answer or a refusal.
	RoleBot Role = "bot"
	// RoleSystem is an operational marker that explains a gap (e.g. a rate-limited
	// turn that has no bot reply), so a human-turn-without-a-bot-turn is not misread
	// as a bug in the audit record.
	RoleSystem Role = "system"
)

// EventRateLimited is the system-marker event for a throttled directed message:
// the human turn is recorded but the bot did not answer, so the marker explains
// the gap (ADR 0015). It is the only marker case — refusals carry their own bot
// turn, ambient messages expect no answer, and unconsented users never enter.
const EventRateLimited = "rate_limited"

// Entry is one line of a conversation transcript. Human turns carry the speaker
// and message threading; a bot turn carries the answer text; a system marker
// carries an event and no user. ChatID is redundant with the file it lands in but
// kept on the line so a copied-out entry is self-describing.
type Entry struct {
	Time      time.Time `json:"time"`
	Role      Role      `json:"role"`
	ChatID    string    `json:"chat_id"`
	UserID    string    `json:"user_id,omitempty"`
	Speaker   string    `json:"speaker,omitempty"`
	Text      string    `json:"text,omitempty"`
	MessageID string    `json:"message_id,omitempty"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	Event     string    `json:"event,omitempty"`
}

// Log appends role-tagged turns to a conversation transcript. Append routes the
// entry to its chat's file by Entry.ChatID.
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

// Entries returns a copy of the recorded entries.
func (l *MemoryLog) Entries() []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Entry, len(l.entries))
	copy(out, l.entries)
	return out
}

var _ Log = (*MemoryLog)(nil)

// DirLog is the durable store: a directory with one append-only <chat-id>.jsonl
// per chat. Files are opened lazily on first write to a chat and cached, so a
// long-running instance keeps one handle per active chat rather than reopening
// per line.
type DirLog struct {
	dir  string
	mu   sync.Mutex
	open map[string]*os.File // sanitized filename -> handle
}

// OpenDir prepares the transcript directory, creating it if needed and verifying
// it is writable up front — a misconfigured or unwritable path fails loud at
// startup rather than silently dropping the audit record later (ADR 0015).
func OpenDir(dir string) (*DirLog, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("transcript: create dir %s: %w", dir, err)
	}
	// Probe writability now, so an unwritable directory is a startup error.
	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return nil, fmt.Errorf("transcript: dir %s not writable: %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return &DirLog{dir: dir, open: make(map[string]*os.File)}, nil
}

// Append writes e as one JSON line to its chat's file. The whole line is
// marshaled first and written in a single Write under the mutex, so concurrent
// handler goroutines can never interleave a line.
func (l *DirLog) Append(_ context.Context, e Entry) error {
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("transcript: marshal entry: %w", err)
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := l.fileFor(e.ChatID)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("transcript: append: %w", err)
	}
	return nil
}

// fileFor returns the (cached, lazily-opened) file for a chat id. Caller holds mu.
func (l *DirLog) fileFor(chatID string) (*os.File, error) {
	name := safeChatFilename(chatID)
	if f, ok := l.open[name]; ok {
		return f, nil
	}
	f, err := os.OpenFile(filepath.Join(l.dir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("transcript: open %s: %w", name, err)
	}
	l.open[name] = f
	return f, nil
}

// Close releases every open chat file.
func (l *DirLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var first error
	for name, f := range l.open {
		if err := f.Close(); err != nil && first == nil {
			first = err
		}
		delete(l.open, name)
	}
	return first
}

var _ Log = (*DirLog)(nil)

// safeChatFilename maps a chat id to a filesystem-safe <id>.jsonl name. Chat ids
// are typically numeric and can be negative (e.g. "-5527987187"), but the store
// must not trust that: every rune outside [A-Za-z0-9_-] is replaced with '_', so
// no path separator or "." can survive — a "../.." or absolute-path id collapses
// to underscores and can never escape the directory. An empty result falls back
// to a fixed token so a blank id still gets a valid file.
func safeChatFilename(chatID string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, chatID)
	if safe == "" {
		safe = "chat"
	}
	return safe + ".jsonl"
}
