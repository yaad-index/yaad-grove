package namespaces_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/namespaces"
)

var t0 = time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)

// A missing file is an empty record; what Seen records is on disk when it
// returns, and a fresh Open reads it back.
func TestRecordSeenIsDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ns.json")
	r, err := namespaces.Open(path)
	require.NoError(t, err)
	assert.Empty(t, r.Namespaces())

	require.NoError(t, r.Seen(t0, "inst-a", "club"))
	reread, err := namespaces.Open(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"club", "inst-a"}, reread.Namespaces())

	tmps, err := filepath.Glob(path + ".*.tmp")
	require.NoError(t, err)
	assert.Empty(t, tmps, "a save leaves no temporary file behind")
}

// A namespace is forgotten only once keep has passed since it was last seen,
// and the forgetting is on disk.
func TestRecordForget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ns.json")
	r, err := namespaces.Open(path)
	require.NoError(t, err)
	require.NoError(t, r.Seen(t0, "club"))
	require.NoError(t, r.Seen(t0.Add(time.Hour), "club"), "a later sighting moves the clock")

	forgot, err := r.Forget("club", t0.Add(2*time.Hour), 2*time.Hour)
	require.NoError(t, err)
	assert.False(t, forgot, "not yet expired since it was last seen")
	assert.Equal(t, []string{"club"}, r.Namespaces())

	forgot, err = r.Forget("club", t0.Add(3*time.Hour), 2*time.Hour)
	require.NoError(t, err)
	assert.True(t, forgot)
	reread, err := namespaces.Open(path)
	require.NoError(t, err)
	assert.Empty(t, reread.Namespaces(), "the forgetting is saved")

	forgot, err = r.Forget("never-seen", t0.Add(10*time.Hour), time.Hour)
	require.NoError(t, err)
	assert.False(t, forgot)
}

// A record that cannot be read is an error, never an empty record that would
// let a dropped namespace go unpurged.
func TestRecordUnreadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ns.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))
	_, err := namespaces.Open(path)
	require.Error(t, err)
}

// A record that exists but cannot be read is an error too.
func TestRecordReadError(t *testing.T) {
	_, err := namespaces.Open(t.TempDir()) // a directory, not a file
	require.Error(t, err)
}
