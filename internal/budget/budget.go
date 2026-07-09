// Package budget is the global spend ceiling (ADR 0006): a single token budget
// per period that bounds total model spend across all users and surfaces. The
// model-call path checks Allow before each call and Records the response's token
// usage after; the per-user rate limit (ADR 0003) defers to this as the outermost
// cost check.
//
// The ceiling is in tokens — the model-agnostic cost driver — so it needs no
// per-model price table or floating-point money. The meter is persisted so a
// restart or crash-loop cannot reset it and blow the budget, and it fails closed:
// a meter cannot be built without a positive ceiling and period.
package budget

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// ErrOverBudget is the typed refusal a model-call path returns when the global
// spend ceiling is reached (ADR 0006): the call is refused, not queued. It is the
// shared error the whole codebase uses so an over-budget refusal is
// distinguishable from any other failure.
var ErrOverBudget = errors.New("budget: global spend ceiling reached")

// Config configures the global spend ceiling (ADR 0006). Both fields are
// required and must be positive — there is no unlimited-by-omission budget.
type Config struct {
	// Ceiling is the maximum tokens spent per Period.
	Ceiling int64
	// Period is the budget window; the meter resets when it elapses.
	Period time.Duration
}

// State is the meter's persisted state — the current period's start and the
// tokens spent within it.
type State struct {
	PeriodStart time.Time `json:"period_start"`
	Spent       int64     `json:"spent"`
}

// Store persists the meter State across restarts. The in-memory store serves
// tests; the bbolt store (bolt.go) serves production.
type Store interface {
	// Load returns the persisted state; found is false on first run.
	Load() (state State, found bool, err error)
	// Save writes the state.
	Save(State) error
}

// Meter is the global spend ceiling: a token budget per period, checked before a
// model call and accumulated after (ADR 0006). It is safe for concurrent use.
type Meter struct {
	ceiling int64
	period  time.Duration
	store   Store

	mu    sync.Mutex
	start time.Time
	spent int64
}

// New builds a Meter. It fails closed: a non-positive ceiling or period, or a nil
// store, is refused — so cost-safety is never off by omission. Any prior
// persisted state is loaded, and its period is rolled on first use if already
// elapsed.
func New(cfg Config, store Store) (*Meter, error) {
	if cfg.Ceiling <= 0 {
		return nil, fmt.Errorf("budget: a positive token ceiling is required (cost-safety is not optional)")
	}
	if cfg.Period <= 0 {
		return nil, fmt.Errorf("budget: a positive period is required")
	}
	if store == nil {
		return nil, errors.New("budget: a store is required")
	}
	m := &Meter{ceiling: cfg.Ceiling, period: cfg.Period, store: store}
	st, found, err := store.Load()
	if err != nil {
		return nil, fmt.Errorf("budget: load state: %w", err)
	}
	if found {
		m.start, m.spent = st.PeriodStart, st.Spent
	} else {
		m.start = time.Now()
	}
	return m, nil
}

// Allow reports whether another model call is within budget: true while the
// current period's spend is below the ceiling. It rolls the period first if it
// has elapsed. A false result is a refusal (fail-safe).
func (m *Meter) Allow() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollLocked(time.Now())
	return m.spent < m.ceiling
}

// Record adds a completed call's actual token usage to the current period and
// persists the meter. It rolls the period first if it has elapsed; a negative
// count is treated as zero. Accounting is post-call because exact usage is only
// known then — so the pre-call Allow blocks the next call after the ceiling is
// crossed, bounding overspend to at most one in-flight call.
func (m *Meter) Record(tokens int64) error {
	if tokens < 0 {
		tokens = 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollLocked(time.Now())
	m.spent += tokens
	return m.store.Save(State{PeriodStart: m.start, Spent: m.spent})
}

// Remaining is the tokens left in the current period (never negative), for
// observability. It rolls the period first if it has elapsed.
func (m *Meter) Remaining() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rollLocked(time.Now())
	if r := m.ceiling - m.spent; r > 0 {
		return r
	}
	return 0
}

// rollLocked resets the period to now if it has elapsed. Callers hold m.mu. The
// reset is best-effort persisted so a restart just after a period boundary does
// not resurrect the spent old period; a Save error here is non-fatal (the next
// Record persists again on the spend path).
func (m *Meter) rollLocked(now time.Time) {
	if now.Sub(m.start) < m.period {
		return
	}
	m.start, m.spent = now, 0
	_ = m.store.Save(State{PeriodStart: m.start, Spent: m.spent})
}

// MemoryStore is an in-memory Store for tests. It is NOT for production: a
// restart loses the budget, which defeats the backstop.
type MemoryStore struct {
	mu    sync.Mutex
	st    State
	found bool
}

// Load returns the in-memory state.
func (s *MemoryStore) Load() (State, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st, s.found, nil
}

// Save stores the state in memory.
func (s *MemoryStore) Save(st State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st, s.found = st, true
	return nil
}
