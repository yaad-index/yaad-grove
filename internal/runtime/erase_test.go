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

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/core"
	memorybuf "github.com/yaad-index/yaad-grove/internal/memory"
	"github.com/yaad-index/yaad-grove/internal/namespaces"
	"github.com/yaad-index/yaad-grove/internal/runtime"
	"github.com/yaad-index/yaad-grove/internal/transport"
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

// consentGate serves a consented directed group message and nudges anyone
// else, reading consent as the real gate does; after reading it runs
// afterRead, once.
type consentGate struct {
	consent   *mockConsenter
	afterRead *func()
}

func (g consentGate) Check(ctx context.Context, _ acl.GateInput) (acl.Decision, error) {
	c, err := g.consent.ConsentOf(ctx, "")
	if hook := *g.afterRead; hook != nil {
		*g.afterRead = nil
		hook()
	}
	if c != acl.ConsentGranted {
		return acl.DecideNudge, err
	}
	return acl.DecideServe, err
}

// hookRetriever runs hook, once, when the engine retrieves: after the gate
// admitted the turn, before the engine takes it up for memory.
type hookRetriever struct{ hook func() }

func (h *hookRetriever) Retrieve(context.Context, string) ([]core.Chunk, error) {
	if h.hook != nil {
		hook := h.hook
		h.hook = nil
		hook()
	}
	return []core.Chunk{{Source: "a.md", Text: "x"}}, nil
}

// fixedChat always answers the same.
type fixedChat struct{}

func (fixedChat) Chat(context.Context, bmodel.ChatRequest) (bmodel.ChatResponse, error) {
	return bmodel.ChatResponse{Content: "answer", StopReason: bmodel.StopEnd, Usage: &bmodel.Usage{InputTokens: 1, OutputTokens: 1}}, nil
}

// Through the handler, with the real engine and eraser: a served group turn is
// kept under the sender's ID, the same ID the sender's /consent remove erases;
// and a withdrawal that lands after the gate admitted a turn, before the
// engine answers it, keeps that turn out of long-term memory and out of the
// conversation buffer.
func TestHandlerWithdrawalErasesTheSameSubject(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	store, err := memory.NewStore(inmem.NewStorage().Open("inst-a"), memory.Options{
		Namespace: "inst-a", Policy: registry.GuardPolicy(nil, nil), Retention: 24 * time.Hour, Now: func() time.Time { return now },
	})
	require.NoError(t, err)
	mem := &core.Memory{Store: store, Window: time.Hour, Now: func() time.Time { return now }}
	retriever := &hookRetriever{}
	engine := core.New(fixedChat{}, "m", retriever, nil, "scope", core.WithMemory(mem))
	consent := &mockConsenter{consent: acl.ConsentGranted}
	buf := memorybuf.New(10)
	policy := runtime.Policy{Memory: buf, Erase: &runtime.Eraser{Memory: mem, Record: openRecord(t, "inst-a"), Stores: map[string]runtime.SubjectDeleter{"inst-a": store}}}
	var afterGateRead func()
	h := runtime.NewHandler(consentGate{consent: consent, afterRead: &afterGateRead}, engine, nil, nil, nil, nil, consent, policy)
	group := transport.Inbound{User: core.User{ID: "u1", Display: "Ada"}, Surface: core.SurfaceGroup, Text: "q", ReplyTo: "chat-1", Directed: true}
	session := core.Session("chat-1", "u1", core.WindowStart(now, time.Hour))
	history := func() []string {
		got, err := store.History(ctx, "u1", session)
		require.NoError(t, err)
		out := make([]string, len(got))
		for i, g := range got {
			out[i] = g.(interface{ Raw() string }).Raw()
		}
		return out
	}

	_, err = h(ctx, group)
	require.NoError(t, err)
	assert.Equal(t, []string{"[Ada] q", "answer"}, history(), "kept under the sender's ID")
	reply, err := h(ctx, dmInbound("/consent remove"))
	require.NoError(t, err)
	assert.Contains(t, reply.Text, "long-term memory is erased")
	assert.Empty(t, history(), "the withdrawal erased the same subject the turn was kept under")

	// Consent again; a turn is admitted, and the user withdraws before the
	// engine takes it up.
	consent.consent = acl.ConsentGranted
	retriever.hook = func() {
		_, err := h(ctx, dmInbound("/consent remove"))
		require.NoError(t, err)
	}
	_, err = h(ctx, group)
	require.NoError(t, err)
	assert.Empty(t, history(), "a turn admitted before the withdrawal is not kept after it")
	for _, turn := range buf.Recent("chat-1", 10) {
		assert.NotEqual(t, "u1", turn.SpeakerID, "nor are the user's turns left in the conversation buffer")
	}

	// The same when the withdrawal lands while the gate decides, after it read
	// consent: the count was read before the gate.
	consent.consent = acl.ConsentGranted
	afterGateRead = func() {
		_, err := h(ctx, dmInbound("/consent remove"))
		require.NoError(t, err)
	}
	_, err = h(ctx, group)
	require.NoError(t, err)
	assert.Empty(t, history(), "a withdrawal during the gate keeps the turn out")
	for _, turn := range buf.Recent("chat-1", 10) {
		assert.NotEqual(t, "u1", turn.SpeakerID, "the turn buffered after the withdrawal's purge is purged too")
	}

	// Consented again, with no withdrawal: the turn is kept.
	consent.consent = acl.ConsentGranted
	_, err = h(ctx, group)
	require.NoError(t, err)
	assert.Equal(t, []string{"[Ada] q", "answer"}, history(), "a turn after consenting again is kept")
}

// A consented turn buffered as context, logged-only or rate-limited, is purged
// again when its user withdrew after the gate admitted it.
func TestHandlerBufferedTurnOfAWithdrawnUser(t *testing.T) {
	for _, decision := range []acl.Decision{acl.DecideLogOnly, acl.DecideRateLimited} {
		buf := memorybuf.New(10)
		withdrawn := &mockConsenter{consent: acl.ConsentUnknown} // withdrew after the gate read it
		h := runtime.NewHandler(&mockGate{decision: decision}, &mockEngine{}, nil, nil, nil, nil, withdrawn, runtime.Policy{Memory: buf})
		_, err := h(context.Background(), transport.Inbound{User: core.User{ID: "u1"}, Surface: core.SurfaceGroup, Text: "chatter", ReplyTo: "chat-1"})
		require.NoError(t, err)
		assert.Empty(t, buf.Recent("chat-1", 10), "decision %d", decision)

		still := &mockConsenter{consent: acl.ConsentGranted}
		h = runtime.NewHandler(&mockGate{decision: decision}, &mockEngine{}, nil, nil, nil, nil, still, runtime.Policy{Memory: buf})
		_, err = h(context.Background(), transport.Inbound{User: core.User{ID: "u1"}, Surface: core.SurfaceGroup, Text: "chatter", ReplyTo: "chat-1"})
		require.NoError(t, err)
		assert.Len(t, buf.Recent("chat-1", 10), 1, "a consented user's turn stays")
	}
}
