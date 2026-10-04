package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/inmem"
	"github.com/yaad-index/bonyan/secret"
	"github.com/yaad-index/bonyan/trust"

	"github.com/yaad-index/yaad-grove/internal/namespaces"
	"github.com/yaad-index/yaad-grove/internal/runtime"
)

// memoryCmd is a serve command with long-term memory fully configured.
func memoryCmd(t *testing.T) *ServeCmd {
	return &ServeCmd{
		LongMemoryRecord:    filepath.Join(t.TempDir(), "namespaces.json"),
		LongMemoryURL:       "http://memory.invalid:8000",
		LongMemoryNamespace: "inst-a",
		LongMemoryRetention: 90 * 24 * time.Hour,
		LongMemoryWindow:    24 * time.Hour,
	}
}

// Long-term memory is off unless its URL is set; set only in part, it fails
// startup. The old acknowledgement that withdrawal does not erase is no longer
// needed, and is still accepted, with or without the URL.
func TestBuildLongMemoryConfig(t *testing.T) {
	cases := []struct {
		name    string
		edit    func(c *ServeCmd)
		wantErr string
	}{
		{"configured", func(*ServeCmd) {}, ""},
		{"old acknowledgement still accepted", func(c *ServeCmd) { c.LongMemoryWithoutErase = true }, ""},
		{"no namespace", func(c *ServeCmd) { c.LongMemoryNamespace = "" }, "--long-memory-namespace is required"},
		{"no retention", func(c *ServeCmd) { c.LongMemoryRetention = 0 }, "--long-memory-retention is required"},
		{"no window", func(c *ServeCmd) { c.LongMemoryWindow = 0 }, "--long-memory-window must be at least"},
		{"window too short", func(c *ServeCmd) { c.LongMemoryWindow, c.LongMemoryRetention = 30*time.Minute, time.Hour }, "--long-memory-window must be at least 1h"},
		{"retention not whole windows", func(c *ServeCmd) { c.LongMemoryRetention = 36 * time.Hour }, "whole number of --long-memory-window"},
		{"options without the URL", func(c *ServeCmd) { c.LongMemoryURL = "" }, "without --long-memory-url"},
		{"group namespaces without the URL", func(c *ServeCmd) {
			*c = ServeCmd{LongMemoryWindow: 24 * time.Hour, LongMemoryGroupNamespaces: []string{"-100123=club"}}
		}, "without --long-memory-url"},
		{"no record file", func(c *ServeCmd) { c.LongMemoryRecord = "" }, "--long-memory-record is required"},
		{"bad group namespace", func(c *ServeCmd) { c.LongMemoryGroupNamespaces = []string{"-100123"} }, "want chatid=namespace"},
		{"group namespaces", func(c *ServeCmd) { c.LongMemoryGroupNamespaces = []string{"-100123=club"} }, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := memoryCmd(t)
			tc.edit(c)
			lm, err := buildLongMemory(c, secret.NewResolver(secret.Env{}), time.Now())
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Nil(t, lm)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, lm)
			assert.Equal(t, 24*time.Hour, lm.memory.Window)
		})
	}

	// Nothing set: memory is off and the engine answers as before.
	lm, err := buildLongMemory(&ServeCmd{LongMemoryWindow: 24 * time.Hour}, secret.NewResolver(secret.Env{}), time.Now())
	require.NoError(t, err)
	assert.Nil(t, lm)
	lm, err = buildLongMemory(&ServeCmd{LongMemoryWindow: 24 * time.Hour, LongMemoryWithoutErase: true}, secret.NewResolver(secret.Env{}), time.Now())
	require.NoError(t, err, "the ignored acknowledgement alone turns nothing on and is no error")
	assert.Nil(t, lm)
}

// opener opens namespaces on one in-process storage, counting the opens.
type opener struct {
	storage *inmem.Storage
	opened  map[string]int
}

func newOpener() *opener { return &opener{storage: inmem.NewStorage(), opened: map[string]int{}} }

func (o *opener) open(ns string) (memory.Backend, error) {
	o.opened[ns]++
	return o.storage.Open(ns), nil
}

// newSet is a store set over o, keeping records for an hour.
func newSet(o *opener, scrubber *secret.Scrubber) *storeSet {
	return &storeSet{open: o.open, retention: time.Hour, scrubber: scrubber}
}

// newRecord is an empty record of namespaces in a temporary directory.
func newRecord(t *testing.T) *namespaces.Record {
	t.Helper()
	r, err := namespaces.Open(filepath.Join(t.TempDir(), "namespaces.json"))
	require.NoError(t, err)
	return r
}

// The store scrubs with the scrubber it is given, so a fact written outside a
// run is kept without a resolved secret too.
func TestNewLongMemoryStoreScrubs(t *testing.T) {
	ctx := context.Background()
	t.Setenv(longMemoryTokenEnv, "memory-token-value")
	secrets := secret.NewResolver(secret.Env{})
	_, err := secrets.Scope(longMemoryTokenEnv).Resolve(ctx, longMemoryTokenEnv)
	require.NoError(t, err)
	lm, err := newLongMemory(newSet(newOpener(), secrets.Scrubber()), newRecord(t), "inst-a", nil, time.Hour, time.Now())
	require.NoError(t, err)
	store := lm.memory.Store
	require.NoError(t, store.Remember(ctx, "u1", content.Provenance{Kind: content.KindUser}, "token memory-token-value"))
	facts, err := store.Recall(ctx, "u1", "token", 5)
	require.NoError(t, err)
	require.Len(t, facts, 1)
	u, ok := facts[0].(content.Untrusted)
	require.True(t, ok, "a recalled fact is untrusted")
	assert.Equal(t, "token "+secret.Redacted, u.Raw())
}

