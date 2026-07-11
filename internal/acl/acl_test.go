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

func check(t *testing.T, g *acl.Gate, id string, surface core.Surface) acl.Decision {
	t.Helper()
	d, err := g.Check(context.Background(), core.User{ID: id}, surface)
	require.NoError(t, err)
	return d
}

// Consent is a hard gate (ADR 0002): a first-seen user is asked (never implied
// yes); continuing after the prompt is the opt-in → grant + serve; an explicitly
// declined user is never auto-granted; granted → serve.
func TestConsentGate(t *testing.T) {
	store := newMemStore()
	g := acl.NewGate(store, acl.TierDefault)

	// First-seen (ConsentUnknown) → ask, never serve.
	assert.Equal(t, acl.DecideAskConsent, check(t, g, "u1", core.SurfaceGroup))
	// The prompt was recorded (throttle timestamp) but consent stays unknown and
	// nothing was counted — the minimal-record guarantee.
	rec := store.recs["u1"]
	assert.Equal(t, acl.ConsentUnknown, rec.Consent, "consent is never implied")
	assert.False(t, rec.LastPromptedAt.IsZero(), "prompt time recorded")
	assert.Equal(t, 0, rec.RateCount, "an unconsented user is not counted")

	// The user's next message, now prompted, is the opt-in ("keep chatting, you're
	// opting in"): serve, the record flips to Granted, and it is now counted.
	assert.Equal(t, acl.DecideServe, check(t, g, "u1", core.SurfaceGroup))
	rec = store.recs["u1"]
	assert.Equal(t, acl.ConsentGranted, rec.Consent, "continuing after the prompt grants consent")
	assert.Equal(t, 1, rec.RateCount, "the now-consented message is counted")

	// Declined does NOT auto-grant — it stays a prompt, never serves.
	store.recs["u2"] = acl.Record{UserID: "u2", Consent: acl.ConsentDeclined}
	assert.Equal(t, acl.DecideAskConsent, check(t, g, "u2", core.SurfaceGroup))

	// Granted → serve.
	store.recs["u3"] = acl.Record{UserID: "u3", Consent: acl.ConsentGranted}
	assert.Equal(t, acl.DecideServe, check(t, g, "u3", core.SurfaceGroup))
}

// The opt-in isn't time-bounded: an unconsented user who was already prompted
// opts in by returning even long after the prompt-throttle window elapsed — the
// grant precedes the re-prompt throttle.
func TestConsentOptInAfterThrottle(t *testing.T) {
	store := newMemStore(acl.Record{
		UserID:         "u1",
		Consent:        acl.ConsentUnknown,
		LastPromptedAt: time.Now().Add(-2 * time.Hour), // older than the 1h throttle
	})
	g := acl.NewGate(store, acl.TierDefault)
	assert.Equal(t, acl.DecideServe, check(t, g, "u1", core.SurfaceGroup))
	assert.Equal(t, acl.ConsentGranted, store.recs["u1"].Consent, "returning opts in regardless of elapsed time")
}

// A declined user who was recently prompted stays silent, never serves — decline
// is never overridden by the opt-in-by-continuing path.
func TestConsentDeclinedNeverServes(t *testing.T) {
	store := newMemStore(acl.Record{
		UserID:         "u1",
		Consent:        acl.ConsentDeclined,
		LastPromptedAt: time.Now(),
	})
	g := acl.NewGate(store, acl.TierDefault)
	assert.Equal(t, acl.DecideSilent, check(t, g, "u1", core.SurfaceGroup))
	assert.Equal(t, acl.ConsentDeclined, store.recs["u1"].Consent, "decline is not auto-granted")
}

// Surface reach (ADR 0003): a DM is served only to an admin-approved user; the
// group surface is membership-bounded upstream. Surface refusal precedes consent.
func TestSurfaceReach(t *testing.T) {
	store := newMemStore(
		acl.Record{UserID: "dm-unapproved", Consent: acl.ConsentGranted},
		acl.Record{UserID: "dm-approved", Consent: acl.ConsentGranted, DMApproved: true},
	)
	g := acl.NewGate(store, acl.TierDefault)

	// Unapproved DM → refuse even though consent is granted (surface gates first).
	assert.Equal(t, acl.DecideRefuse, check(t, g, "dm-unapproved", core.SurfaceDM))
	// Approved DM + granted → serve.
	assert.Equal(t, acl.DecideServe, check(t, g, "dm-approved", core.SurfaceDM))
	// The same unapproved user in a group is fine (membership is the boundary).
	assert.Equal(t, acl.DecideServe, check(t, g, "dm-unapproved", core.SurfaceGroup))
}

// Rate limit (consented users only): under the tier allowance serves, at it
// refuses; the window reset restores the allowance; unlimited tiers never limit.
func TestRateLimit(t *testing.T) {
	// Default tier = throttled (allowance 5).
	store := newMemStore(acl.Record{UserID: "u1", Consent: acl.ConsentGranted})
	g := acl.NewGate(store, acl.TierThrottled)
	for i := 0; i < 5; i++ {
		assert.Equal(t, acl.DecideServe, check(t, g, "u1", core.SurfaceGroup), "call %d", i+1)
	}
	assert.Equal(t, acl.DecideRateLimited, check(t, g, "u1", core.SurfaceGroup), "6th over the allowance")

	// Window reset: an elapsed window restores the allowance.
	r := store.recs["u1"]
	r.RateWindowStart = time.Now().Add(-2 * time.Hour)
	store.recs["u1"] = r
	assert.Equal(t, acl.DecideServe, check(t, g, "u1", core.SurfaceGroup), "reset restores the allowance")

	// An unlimited tier (admin) is never rate-limited.
	store.recs["admin"] = acl.Record{UserID: "admin", Consent: acl.ConsentGranted, Tier: acl.TierAdmin}
	for i := 0; i < 50; i++ {
		require.Equal(t, acl.DecideServe, check(t, g, "admin", core.SurfaceGroup))
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
	assert.Equal(t, acl.DecideServe, check(t, g, "vip", core.SurfaceGroup),
		"per-user tier override beats the instance default")

	// A user with no override falls to the default (throttled) and is limited at 5.
	store.recs["plain"] = acl.Record{
		UserID: "plain", Consent: acl.ConsentGranted,
		RateCount: 5, RateWindowStart: time.Now(),
	}
	assert.Equal(t, acl.DecideRateLimited, check(t, g, "plain", core.SurfaceGroup))
}

// A store error fails closed: the gate refuses, never serves on an unknown state.
func TestFailClosedOnStoreError(t *testing.T) {
	g := acl.NewGate(failStore{}, acl.TierDefault)
	d, err := g.Check(context.Background(), core.User{ID: "u1"}, core.SurfaceGroup)
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

	rec := acl.Record{UserID: "u1", Consent: acl.ConsentGranted, Tier: acl.TierTrusted, DMApproved: true, RateCount: 3}
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
	assert.True(t, got.DMApproved)
	assert.Equal(t, 3, got.RateCount)
}
