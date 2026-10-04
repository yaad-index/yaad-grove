package runtime_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/inmem"
	bmodel "github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/registry"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/namespaces"
	"github.com/yaad-index/yaad-grove/internal/runtime"
)

// fakeDeleter records the subjects it deletes, failing with err.
type fakeDeleter struct {
	deleted []string
	err     error
}

func (f *fakeDeleter) DeleteSubject(_ context.Context, subject string) error {
	f.deleted = append(f.deleted, subject)
	return f.err
}

func openRecord(t *testing.T, configured ...string) *namespaces.Record {
	t.Helper()
	r, err := namespaces.Open(filepath.Join(t.TempDir(), "ns.json"))
	require.NoError(t, err)
	require.NoError(t, r.Configured(configured...))
	return r
}

// Erasing tries every recorded namespace, a dropped one included, even after
// one fails, and reports each; a recorded namespace with no store is a failure,
// never skipped.
func TestEraserErasesEveryRecordedNamespace(t *testing.T) {
	record := openRecord(t, "inst-a", "club", "old", "lost")
	_, err := record.Dropped("old", time.Now())
	require.NoError(t, err)
	failing := errors.New("service down")
	inst, club, old := &fakeDeleter{}, &fakeDeleter{err: failing}, &fakeDeleter{}
	e := &runtime.Eraser{Record: record, Stores: map[string]runtime.SubjectDeleter{"inst-a": inst, "club": club, "old": old}}

	results := e.Erase(context.Background(), "u1")
	require.Len(t, results, 4)
	assert.Equal(t, "club", results[0].Namespace)
	require.ErrorIs(t, results[0].Err, failing)
	assert.Equal(t, runtime.EraseResult{Namespace: "inst-a"}, results[1])
	assert.Equal(t, "lost", results[2].Namespace)
	require.Error(t, results[2].Err, "a recorded namespace with no store is not erased, and says so")
	assert.Equal(t, runtime.EraseResult{Namespace: "old"}, results[3], "a dropped namespace is erased too")
	for _, d := range []*fakeDeleter{inst, club, old} {
		assert.Equal(t, []string{"u1"}, d.deleted)
	}
	assert.False(t, runtime.Erased(results))
	assert.True(t, runtime.Erased(results[1:2]))
	assert.True(t, runtime.Erased(nil))
}

// blockingChat answers once proceed is closed, telling inModel it was called.
type blockingChat struct {
	inModel chan struct{}
	proceed chan struct{}
}

func (b blockingChat) Chat(context.Context, bmodel.ChatRequest) (bmodel.ChatResponse, error) {
	close(b.inModel)
	<-b.proceed
	return bmodel.ChatResponse{Content: "answer", StopReason: bmodel.StopEnd, Usage: &bmodel.Usage{InputTokens: 1, OutputTokens: 1}}, nil
}

// deleteThen deletes through store, then runs after.
type deleteThen struct {
	store *memory.Store
	after func()
}

func (d deleteThen) DeleteSubject(ctx context.Context, subject string) error {
	err := d.store.DeleteSubject(ctx, subject)
	d.after()
	return err
}

// An answer under way when its user withdraws keeps nothing, even when it
// finishes only after the erase: the eraser stops the engine keeping it before
// it deletes anything.
func TestEraserStopsTheTurnUnderWayBeforeDeleting(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	store, err := memory.NewStore(inmem.NewStorage().Open("inst-a"), memory.Options{
		Namespace: "inst-a", Policy: registry.GuardPolicy(nil, nil), Retention: 24 * time.Hour, Now: func() time.Time { return now },
	})
	require.NoError(t, err)
	mem := &core.Memory{Store: store, Window: time.Hour, Now: func() time.Time { return now }}
	chat := blockingChat{inModel: make(chan struct{}), proceed: make(chan struct{})}
	engine := core.New(chat, "m", oneChunk{}, nil, "scope", core.WithMemory(mem))
	answered := make(chan struct{})
	go func() {
		_, _ = engine.Answer(ctx, core.Query{Text: "q", User: core.User{ID: "u1"}, Chat: "chat-1", Remember: true})
		close(answered)
	}()
	<-chat.inModel

	// The answer finishes only after the namespace is erased.
	e := &runtime.Eraser{Memory: mem, Record: openRecord(t, "inst-a"), Stores: map[string]runtime.SubjectDeleter{
		"inst-a": deleteThen{store: store, after: func() { close(chat.proceed); <-answered }},
	}}
	require.True(t, runtime.Erased(e.Erase(ctx, "u1")))

	got, err := store.History(ctx, "u1", core.Session("chat-1", "u1", core.WindowStart(now, time.Hour)))
	require.NoError(t, err)
	assert.Empty(t, got, "the turn under way kept nothing after the erase")
}
