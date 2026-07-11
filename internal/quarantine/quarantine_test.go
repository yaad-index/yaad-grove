package quarantine_test

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/quarantine"
)

func TestMemoryLog(t *testing.T) {
	l := &quarantine.MemoryLog{}
	require.NoError(t, l.Append(context.Background(), quarantine.Entry{UserID: "u1", Surface: "dm", Text: "hi"}))
	require.NoError(t, l.Append(context.Background(), quarantine.Entry{UserID: "u2", Surface: "group", Text: "yo"}))

	got := l.Entries()
	require.Len(t, got, 2)
	assert.Equal(t, "u1", got[0].UserID)
	assert.Equal(t, "yo", got[1].Text)
}

// FileLog writes one JSON object per line, appended in order, re-readable.
func TestFileLogAppendsJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.jsonl")
	l, err := quarantine.OpenFile(path)
	require.NoError(t, err)

	require.NoError(t, l.Append(context.Background(), quarantine.Entry{Time: time.Unix(1, 0).UTC(), UserID: "u1", Surface: "dm", Text: "first"}))
	require.NoError(t, l.Append(context.Background(), quarantine.Entry{Time: time.Unix(2, 0).UTC(), UserID: "u2", Surface: "group", Text: "second"}))
	require.NoError(t, l.Close())

	entries := readLog(t, path)
	require.Len(t, entries, 2)
	assert.Equal(t, "first", entries[0].Text)
	assert.Equal(t, "u2", entries[1].UserID)
	assert.Equal(t, "group", entries[1].Surface)
}

// Appends survive across reopens (append-only, not truncating).
func TestFileLogAppendsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.jsonl")
	l1, err := quarantine.OpenFile(path)
	require.NoError(t, err)
	require.NoError(t, l1.Append(context.Background(), quarantine.Entry{Text: "one"}))
	require.NoError(t, l1.Close())

	l2, err := quarantine.OpenFile(path)
	require.NoError(t, err)
	require.NoError(t, l2.Append(context.Background(), quarantine.Entry{Text: "two"}))
	require.NoError(t, l2.Close())

	entries := readLog(t, path)
	require.Len(t, entries, 2, "reopen appends rather than truncates")
}

// Concurrent appends never interleave — each line stays a valid, whole JSON
// object (run under -race).
func TestFileLogConcurrentAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.jsonl")
	l, err := quarantine.OpenFile(path)
	require.NoError(t, err)

	const n = 50
	long := strings.Repeat("x", 4096) // exceed a small buffer to stress interleaving
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = l.Append(context.Background(), quarantine.Entry{UserID: "u", Text: long})
		}()
	}
	wg.Wait()
	require.NoError(t, l.Close())

	entries := readLog(t, path)
	assert.Len(t, entries, n, "every append is one intact line, none corrupted or lost")
	for _, e := range entries {
		assert.Equal(t, long, e.Text, "no line interleaved")
	}
}

func readLog(t *testing.T, path string) []quarantine.Entry {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var out []quarantine.Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var e quarantine.Entry
		require.NoError(t, json.Unmarshal(sc.Bytes(), &e), "each line is a valid JSON object")
		out = append(out, e)
	}
	require.NoError(t, sc.Err())
	return out
}
