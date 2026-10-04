package core

import (
	"fmt"
	"time"

	"github.com/yaad-index/bonyan/agent"
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

// use makes a's run use the memory for q: the user is the subject and the
// session is the user's turns in q's chat within the current window (ADR 0023
// §2). A query the runtime did not mark, or one missing its user or chat, runs
// without memory.
func (m *Memory) use(a *agent.Agent, q Query) {
	if m == nil || !q.Remember || q.User.ID == "" || q.Chat == "" {
		return
	}
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	a.Memory = m.storeFor(q.Chat)
	a.Subject = q.User.ID
	a.Session = Session(q.Chat, q.User.ID, WindowStart(now(), m.Window))
	a.Scrubber = m.Scrubber
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
