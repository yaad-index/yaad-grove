// Package pending is the callback token store: it holds an Action awaiting a
// button click, keyed by a short random token, so only the token rides in a
// Telegram callback_data (capped at 64 bytes) while the full action stays
// server-side (ADR 0009).
//
// Two safety properties fall out of the store, and the security spine leans on
// both: expiry (a stale button dies) and single-use (a click can't be
// replayed). Single-use is a tombstone rather than a delete — a resolved record
// is kept but marked consumed — so a second click is distinguishable from an
// expired one, and the two get different toasts.
//
// The store is transport-neutral (token -> core.Action); the Telegram adapter
// mints tokens when it renders a keyboard, and the runtime resolves them when a
// click arrives.
package pending

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"sync"
	"time"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// Status is the outcome of resolving a token.
type Status int

const (
	// Resolved: a fresh token — the action is returned and the record is now
	// tombstoned (a further Resolve yields Consumed).
	Resolved Status = iota
	// Expired: the token is unknown or past its TTL — a dead button.
	Expired
	// Consumed: the token resolved once already — a replayed click.
	Consumed
)

// record is a pending action with its lifetime. ConsumedAt is zero until the
// first Resolve tombstones it.
type record struct {
	Action     core.Action `json:"action"`
	ExpiresAt  time.Time   `json:"expires_at"`
	ConsumedAt time.Time   `json:"consumed_at,omitempty"`
}

// Store holds pending actions keyed by an opaque token.
type Store interface {
	// Put stores action under a fresh random token with the store's TTL and
	// returns the token.
	Put(ctx context.Context, action core.Action) (string, error)
	// Resolve consumes token: a fresh token returns (action, Resolved) and is
	// tombstoned; a replayed one returns Consumed; an unknown or expired one
	// returns Expired.
	Resolve(ctx context.Context, token string) (core.Action, Status, error)
	// Sweep removes every record past its TTL — unclicked tokens that aged out
	// and consumed tombstones alike — and reports how many it dropped. Resolve's
	// lazy checks keep correctness, but nothing re-touches a token that is never
	// clicked again, so an unswept bucket grows without bound; the runtime calls
	// Sweep periodically.
	Sweep(ctx context.Context) (int, error)
}

// newToken returns a short, URL-safe, unguessable token — 16 bytes of
// randomness (~22 chars base64), well under Telegram's 64-byte callback_data cap.
func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// RunSweeper calls s.Sweep every interval until ctx is cancelled — the store's
// garbage collector. A method that is merely implemented is not a bound: without
// something driving it, expired tokens and old tombstones the lazy path never
// re-touches would accumulate. BoltStore starts this itself on open; a caller
// using another Store starts it directly. A non-positive interval disables it.
func RunSweeper(ctx context.Context, s Store, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.Sweep(ctx); err != nil {
				slog.Warn("pending: sweep failed", "err", err)
			} else if n > 0 {
				slog.Debug("pending: swept expired tokens", "count", n)
			}
		}
	}
}

// resolveRecord applies the expiry/single-use rules to a found record read at
// now, returning the status and, for a fresh resolve, the record to persist
// back (tombstoned). It is the shared decision the memory and bolt stores both
// use; a token that was not found is Expired at the call site.
//
// The consumed check comes before the TTL check on purpose: once a button has
// been used, a later click reads "already completed" even if the TTL has since
// elapsed — the outcome the user cares about is that they already did it, not
// that it aged out.
func resolveRecord(rec record, now time.Time) (core.Action, Status, *record) {
	switch {
	case !rec.ConsumedAt.IsZero():
		return core.Action{}, Consumed, nil
	case now.After(rec.ExpiresAt):
		return core.Action{}, Expired, nil
	default:
		rec.ConsumedAt = now
		return rec.Action, Resolved, &rec
	}
}

// MemoryStore is an in-process Store — used in tests and for a non-persistent
// bot. BoltStore (bolt.go) is the durable variant, matching the memory/bolt
// pairing in internal/budget and internal/acl.
type MemoryStore struct {
	mu  sync.Mutex
	m   map[string]record
	ttl time.Duration
	now func() time.Time // injectable clock; time.Now in production
}

// NewMemoryStore returns an in-memory store whose tokens live for ttl.
func NewMemoryStore(ttl time.Duration) *MemoryStore {
	return &MemoryStore{m: make(map[string]record), ttl: ttl, now: time.Now}
}

// Put stores action under a fresh token expiring ttl from now.
func (s *MemoryStore) Put(_ context.Context, action core.Action) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[token] = record{Action: action, ExpiresAt: s.now().Add(s.ttl)}
	return token, nil
}

// Resolve applies the expiry/single-use rules and tombstones a fresh token.
func (s *MemoryStore) Resolve(_ context.Context, token string) (core.Action, Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.m[token]
	if !ok {
		return core.Action{}, Expired, nil
	}
	action, status, updated := resolveRecord(rec, s.now())
	if updated != nil {
		s.m[token] = *updated
	}
	return action, status, nil
}

// Sweep drops every record past its TTL.
func (s *MemoryStore) Sweep(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	n := 0
	for token, rec := range s.m {
		if now.After(rec.ExpiresAt) {
			delete(s.m, token)
			n++
		}
	}
	return n, nil
}

var _ Store = (*MemoryStore)(nil)
