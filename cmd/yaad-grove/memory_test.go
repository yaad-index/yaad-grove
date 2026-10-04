package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/bonyan/content"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/namespaces"
	"github.com/yaad-index/yaad-grove/internal/runtime"
)

// The eraser covers every recorded namespace, the configured ones and one
// dropped from the configuration, and erases the user in each.
func TestNewLongMemoryErasesEveryRecordedNamespace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "namespaces.json")
	record, err := namespaces.Open(path)
	require.NoError(t, err)
	o := newOpener()
	start := time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)
	lm, err := newLongMemory(newSet(o, nil), record, "inst-a", map[string]string{"g1": "club"}, time.Hour, start)
	require.NoError(t, err)
	club := lm.memory.Groups["g1"]
	require.NoError(t, club.Append(ctx, "u1", "s", content.Provenance{Kind: content.KindUser}, "in the club"))
	require.NoError(t, lm.memory.Store.Append(ctx, "u1", "s", content.Provenance{Kind: content.KindUser}, "in the instance"))

	// The club's mapping is removed: its namespace is dropped, and still erased.
	reread, err := namespaces.Open(path)
	require.NoError(t, err)
	set := &storeSet{open: o.open, retention: time.Hour}
	lm, err = newLongMemory(set, reread, "inst-a", nil, time.Hour, start.Add(time.Hour))
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"club", "inst-a"}, keys(lm.eraser.Stores))
	assert.Same(t, lm.memory, lm.eraser.Memory)

	results := lm.eraser.Erase(ctx, "u1")
	assert.Equal(t, []runtime.EraseResult{{Namespace: "club"}, {Namespace: "inst-a"}}, results)
	for _, ns := range []string{"club", "inst-a"} {
		st, err := set.get(ns)
		require.NoError(t, err)
		got, err := st.History(ctx, "u1", "s")
		require.NoError(t, err)
		assert.Empty(t, got, "u1 erased in %s", ns)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// fakeEraser answers each user with set results.
type fakeEraser map[string][]runtime.EraseResult

func (f fakeEraser) Erase(_ context.Context, user string) []runtime.EraseResult { return f[user] }
func (f fakeEraser) Withdrawals(string) uint64                                  { return 0 }

// The catch-up prints one line per user and namespace and a total, and fails
// when any erase failed.
func TestEraseUsers(t *testing.T) {
	var out bytes.Buffer
	ok := fakeEraser{"u1": {{Namespace: "club"}, {Namespace: "inst-a"}}, "u2": {{Namespace: "club"}, {Namespace: "inst-a"}}}
	require.NoError(t, eraseUsers(context.Background(), ok, []string{"u1", "u2"}, &out))
	assert.Equal(t, "u1\tclub\terased\nu1\tinst-a\terased\nu2\tclub\terased\nu2\tinst-a\terased\n2 users, 0 failed erases\n", out.String())

	out.Reset()
	bad := fakeEraser{"u1": {{Namespace: "club", Err: errors.New("down")}, {Namespace: "inst-a"}}}
	err := eraseUsers(context.Background(), bad, []string{"u1"}, &out)
	require.ErrorContains(t, err, "1 erases failed")
	assert.Equal(t, "u1\tclub\tfailed: down\nu1\tinst-a\terased\n1 users, 1 failed erases\n", out.String())
}

// memory erase needs exactly one source of users and long-term memory
// configured, and --unconsented refuses while the bot holds the store open.
func TestMemoryEraseCmdChecks(t *testing.T) {
	cmd := func(t *testing.T) *MemoryEraseCmd { return &MemoryEraseCmd{ServeCmd: *memoryCmd(t)} }

	c := cmd(t)
	require.ErrorContains(t, c.Run(discard), "one of them")
	c.Users, c.Unconsented = []string{"u1"}, true
	require.ErrorContains(t, c.Run(discard), "one of them")

	c = &MemoryEraseCmd{ServeCmd: ServeCmd{LongMemoryWindow: time.Hour}, Users: []string{"u1"}}
	require.ErrorContains(t, c.Run(discard), "not configured")

	c = cmd(t)
	c.Unconsented = true
	c.ACLDB = filepath.Join(t.TempDir(), "acl.db")
	held, err := acl.OpenBolt(c.ACLDB)
	require.NoError(t, err)
	defer func() { _ = held.Close() }()
	require.ErrorContains(t, c.Run(discard), "stop the bot first")
}

// `memory erase` reads serve's section of the configuration file.
func TestConfigLoaderMemoryEraseReadsServe(t *testing.T) {
	var cli CLI
	parser, err := kong.New(&cli, kong.Configuration(configLoader, writeFile(t, "config.yaml", "serve:\n  long-memory-namespace: inst-a\n  acl-db: /srv/acl.db\n")))
	require.NoError(t, err)
	_, err = parser.Parse([]string{"memory", "erase", "--unconsented"})
	require.NoError(t, err)
	assert.Equal(t, "inst-a", cli.Memory.Erase.LongMemoryNamespace)
	assert.Equal(t, "/srv/acl.db", cli.Memory.Erase.ACLDB)
}

// Serve's policy erases through the long-term memory's eraser and discloses
// derivation as configured; with no long-term memory it does neither.
func TestLongMemoryWithdrawalPolicy(t *testing.T) {
	lm, err := newLongMemory(newSet(newOpener(), nil), newRecord(t), "inst-a", nil, time.Hour, time.Now())
	require.NoError(t, err)
	var p runtime.Policy
	lm.withdrawal(&p, true)
	assert.Same(t, lm.eraser, p.Erase)
	assert.True(t, p.MemoryDerive)

	var none *longMemory
	p = runtime.Policy{}
	none.withdrawal(&p, true)
	assert.Nil(t, p.Erase, "no memory, nothing to erase: and no typed nil either")
	assert.False(t, p.MemoryDerive)
}
