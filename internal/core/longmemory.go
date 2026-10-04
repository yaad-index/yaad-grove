package core

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/secret"
)

// Memory is the engine's long-term memory (ADR 0023): bonyan's memory stores,
// one per namespace, with the subject and session every run is keyed by.
type Memory struct {
	// Store is the memory in the instance's namespace, which every group chat
	// not in Groups uses.
	Store *memory.Store
	// Groups maps a group chat's ID to the store of the namespace it was
	// given; nil gives none its own.
	Groups map[string]*memory.Store
	// Window is the length of a session's window: a session holds one user's
	// turns in one chat that started within one window, so retention can
	// delete it whole. It must be positive.
	Window time.Duration
	// Scrubber removes resolved secrets from what the run keeps; it should be
	// the one the store scrubs with. Nil removes nothing.
	Scrubber *secret.Scrubber
	// Now is the clock; nil is time.Now.
	Now func() time.Time

	// mu guards withdrawn. keep holds it for reading while it appends, and
	// Withdraw takes it for writing, so a withdrawal waits for appends already
	// under way and every later keep sees it.
	mu sync.RWMutex
	// withdrawn counts each user's withdrawals. A turn keeps only if the count
	// is the one read before the gate admitted it (Query.Withdrawals).
	withdrawn map[string]uint64
}

// Withdrawals is user's withdrawal count: the caller reads it before the
// consent gate decides, and sets it on the Query. No memory counts none.
func (m *Memory) Withdrawals(user string) uint64 {
	if m == nil {
		return 0
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.withdrawn[user]
}

// Withdraw stops every turn of user already under way from keeping anything,
// and returns once no turn of user is still keeping. The caller then erases
// user's memory (ADR 0023 §5), and nothing kept before the erase outlives it.
// A turn that starts afterwards keeps as usual, so a user who consents again is
// remembered again.
func (m *Memory) Withdraw(user string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.withdrawn == nil {
		m.withdrawn = map[string]uint64{}
	}
	m.withdrawn[user]++
}

// WithMemory gives the engine long-term memory (ADR 0023). A nil m, or one
// with no store, is no memory, so the engine answers as before.
func WithMemory(m *Memory) Option {
	return func(e *Engine) {
		if m != nil && m.Store != nil && m.Window > 0 {
			e.memory = m
		}
	}
}

// turn is one query's place in long-term memory: the store of its chat's
// namespace, the asker as subject, and the session.
type turn struct {
	store    *memory.Store
	subject  string
	session  string
	scrubber *secret.Scrubber
	mem      *Memory
	// withdrawn is the subject's withdrawal count read before the gate.
	withdrawn uint64
}

// use makes a's run recall what is known about q's asker, and returns where
// the turn is kept once it is answered. The user is the subject and the
// session is the user's turns in q's chat within the current window (ADR 0023
// §2). The run itself keeps nothing, so a refused turn is never kept. A query
// the runtime did not mark, or one missing its user or chat, runs without
// memory and returns nil.
func (m *Memory) use(a *agent.Agent, q Query) *turn {
	if m == nil || !q.Remember || q.User.ID == "" || q.Chat == "" {
		return nil
	}
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	t := &turn{
		store:     m.storeFor(q.Chat),
		subject:   q.User.ID,
		session:   Session(q.Chat, q.User.ID, WindowStart(now(), m.Window)),
		scrubber:  m.Scrubber,
		mem:       m,
		withdrawn: q.Withdrawals,
	}
	a.Memory = t.store
	a.Subject = t.subject
	return t
}

// keep keeps an answered turn: the asker's message, as the run received it,
// and the answer, as model output (ADR 0023 §3, §5), each scrubbed of resolved
// secrets before it leaves the engine. A turn whose user withdrew since it
// started keeps nothing. A failure is logged; the answer is still the user's.
func (t *turn) keep(ctx context.Context, message, answer string) {
	if t == nil {
		return
	}
	t.mem.mu.RLock()
	defer t.mem.mu.RUnlock()
	if t.mem.withdrawn[t.subject] != t.withdrawn {
		slog.Info("long-term memory: the user withdrew during the answer; the turn is not kept")
		return
	}
	scrub := func(s string) string { return s }
	if t.scrubber != nil {
		scrub = t.scrubber.Scrub
	}
	if err := t.store.Append(ctx, t.subject, t.session, content.Provenance{Kind: content.KindUser}, scrub(message)); err != nil {
		slog.Warn("long-term memory: keeping the turn failed", "err", err)
		return
	}
	if err := t.store.Append(ctx, t.subject, t.session, content.Provenance{Kind: content.KindModel}, scrub(answer)); err != nil {
		slog.Warn("long-term memory: keeping the answer failed", "err", err)
	}
}

// storeFor is the store of chat's namespace.
func (m *Memory) storeFor(chat string) *memory.Store {
	if s, ok := m.Groups[chat]; ok && s != nil {
		return s
	}
	return m.Store
}

// WindowStart is the start of the window of length w that t falls in, counted
// from the Unix epoch, so every instance and every restart agrees on it.
func WindowStart(t time.Time, w time.Duration) time.Time {
	return time.Unix(0, 0).UTC().Add(t.Sub(time.Unix(0, 0)).Truncate(w))
}

// Session names the session of user's turns in chat that started in the
// window beginning at start. The backend encodes the name itself.
func Session(chat, user string, start time.Time) string {
	return fmt.Sprintf("%s/%s/%d", chat, user, start.Unix())
}