// The store refuses a backend opened in another namespace than its own.
func TestNewLongMemoryNamespace(t *testing.T) {
	storage := inmem.NewStorage()
	set := &storeSet{open: func(string) (memory.Backend, error) { return storage.Open("inst-b"), nil }, retention: time.Hour}
	_, err := newLongMemory(set, newRecord(t), "inst-a", nil, time.Hour, time.Now())
	require.Error(t, err, "a backend opened in another namespace is refused")
}

// Each namespace gets one store, opened once: group chats sharing a namespace
// share its store, and a group given the instance's namespace uses the
// instance's store.
func TestNewLongMemoryGroupNamespaces(t *testing.T) {
	o := newOpener()
	lm, err := newLongMemory(newSet(o, nil), newRecord(t), "inst-a", map[string]string{"g1": "club", "g2": "club", "g3": "inst-a", "g4": "other"}, time.Hour, time.Now())
	require.NoError(t, err)
	m := lm.memory
	assert.Equal(t, map[string]int{"inst-a": 1, "club": 1, "other": 1}, o.opened, "each namespace's backend is opened once")
	assert.Same(t, m.Groups["g1"], m.Groups["g2"])
	assert.Same(t, m.Store, m.Groups["g3"])
	assert.NotSame(t, m.Store, m.Groups["g1"])
	assert.NotSame(t, m.Groups["g1"], m.Groups["g4"])
	assert.Len(t, lm.purgers, 3, "a purge covers each namespace once")
}

// Every configured namespace is recorded before anything is kept, and a
// namespace dropped from the configuration is still purged, as a dropped one.
func TestNewLongMemoryRecordsNamespaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "namespaces.json")
	record, err := namespaces.Open(path)
	require.NoError(t, err)
	start := time.Date(2026, 10, 4, 8, 0, 0, 0, time.UTC)
	_, err = newLongMemory(newSet(newOpener(), nil), record, "inst-a", map[string]string{"g1": "club"}, time.Hour, start)
	require.NoError(t, err)
	reread, err := namespaces.Open(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"club", "inst-a"}, reread.Namespaces(), "the configured namespaces are on disk")

	// The group's mapping is removed; the next start still purges its
	// namespace, as dropped at that start.
	restart := start.Add(time.Hour)
	lm, err := newLongMemory(newSet(newOpener(), nil), reread, "inst-a", nil, time.Hour, restart)
	require.NoError(t, err)
	require.Len(t, lm.purgers, 2)
	byName := map[string]runtime.NamespacePurge{}
	for _, p := range lm.purgers {
		np, ok := p.(runtime.NamespacePurge)
		require.True(t, ok)
		require.NotNil(t, np.Backend)
		byName[np.Namespace] = np
	}
	assert.True(t, byName["inst-a"].Dropped.IsZero(), "the configured namespace is not dropped")
	assert.Equal(t, restart, byName["club"].Dropped, "the dropped namespace is purged as dropped")
	assert.Equal(t, time.Hour, byName["club"].Retention)

	// A later start keeps the first drop time.
	again, err := namespaces.Open(path)
	require.NoError(t, err)
	lm, err = newLongMemory(newSet(newOpener(), nil), again, "inst-a", nil, time.Hour, restart.Add(time.Hour))
	require.NoError(t, err)
	for _, p := range lm.purgers {
		if np := p.(runtime.NamespacePurge); np.Namespace == "club" {
			assert.True(t, np.Dropped.Equal(restart))
		}
	}
}

func TestParseGroupNamespaces(t *testing.T) {
	got, err := parseGroupNamespaces([]string{"-100123=club", " -100456 = other "})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"-100123": "club", "-100456": "other"}, got)

	for _, bad := range [][]string{{"-100123"}, {"=club"}, {"-100123="}, {"-100123=club", "-100123=other"}} {
		_, err := parseGroupNamespaces(bad)
		assert.Error(t, err, "%q", bad)
	}
}

// The memory service's token, read from the environment, is what the backend
// authenticates with.
func TestBuildLongMemorySendsTheToken(t *testing.T) {
	t.Setenv(longMemoryTokenEnv, "memory-token-value")
	auth := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case auth <- r.Header.Get("Authorization"):
		default:
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := memoryCmd(t)
	c.LongMemoryURL = srv.URL
	lm, err := buildLongMemory(c, secret.NewResolver(secret.Env{}), time.Now())
	require.NoError(t, err)
	_, err = lm.memory.Store.Recall(context.Background(), "u1", "anything", 5)
	require.Error(t, err, "the fake service refuses")
	assert.Equal(t, "Bearer memory-token-value", <-auth)
}

// Each record is stored under the default policy's name.
func TestNewLongMemoryPolicyName(t *testing.T) {
	ctx := context.Background()
	set := newSet(newOpener(), nil)
	store, err := set.get("inst-a")
	require.NoError(t, err)
	require.NoError(t, store.Append(ctx, "u1", "s1", content.Provenance{Kind: content.KindUser}, "hello"))
	recs, err := set.backends["inst-a"].History(ctx, url.QueryEscape("inst-a")+"/"+url.QueryEscape("u1"), "s1", time.Time{})
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, trust.DefaultName, recs[0].Decision.Policy)
}
