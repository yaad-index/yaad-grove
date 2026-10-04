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

// The purge runs at once, then after each window boundary, over every
// namespace; a failed purge stops neither the other namespaces' nor the next
// window's. It stops when its context ends.
func TestRunPurgeEveryWindow(t *testing.T) {
	window := 24 * time.Hour
	now := time.Date(2026, 10, 4, 7, 0, 0, 0, time.UTC)
	waits := make(chan time.Duration, 10)
	ticks := make(chan time.Time)
	after := func(d time.Duration) <-chan time.Time {
		waits <- d
		return ticks
	}
	calls := make(chan struct{})
	p := &countingPurger{fail: map[int]bool{1: true, 2: true}, calls: calls}
	q := &countingPurger{calls: calls}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runtime.RunPurge(ctx, []runtime.Purger{p, q}, window, func() time.Time { return now }, after)
		close(done)
	}()

	<-calls // at start, before any wait
	<-calls
	assert.Equal(t, 17*time.Hour+time.Minute, <-waits, "then it waits for a minute past the next boundary")
	for range 2 {
		ticks <- now
		<-calls
		<-calls
		<-waits
	}
	cancel()
	<-done
	assert.Equal(t, 3, p.n, "a failed purge does not stop the next window's")
	assert.Equal(t, 3, q.n, "a failed purge does not stop the other namespace's")
}

// The cut is retention before the start of the current window, a window
// boundary wherever in the window the purge runs, so it falls between sessions.
func TestPurgeCut(t *testing.T) {
	day := 24 * time.Hour
	retention := 30 * day
	boundary := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	want := boundary.Add(-retention)
	for _, at := range []time.Time{boundary, boundary.Add(time.Minute), boundary.Add(day - time.Nanosecond)} {
		cut := runtime.PurgeCut(at, day, retention)
		assert.Equal(t, want, cut, "at %s", at)
		assert.Equal(t, cut, core.WindowStart(cut, day), "the cut is a window boundary")
	}
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

// fakeBackend records the cuts it deletes before, failing while fail is set.
type fakeBackend struct {
	fail bool
	cuts []time.Time
}

func (b *fakeBackend) DeleteBefore(_ context.Context, t time.Time) error {
	b.cuts = append(b.cuts, t)
	if b.fail {
		return errors.New("service down")
	}
	return nil
}

// A namespace is purged at the aligned cut. A configured one is never
// forgotten. A dropped one is forgotten only after a purge succeeds whose cut
// has reached the moment it was dropped, since nothing was kept in it after.
func TestNamespacePurge(t *testing.T) {
	ctx := context.Background()
	day := 24 * time.Hour
	dropped := time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)
	record, err := namespaces.Open(filepath.Join(t.TempDir(), "ns.json"))
	require.NoError(t, err)
	require.NoError(t, record.Configured("inst-a", "club"))
	_, err = record.Dropped("club", dropped)
	require.NoError(t, err)
	now := dropped
	clock := func() time.Time { return now }

	inst := &fakeBackend{}
	configured := runtime.NamespacePurge{Namespace: "inst-a", Backend: inst, Record: record, Retention: 2 * day, Window: day, Now: clock}
	now = dropped.Add(10 * day)
	require.NoError(t, configured.Purge(ctx))
	assert.Equal(t, []time.Time{runtime.PurgeCut(now, day, 2*day)}, inst.cuts)
	assert.Contains(t, record.Namespaces(), "inst-a", "a configured namespace is never forgotten")

	club := &fakeBackend{}
	p := runtime.NamespacePurge{Namespace: "club", Backend: club, Record: record, Retention: 2 * day, Window: day, Dropped: dropped, Now: clock}
	// Two days and a window later the cut is the start of the drop day: still
	// before the drop.
	now = dropped.Add(2 * day)
	require.NoError(t, p.Purge(ctx))
	assert.Contains(t, record.Namespaces(), "club", "not forgotten while the cut is before the drop")

	now = dropped.Add(3 * day)
	club.fail = true
	require.ErrorContains(t, p.Purge(ctx), `namespace "club"`)
	assert.Contains(t, record.Namespaces(), "club", "not forgotten after a failed purge")

	club.fail = false
	require.NoError(t, p.Purge(ctx))
	assert.False(t, club.cuts[len(club.cuts)-1].Before(dropped), "the cut has reached the drop")
	assert.NotContains(t, record.Namespaces(), "club", "forgotten once a purge's cut reaches the drop")
	assert.Contains(t, record.Namespaces(), "inst-a")
}
