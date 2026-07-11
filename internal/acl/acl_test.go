package acl_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/core"
)

// memStore is an in-memory Store for tests; a first-seen user yields a zero
// Record (mirroring the bbolt store).
type memStore struct{ recs map[string]acl.Record }

func newMemStore(seed ...acl.Record) *memStore {
	m := &memStore{recs: map[string]acl.Record{}}
	for _, r := range seed {
		m.recs[r.UserID] = r
	}
	return m
}

func (m *memStore) Get(_ context.Context, id string) (acl.Record, error) {
	if r, ok := m.recs[id]; ok {
		return r, nil
	}
	return acl.Record{UserID: id}, nil
}

func (m *memStore) Put(_ context.Context, r acl.Record) error {
	m.recs[r.UserID] = r
	return nil
}

func (m *memStore) Update(_ context.Context, id string, mutate func(*acl.Record) error) error {
	rec, ok := m.recs[id]
	if !ok {
		rec = acl.Record{UserID: id}
	}
	if err := mutate(&rec); err != nil {
		return err
	}
	m.recs[id] = rec
	return nil
}

// failStore errors on Get/Update — for the fail-closed tests.
type failStore struct{}

func (failStore) Get(context.Context, string) (acl.Record, error) {
	return acl.Record{}, errors.New("boom")
}
func (failStore) Put(context.Context, acl.Record) error { return nil }
func (failStore) Update(context.Context, string, func(*acl.Record) error) error {
	return errors.New("boom")
}

// decide runs the group gate for a user with the given directedness.
func decide(t *testing.T, g *acl.Gate, id string, directed bool) acl.Decision {
	t.Helper()
	d, err := g.Check(context.Background(), acl.GateInput{
		User: core.User{ID: id}, Surface: core.SurfaceGroup, Directed: directed,
	})
	require.NoError(t, err)
	return d
}

// The consent × directedness matrix (ADR 0012): consent gates both answering and
// logging; directedness splits the reply. Consent is never implied.
func TestConsentGateMatrix(t *testing.T) {
	store := newMemStore()
	g := acl.NewGate(store, acl.TierDefault)

	// Unconsented (first-seen): directed → nudge, ambient → silent.
	assert.Equal(t, acl.DecideNudge, decide(t, g, "u1", true), "unconsented directed → nudge")
	assert.Equal(t, acl.DecideSilent, decide(t, g, "u1", false), "unconsented ambient → ignored")
	// Nothing was recorded — the minimal-record guarantee: no consent is written,
	// no rate row is created for a user who never consented.
	_, recorded := store.recs["u1"]
	assert.False(t, recorded, "an unconsented user is never recorded")

	// Consented: directed → serve, ambient → log-only.
	store.recs["c1"] = acl.Record{UserID: "c1", Consent: acl.ConsentGranted}
	assert.Equal(t, acl.DecideServe, decide(t, g, "c1", true), "consented directed → serve")
	assert.Equal(t, acl.DecideLogOnly, decide(t, g, "c1", false), "consented ambient → log-only")

	// Declined collapses to the same unconsented handling (nudge when directed).
	store.recs["d1"] = acl.Record{UserID: "d1", Consent: acl.ConsentDeclined}
	assert.Equal(t, acl.DecideNudge, decide(t, g, "d1", true), "declined directed → nudge")
	assert.Equal(t, acl.DecideSilent, decide(t, g, "d1", false), "declined ambient → ignored")
}

// Consented ambient chatter is logged but not rate-counted — logging is cheap and
// nothing is answered, so it must not consume the answer allowance.
func TestAmbientLogOnlyNotRateCounted(t *testing.T) {
	store := newMemStore(acl.Record{UserID: "u1", Consent: acl.ConsentGranted})
	g := acl.NewGate(store, acl.TierThrottled) // allowance 5

	for i := 0; i < 10; i++ {
		require.Equal(t, acl.DecideLogOnly, decide(t, g, "u1", false), "ambient %d", i+1)
	}
	assert.Equal(t, 0, store.recs["u1"].RateCount, "ambient chatter never touches the rate counter")
	// A directed message still has its full allowance available.
	assert.Equal(t, acl.DecideServe, decide(t, g, "u1", true))
}

