package budget

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// When the period elapses, the meter resets: spend clears and Allow passes again.
// The elapse is forced by rewinding the period start, so the test is
// deterministic (no sleeps).
func TestPeriodReset(t *testing.T) {
	m, err := New(Config{Ceiling: 100, Period: time.Hour}, &MemoryStore{})
	require.NoError(t, err)
	require.NoError(t, m.Record(100))
	assert.False(t, m.Allow(), "at the ceiling before the window elapses")

	m.mu.Lock()
	m.start = time.Now().Add(-2 * time.Hour) // window has elapsed
	m.mu.Unlock()

	assert.True(t, m.Allow(), "an elapsed window resets the meter")
	assert.Equal(t, int64(100), m.Remaining(), "reset restores the full ceiling")
}

// The reset is persisted, so a restart just after a period boundary sees the
// fresh period rather than resurrecting the spent old one.
func TestPeriodResetPersists(t *testing.T) {
	store := &MemoryStore{}
	m, err := New(Config{Ceiling: 100, Period: time.Hour}, store)
	require.NoError(t, err)
	require.NoError(t, m.Record(100))

	m.mu.Lock()
	m.start = time.Now().Add(-2 * time.Hour)
	m.mu.Unlock()
	m.Allow() // triggers the roll and its best-effort persist

	m2, err := New(Config{Ceiling: 100, Period: time.Hour}, store)
	require.NoError(t, err)
	assert.Equal(t, int64(100), m2.Remaining(), "the roll was persisted, not the spent period")
}
