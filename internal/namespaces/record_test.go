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

// A missing file is an empty record; what Configured records is on disk when
// it returns, and a fresh Open reads it back.
func TestRecordConfiguredIsDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ns.json")
	r, err := namespaces.Open(path)
	require.NoError(t, err)
	assert.Empty(t, r.Namespaces())

	require.NoError(t, r.Configured("inst-a", "club"))
	reread, err := namespaces.Open(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"club", "inst-a"}, reread.Namespaces())

	tmps, err := filepath.Glob(path + ".*.tmp")
	require.NoError(t, err)
	assert.Empty(t, tmps, "a save leaves no temporary file behind")
}

// A namespace's drop time is the first time it was found missing, kept across
// restarts; configuring it again clears it.
func TestRecordDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ns.json")
	r, err := namespaces.Open(path)
	require.NoError(t, err)
	require.NoError(t, r.Configured("club"))

	at, err := r.Dropped("club", t0)
	require.NoError(t, err)
	assert.Equal(t, t0, at)
	reread, err := namespaces.Open(path)
	require.NoError(t, err)
	at, err = reread.Dropped("club", t0.Add(time.Hour))
	require.NoError(t, err)
	assert.True(t, at.Equal(t0), "a later start keeps the first drop time")

	require.NoError(t, reread.Configured("club"))
	at, err = reread.Dropped("club", t0.Add(2*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, t0.Add(2*time.Hour), at, "configured again, a later drop starts its own clock")
}

// Forgetting a namespace is saved.
func TestRecordForget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ns.json")
	r, err := namespaces.Open(path)
	require.NoError(t, err)
	require.NoError(t, r.Configured("club", "inst-a"))
	require.NoError(t, r.Forget("club"))
	require.NoError(t, r.Forget("never-recorded"))
	reread, err := namespaces.Open(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"inst-a"}, reread.Namespaces())
}

// A record that cannot be parsed is an error, never an empty record that
// would let a dropped namespace go unpurged.
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