// Rate limit on the directed (answered) path only: under the tier allowance
// serves, at it rate-limits; a window reset restores it; unlimited tiers never
// limit.
func TestRateLimitDirected(t *testing.T) {
	store := newMemStore(acl.Record{UserID: "u1", Consent: acl.ConsentGranted})
	g := acl.NewGate(store, acl.TierThrottled) // allowance 5
	for i := 0; i < 5; i++ {
		assert.Equal(t, acl.DecideServe, decide(t, g, "u1", true), "call %d", i+1)
	}
	assert.Equal(t, acl.DecideRateLimited, decide(t, g, "u1", true), "6th over the allowance")

	// Window reset: an elapsed window restores the allowance.
	r := store.recs["u1"]
	r.RateWindowStart = time.Now().Add(-2 * time.Hour)
	store.recs["u1"] = r
	assert.Equal(t, acl.DecideServe, decide(t, g, "u1", true), "reset restores the allowance")

	// An unlimited tier (admin) is never rate-limited.
	store.recs["admin"] = acl.Record{UserID: "admin", Consent: acl.ConsentGranted, Tier: acl.TierAdmin}
	for i := 0; i < 50; i++ {
		require.Equal(t, acl.DecideServe, decide(t, g, "admin", true))
	}
}

// Layered tier: a per-user tier override beats the instance default (ADR 0003).
func TestTierResolutionOverrideBeatsDefault(t *testing.T) {
	// Instance default is throttled (5); the user is overridden to trusted (120).
	// Both sit at 5 in a current window (a zero window would reset the count).
	store := newMemStore(acl.Record{
		UserID: "vip", Consent: acl.ConsentGranted, Tier: acl.TierTrusted,
		RateCount: 5, RateWindowStart: time.Now(),
	})
	g := acl.NewGate(store, acl.TierThrottled)
	// At 5 calls a throttled user is over the limit, but the trusted override is not.
	assert.Equal(t, acl.DecideServe, decide(t, g, "vip", true),
		"per-user tier override beats the instance default")

	// A user with no override falls to the default (throttled) and is limited at 5.
	store.recs["plain"] = acl.Record{
		UserID: "plain", Consent: acl.ConsentGranted,
		RateCount: 5, RateWindowStart: time.Now(),
	}
	assert.Equal(t, acl.DecideRateLimited, decide(t, g, "plain", true))
}

// A store error fails closed: the gate refuses, never serves on an unknown state.
func TestFailClosedOnStoreError(t *testing.T) {
	g := acl.NewGate(failStore{}, acl.TierDefault)
	d, err := g.Check(context.Background(), acl.GateInput{
		User: core.User{ID: "u1"}, Surface: core.SurfaceGroup, Directed: true,
	})
	assert.Error(t, err)
	assert.Equal(t, acl.DecideRefuse, d)
}

// The bbolt store round-trips a Record across a reopen; a first-seen user is a
// zero Record, not an error.
func TestBoltStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acl.db")
	ctx := context.Background()

	s1, err := acl.OpenBolt(path)
	require.NoError(t, err)

	// First-seen → zero Record with the id set, ConsentUnknown.
	got, err := s1.Get(ctx, "new")
	require.NoError(t, err)
	assert.Equal(t, acl.Record{UserID: "new"}, got)
	assert.Equal(t, acl.ConsentUnknown, got.Consent)

	rec := acl.Record{UserID: "u1", Consent: acl.ConsentGranted, Tier: acl.TierTrusted, RateCount: 3}
	require.NoError(t, s1.Put(ctx, rec))
	require.NoError(t, s1.Close())

	// Reopen — a real restart.
	s2, err := acl.OpenBolt(path)
	require.NoError(t, err)
	defer func() { _ = s2.Close() }()
	got, err = s2.Get(ctx, "u1")
	require.NoError(t, err)
	assert.Equal(t, acl.ConsentGranted, got.Consent)
	assert.Equal(t, acl.TierTrusted, got.Tier)
	assert.Equal(t, 3, got.RateCount)
}
