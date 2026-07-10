package pending

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"

	"github.com/yaad-index/yaad-grove/internal/core"
)

const testTTL = 10 * time.Minute

// clock is a manually-advanced time source so expiry is deterministic.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) advance(d time.Duration) {
	c.t = c.t.Add(d)
}

// stores returns both implementations wired to clk, so the one suite covers the
// memory and bolt variants identically.
func stores(t *testing.T, clk *clock) map[string]Store {
	t.Helper()
	mem := NewMemoryStore(testTTL)
	mem.now = clk.now

	// sweepInterval 0: no background sweeper, so the manual clock these tests
	// inject is never raced by an auto-sweep goroutine.
	blt, err := OpenBolt(filepath.Join(t.TempDir(), "pending.db"), testTTL, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blt.Close() })
	blt.now = clk.now

	return map[string]Store{"memory": mem, "bolt": blt}
}

var echoAction = core.Action{Verb: "echo", Params: map[string]string{"say": "hi"}, Label: "Echo"}

func TestResolveLifecycle(t *testing.T) {
	for name, st := range stores(t, &clock{t: time.Unix(1_700_000_000, 0)}) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			// Unknown token -> Expired.
			_, status, err := st.Resolve(ctx, "no-such-token")
			require.NoError(t, err)
			assert.Equal(t, Expired, status)

			// Fresh token -> Resolved, returning the stored action.
			token, err := st.Put(ctx, echoAction)
			require.NoError(t, err)
			assert.NotEmpty(t, token)
			got, status, err := st.Resolve(ctx, token)
			require.NoError(t, err)
			assert.Equal(t, Resolved, status)
			assert.Equal(t, echoAction, got)

			// Replayed token -> Consumed, no action.
			got, status, err = st.Resolve(ctx, token)
			require.NoError(t, err)
			assert.Equal(t, Consumed, status)
			assert.Empty(t, got.Verb, "a replayed token returns no action")
		})
	}
}

// An unconsumed token past its TTL resolves as Expired.
func TestResolveExpiry(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	for name, st := range stores(t, clk) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			token, err := st.Put(ctx, echoAction)
			require.NoError(t, err)

			clk.advance(testTTL + time.Second)
			_, status, err := st.Resolve(ctx, token)
			require.NoError(t, err)
			assert.Equal(t, Expired, status)
		})
	}
}

// A consumed token that later ages past its TTL still reads Consumed, not
// Expired — the consumed check precedes the TTL check (the user already did it).
func TestConsumedBeatsExpiry(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	for name, st := range stores(t, clk) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			token, err := st.Put(ctx, echoAction)
			require.NoError(t, err)

			_, status, err := st.Resolve(ctx, token) // consume within TTL
			require.NoError(t, err)
			require.Equal(t, Resolved, status)

			clk.advance(testTTL + time.Hour) // now well past TTL
			_, status, err = st.Resolve(ctx, token)
			require.NoError(t, err)
			assert.Equal(t, Consumed, status, "a consumed token stays 'already completed' past its TTL")
		})
	}
}

// Sweep drops records past their TTL (unclicked and tombstoned alike) and keeps
// the rest.
func TestSweep(t *testing.T) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	for name, st := range stores(t, clk) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()

			// Two tokens now; one gets consumed (tombstoned).
			old1, err := st.Put(ctx, echoAction)
			require.NoError(t, err)
			old2, err := st.Put(ctx, echoAction)
			require.NoError(t, err)
			_, status, err := st.Resolve(ctx, old2)
			require.NoError(t, err)
			require.Equal(t, Resolved, status)

			// Age both past TTL, then mint a fresh one that must survive.
			clk.advance(testTTL + time.Second)
			fresh, err := st.Put(ctx, echoAction)
			require.NoError(t, err)

			n, err := st.Sweep(ctx)
			require.NoError(t, err)
			assert.Equal(t, 2, n, "both aged records (unclicked + tombstoned) are swept")

			// The old tokens are gone; the fresh one still resolves.
			_, status, err = st.Resolve(ctx, old1)
			require.NoError(t, err)
			assert.Equal(t, Expired, status)
			_, status, err = st.Resolve(ctx, fresh)
			require.NoError(t, err)
			assert.Equal(t, Resolved, status)
		})
	}
}

// memSize reads the record count under the store's own lock, so it is safe to
// poll while a sweeper goroutine runs.
func memSize(s *MemoryStore) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.m)
}

// boltCount reads the record count from the bucket.
func boltCount(t *testing.T, s *BoltStore) int {
	t.Helper()
	n := 0
	require.NoError(t, s.db.View(func(tx *bbolt.Tx) error {
		n = tx.Bucket(pendingBucket).Stats().KeyN
		return nil
	}))
	return n
}

// RunSweeper drives Sweep on a ticker and removes expired records until ctx is
// cancelled. Uses real (short) time — no clock injection — since it exercises the
// ticker itself.
func TestRunSweeper(t *testing.T) {
	st := NewMemoryStore(20 * time.Millisecond) // short TTL
	_, err := st.Put(context.Background(), echoAction)
	require.NoError(t, err)
	_, err = st.Put(context.Background(), echoAction)
	require.NoError(t, err)
	require.Equal(t, 2, memSize(st))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunSweeper(ctx, st, 5*time.Millisecond)

	assert.Eventually(t, func() bool { return memSize(st) == 0 }, time.Second, 5*time.Millisecond,
		"the sweeper removes expired records")
}

// A non-positive interval disables RunSweeper: it returns at once without a
// ticker, sweeping nothing.
func TestRunSweeperDisabled(t *testing.T) {
	st := NewMemoryStore(time.Nanosecond)
	_, err := st.Put(context.Background(), echoAction)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() { RunSweeper(context.Background(), st, 0); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunSweeper with interval<=0 should return immediately")
	}
	// Nothing was swept even though the record is expired.
	assert.Equal(t, 1, memSize(st))
}

// OpenBolt with a positive interval sweeps in the background, and Close stops it
// cleanly (joining the goroutine) rather than leaking a ticker.
func TestBoltAutoSweepAndClose(t *testing.T) {
	st, err := OpenBolt(filepath.Join(t.TempDir(), "sweep.db"), 20*time.Millisecond, 5*time.Millisecond)
	require.NoError(t, err)

	_, err = st.Put(context.Background(), echoAction)
	require.NoError(t, err)
	assert.Eventually(t, func() bool { return boltCount(t, st) == 0 }, time.Second, 5*time.Millisecond,
		"the background sweeper removes the expired record")

	done := make(chan struct{})
	go func() { _ = st.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close should join the sweeper goroutine and return, not hang")
	}
}

// Tokens are distinct per Put.
func TestTokensAreUnique(t *testing.T) {
	st := NewMemoryStore(testTTL)
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		token, err := st.Put(context.Background(), echoAction)
		require.NoError(t, err)
		assert.False(t, seen[token], "duplicate token minted")
		seen[token] = true
	}
}
