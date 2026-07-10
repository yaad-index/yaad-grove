package acl_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/core"
)

func TestAtLeast(t *testing.T) {
	assert.True(t, acl.AtLeast(acl.TierAdmin, acl.TierAdmin))
	assert.True(t, acl.AtLeast(acl.TierAdmin, acl.TierTrusted))
	assert.True(t, acl.AtLeast(acl.TierTrusted, acl.TierDefault))

	// A rate grant is not an authority grant: unlimited does not clear admin.
	assert.False(t, acl.AtLeast(acl.TierUnlimited, acl.TierAdmin))
	assert.False(t, acl.AtLeast(acl.TierDefault, acl.TierAdmin))

	// Unknown requirement fails closed; unknown holder ranks lowest.
	assert.False(t, acl.AtLeast(acl.TierAdmin, acl.Tier("bogus")))
	assert.False(t, acl.AtLeast(acl.Tier("bogus"), acl.TierDefault))
}

func TestValidTier(t *testing.T) {
	assert.True(t, acl.ValidTier(acl.TierAdmin))
	assert.False(t, acl.ValidTier(acl.Tier("nope")))
	assert.False(t, acl.ValidTier(acl.Tier("")))
}

// Authorize reads the tier fresh: an admin clears an admin requirement; a
// non-admin does not; a store error fails closed.
func TestAuthorize(t *testing.T) {
	ctx := context.Background()
	admin := core.User{ID: "a"}
	store := newMemStore(acl.Record{UserID: "a", Tier: acl.TierAdmin})
	gate := acl.NewGate(store, acl.TierDefault)

	ok, err := gate.Authorize(ctx, admin, acl.TierAdmin)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = gate.Authorize(ctx, core.User{ID: "b"}, acl.TierAdmin) // unseen -> default tier
	require.NoError(t, err)
	assert.False(t, ok)

	ok, err = acl.NewGate(failStore{}, acl.TierDefault).Authorize(ctx, admin, acl.TierAdmin)
	require.Error(t, err)
	assert.False(t, ok, "a store error fails closed")
}

// The load-bearing rule: a demotion between render and click denies. Authorize
// reads the tier at call time, not a render-time snapshot.
func TestAuthorizeDemotionDenies(t *testing.T) {
	ctx := context.Background()
	user := core.User{ID: "a"}
	store := newMemStore(acl.Record{UserID: "a", Tier: acl.TierAdmin})
	gate := acl.NewGate(store, acl.TierDefault)

	ok, err := gate.Authorize(ctx, user, acl.TierAdmin)
	require.NoError(t, err)
	require.True(t, ok, "admin authorized at render time")

	// Demote after the button was shown.
	require.NoError(t, gate.SetTier(ctx, "a", acl.TierDefault))

	ok, err = gate.Authorize(ctx, user, acl.TierAdmin)
	require.NoError(t, err)
	assert.False(t, ok, "the click is re-authorized against the current tier — denied")
}

// SetTier persists the tier, refuses an unknown tier, and creates a row for an
// unseen user.
func TestSetTier(t *testing.T) {
	ctx := context.Background()
	store := newMemStore()
	gate := acl.NewGate(store, acl.TierDefault)

	require.NoError(t, gate.SetTier(ctx, "newuser", acl.TierTrusted))
	rec, err := store.Get(ctx, "newuser")
	require.NoError(t, err)
	assert.Equal(t, acl.TierTrusted, rec.Tier)

	assert.ErrorIs(t, gate.SetTier(ctx, "newuser", acl.Tier("bogus")), acl.ErrUnknownTier)
}
