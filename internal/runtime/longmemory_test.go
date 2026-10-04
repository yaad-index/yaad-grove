package runtime_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/namespaces"
	"github.com/yaad-index/yaad-grove/internal/runtime"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

// Only a consented, directed group turn may use long-term memory, and its
// query names the chat (ADR 0023 §5).
func TestHandlerRememberOnlyConsentedGroupTurns(t *testing.T) {
	engine := &mockEngine{reply: core.Reply{Text: "answer"}}
	_, err := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, engine, nil, nil, nil, nil, nil, runtime.Policy{})(context.Background(), inbound)
	require.NoError(t, err)
	require.True(t, engine.called)
	assert.True(t, engine.gotQuery.Remember, "a served group turn is kept")
	assert.Equal(t, "chat-1", engine.gotQuery.Chat)

	// An admin's DM is answered without memory: the admin need not have
	// consented.
	engine = &mockEngine{reply: core.Reply{Text: "answer"}}
	policy := runtime.Policy{Admins: runtime.NewAdminSet([]string{"admin1"})}
	h := runtime.NewHandler(&mockGate{}, engine, nil, nil, nil, nil, &mockConsenter{consent: acl.ConsentUnknown}, policy)
	_, err = h(context.Background(), transport.Inbound{User: core.User{ID: "admin1"}, Surface: core.SurfaceDM, Text: "q", ReplyTo: "dm-1", Directed: true})
	require.NoError(t, err)
	require.True(t, engine.called)
	assert.False(t, engine.gotQuery.Remember, "an admin DM is not kept")

	// A DM the gate serves, on a bot with no consent surface, is not kept
	// either.
	engine = &mockEngine{reply: core.Reply{Text: "answer"}}
	dm := inbound
	dm.Surface = core.SurfaceDM
	_, err = runtime.NewHandler(&mockGate{decision: acl.DecideServe}, engine, nil, nil, nil, nil, nil, runtime.Policy{})(context.Background(), dm)
	require.NoError(t, err)
	require.True(t, engine.called)
	assert.False(t, engine.gotQuery.Remember, "a served DM is not kept")
}

// countingPurger counts purges and fails the ones listed.
type countingPurger struct {
	mu    sync.Mutex
	n     int
	fail  map[int]bool
	calls chan struct{}
}

func (p *countingPurger) Purge(context.Context) error {
	p.mu.Lock()
	p.n++
	n := p.n
	p.mu.Unlock()
	p.calls <- struct{}{}
	if p.fail[n] {
		return errors.New("service down")
	}
	return nil
}

// The purge waits for each window boundary and purges every store; a failed
// purge stops neither the other stores' nor the next window's. It stops when
// its context ends.
func TestRunPurgeEveryWindow(t *testing.T) {
	window := 24 * time.Hour
	now := time.Date(2026, 10, 4, 7, 0, 0, 0, time.UTC)
	var waits []time.Duration
	ticks := make(chan time.Time)
	after := func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		return ticks
	}
	calls := make(chan struct{})
	p := &countingPurger{fail: map[int]bool{1: true}, calls: calls}
	q := &countingPurger{calls: calls}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runtime.RunPurge(ctx, []runtime.Purger{p, q}, window, func() time.Time { return now }, after)
		close(done)
	}()

	for range 3 {
		ticks <- now
		<-calls
		<-calls
	}
	cancel()
	<-done
	assert.Equal(t, 3, p.n, "a failed purge does not stop the next window's")
	assert.Equal(t, 3, q.n, "a failed purge does not stop the other store's")
	require.NotEmpty(t, waits)
	assert.Equal(t, 17*time.Hour+time.Minute, waits[0], "the first purge waits for a minute past the next boundary")
}

// The wait reaches a minute past a window boundary: the next one, or the one
// just passed while still within that minute.
func TestUntilPurge(t *testing.T) {
	day := 24 * time.Hour
	midnight := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		at   time.Time
		want time.Duration
	}{
		{"mid-window", midnight.Add(7 * time.Hour), 17*time.Hour + time.Minute},
		{"just past a boundary", midnight.Add(30 * time.Second), 30 * time.Second},
		{"right after a purge", midnight.Add(time.Minute), day},
		{"just before a boundary", midnight.Add(-time.Second), time.Minute + time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, runtime.UntilPurge(c.at, day))
		})
	}
}

// fakeStore fails its purges while fail is set.
type fakeStore struct {
	fail   bool
	purged int
}

func (s *fakeStore) Purge(context.Context) error {
	s.purged++
	if s.fail {
		return errors.New("service down")
	}
	return nil
}

// A configured namespace is recorded as seen at every purge, even one that
// fails, and is never forgotten. A dropped one is forgotten only after a purge
// in it succeeds once keep has passed since it was last seen.
func TestNamespacePurge(t *testing.T) {
	ctx := context.Background()
	t0 := time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)
	record, err := namespaces.Open(filepath.Join(t.TempDir(), "ns.json"))
	require.NoError(t, err)
	require.NoError(t, record.Seen(t0, "inst-a", "club"))
	now := t0
	clock := func() time.Time { return now }

	inst := &fakeStore{fail: true}
	configured := runtime.NamespacePurge{Namespace: "inst-a", Store: inst, Record: record, Configured: true, Keep: time.Hour, Now: clock}
	now = t0.Add(5 * time.Hour)
	require.Error(t, configured.Purge(ctx))
	forgot, err := record.Forget("inst-a", t0.Add(5*time.Hour+30*time.Minute), time.Hour)
	require.NoError(t, err)
	assert.False(t, forgot, "a failed purge still records the configured namespace as seen")

	club := &fakeStore{fail: true}
	dropped := runtime.NamespacePurge{Namespace: "club", Store: club, Record: record, Keep: time.Hour, Now: clock}
	now = t0.Add(30 * time.Minute)
	club.fail = false
	require.NoError(t, dropped.Purge(ctx))
	assert.Contains(t, record.Namespaces(), "club", "not forgotten before keep has passed")

	now = t0.Add(2 * time.Hour)
	club.fail = true
	require.Error(t, dropped.Purge(ctx))
	assert.Contains(t, record.Namespaces(), "club", "not forgotten after a failed purge")

	club.fail = false
	require.NoError(t, dropped.Purge(ctx))
	assert.NotContains(t, record.Namespaces(), "club", "forgotten after a purge succeeds once keep has passed")
	assert.Contains(t, record.Namespaces(), "inst-a")
}
