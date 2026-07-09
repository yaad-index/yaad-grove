package budget_test

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/budget"
)

func newMeter(t *testing.T, ceiling int64, period time.Duration) *budget.Meter {
	t.Helper()
	m, err := budget.New(budget.Config{Ceiling: ceiling, Period: period}, &budget.MemoryStore{})
	require.NoError(t, err)
	return m
}

// Under the ceiling, Allow passes; at/over it, Allow refuses — deterministically.
func TestUnderLimitPassesAtLimitRefuses(t *testing.T) {
	m := newMeter(t, 100, time.Hour)
	assert.True(t, m.Allow(), "fresh meter allows")
	assert.Equal(t, int64(100), m.Remaining())

	require.NoError(t, m.Record(60))
	assert.True(t, m.Allow(), "still under the ceiling")
	assert.Equal(t, int64(40), m.Remaining())

	require.NoError(t, m.Record(40)) // now exactly at the ceiling
	assert.False(t, m.Allow(), "at the ceiling → refused")
	assert.Equal(t, int64(0), m.Remaining())

	require.NoError(t, m.Record(1000)) // over the ceiling
	assert.False(t, m.Allow(), "over the ceiling → still refused")
	assert.Equal(t, int64(0), m.Remaining(), "remaining never goes negative")
}

// New fails closed: a non-positive ceiling or period, or a nil store, is refused —
// cost-safety cannot be turned off by zeroing it.
func TestNewFailsClosed(t *testing.T) {
	_, err := budget.New(budget.Config{Ceiling: 0, Period: time.Hour}, &budget.MemoryStore{})
	assert.Error(t, err, "zero ceiling refused")
	_, err = budget.New(budget.Config{Ceiling: -1, Period: time.Hour}, &budget.MemoryStore{})
	assert.Error(t, err, "negative ceiling refused")
	_, err = budget.New(budget.Config{Ceiling: 100, Period: 0}, &budget.MemoryStore{})
	assert.Error(t, err, "zero period refused")
	_, err = budget.New(budget.Config{Ceiling: 100, Period: time.Hour}, nil)
	assert.Error(t, err, "nil store refused")

	m, err := budget.New(budget.Config{Ceiling: 100, Period: time.Hour}, &budget.MemoryStore{})
	require.NoError(t, err)
	require.NotNil(t, m)
}

// A negative token count in Record is treated as zero, never a refund.
func TestRecordNegativeIsZero(t *testing.T) {
	m := newMeter(t, 100, time.Hour)
	require.NoError(t, m.Record(100))
	require.NoError(t, m.Record(-50))
	assert.False(t, m.Allow(), "a negative record does not refund spend")
}

// Concurrent Allow/Record/Remaining are race-clean (the meter is the shared cost
// backstop across request goroutines).
func TestConcurrentAccess(t *testing.T) {
	m := newMeter(t, 1_000_000, time.Hour)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				if m.Allow() {
					_ = m.Record(3)
				}
				_ = m.Remaining()
			}
		}()
	}
	wg.Wait()
	// 8*200*3 = 4800 tokens spent, well under the ceiling; still allowing.
	assert.True(t, m.Allow())
	assert.Equal(t, int64(1_000_000-4800), m.Remaining())
}

// The meter's state persists across a restart (a new Meter over the same store):
// the budget is not reset by a restart or crash-loop — the failure the ceiling
// most needs to survive.
func TestPersistenceAcrossRestart(t *testing.T) {
	store := &budget.MemoryStore{}
	m1, err := budget.New(budget.Config{Ceiling: 100, Period: time.Hour}, store)
	require.NoError(t, err)
	require.NoError(t, m1.Record(90))

	// "Restart": a fresh Meter loading the same persisted store.
	m2, err := budget.New(budget.Config{Ceiling: 100, Period: time.Hour}, store)
	require.NoError(t, err)
	assert.Equal(t, int64(10), m2.Remaining(), "prior spend survived the restart")
	require.NoError(t, m2.Record(10))
	assert.False(t, m2.Allow(), "the reloaded meter enforces the same ceiling")
}

// The bbolt store round-trips through a real reopen (persisted on disk).
func TestBoltPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "budget.db")

	store, err := budget.OpenBolt(path)
	require.NoError(t, err)
	m, err := budget.New(budget.Config{Ceiling: 100, Period: time.Hour}, store)
	require.NoError(t, err)
	require.NoError(t, m.Record(75))
	require.NoError(t, store.Close())

	// Reopen the file — a real process restart.
	store2, err := budget.OpenBolt(path)
	require.NoError(t, err)
	defer func() { _ = store2.Close() }()
	m2, err := budget.New(budget.Config{Ceiling: 100, Period: time.Hour}, store2)
	require.NoError(t, err)
	assert.Equal(t, int64(25), m2.Remaining(), "spend persisted on disk across reopen")
}

// ErrOverBudget is the exported typed refusal the model-call path returns when the
// ceiling is reached.
func TestErrOverBudgetExported(t *testing.T) {
	assert.Error(t, budget.ErrOverBudget)
	assert.Contains(t, budget.ErrOverBudget.Error(), "spend ceiling")
}
