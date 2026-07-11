package transcript

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemoryLogAppend(t *testing.T) {
	var l MemoryLog
	require.NoError(t, l.Append(context.Background(), Entry{Role: RoleHuman, ChatID: "1", Text: "hi"}))
	require.NoError(t, l.Append(context.Background(), Entry{Role: RoleBot, ChatID: "1", Text: "yo"}))
	got := l.Entries()
	require.Len(t, got, 2)
	assert.Equal(t, RoleHuman, got[0].Role)
	assert.Equal(t, RoleBot, got[1].Role)
}

// A DirLog writes one file per chat id, each holding that chat's lines in order.
func TestDirLogPerChatFiles(t *testing.T) {
	dir := t.TempDir()
	l, err := OpenDir(dir)
	require.NoError(t, err)

	require.NoError(t, l.Append(context.Background(), Entry{Time: time.Unix(1, 0), Role: RoleHuman, ChatID: "-100", Speaker: "alice", Text: "q1"}))
	require.NoError(t, l.Append(context.Background(), Entry{Time: time.Unix(2, 0), Role: RoleBot, ChatID: "-100", Text: "a1"}))
	require.NoError(t, l.Append(context.Background(), Entry{Time: time.Unix(3, 0), Role: RoleHuman, ChatID: "200", Speaker: "bob", Text: "q2"}))
	require.NoError(t, l.Close())

	// Two chats -> two files, named by (sanitized) chat id.
	a := readLines(t, filepath.Join(dir, "-100.jsonl"))
	require.Len(t, a, 2)
	assert.Equal(t, RoleHuman, a[0].Role)
	assert.Equal(t, "alice", a[0].Speaker)
	assert.Equal(t, "q1", a[0].Text)
	assert.Equal(t, RoleBot, a[1].Role)
	assert.Equal(t, "a1", a[1].Text)

	b := readLines(t, filepath.Join(dir, "200.jsonl"))
	require.Len(t, b, 1)
	assert.Equal(t, "bob", b[0].Speaker)
}

// Appends across separate DirLog opens accumulate — the store is append-only, so
// a restart never truncates a chat's history (the prospective-withdrawal posture
// relies on past entries surviving).
func TestDirLogAppendOnlyAcrossOpens(t *testing.T) {
	dir := t.TempDir()
	l1, err := OpenDir(dir)
	require.NoError(t, err)
	require.NoError(t, l1.Append(context.Background(), Entry{Role: RoleHuman, ChatID: "5", Text: "first"}))
	require.NoError(t, l1.Close())

	l2, err := OpenDir(dir)
	require.NoError(t, err)
	require.NoError(t, l2.Append(context.Background(), Entry{Role: RoleHuman, ChatID: "5", Text: "second"}))
	require.NoError(t, l2.Close())

	got := readLines(t, filepath.Join(dir, "5.jsonl"))
	require.Len(t, got, 2)
	assert.Equal(t, "first", got[0].Text)
	assert.Equal(t, "second", got[1].Text)
}

// A hostile chat id cannot escape the directory: separators and dots are
// neutralized, so the write lands on a plain file INSIDE dir and no parent-dir
// file is ever created.
func TestDirLogChatIDTraversalContained(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "transcripts")
	l, err := OpenDir(dir)
	require.NoError(t, err)
	require.NoError(t, l.Append(context.Background(), Entry{Role: RoleHuman, ChatID: "../../etc/passwd", Text: "x"}))
	require.NoError(t, l.Close())

	// Nothing escaped into the parent.
	escaped, _ := filepath.Glob(filepath.Join(root, "*.jsonl"))
	assert.Empty(t, escaped, "no transcript file written outside the configured dir")

	// The one file that exists is inside dir and its name has no path separator.
	inside, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	require.Len(t, inside, 1)
	base := filepath.Base(inside[0])
	assert.NotContains(t, base, "/")
	assert.NotContains(t, base, "..")
}

func TestSafeChatFilename(t *testing.T) {
	cases := map[string]string{
		"-5527987187":      "-5527987187.jsonl", // negative numeric id kept as-is
		"200":              "200.jsonl",
		"../../etc/passwd": "______etc_passwd.jsonl", // separators + dots neutralized
		"a/b":              "a_b.jsonl",
		"":                 "chat.jsonl", // blank id falls back
		"..":               "__.jsonl",   // dots are not a traversal, just underscores
	}
	for in, want := range cases {
		assert.Equal(t, want, safeChatFilename(in), "id %q", in)
	}
}

// OpenDir fails loud when the directory can't be created (a file sits where the
// dir should be), matching the startup-fatal posture (ADR 0015).
func TestOpenDirUnwritableIsFatal(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocker")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), 0o600))
	// A path UNDER a regular file can't be a directory.
	_, err := OpenDir(filepath.Join(blocker, "sub"))
	assert.Error(t, err)
}

func readLines(t *testing.T, path string) []Entry {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var out []Entry
	for _, ln := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if ln == "" {
			continue
		}
		var e Entry
		require.NoError(t, json.Unmarshal([]byte(ln), &e))
		out = append(out, e)
	}
	return out
}
