package core_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/bonyan/assemble"
	"github.com/yaad-index/bonyan/content"
	"github.com/yaad-index/bonyan/memory"
	"github.com/yaad-index/bonyan/memory/inmem"
	bmodel "github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/registry"
	"github.com/yaad-index/bonyan/secret"

	"github.com/yaad-index/yaad-grove/internal/core"
)

var (
	memNow    = time.Date(2026, 10, 4, 9, 30, 0, 0, time.UTC)
	memWindow = 24 * time.Hour
	grounded  = mockRetriever{chunks: []core.Chunk{{Source: "faq.md", Text: "The widget installs via the script."}}}
)

// openStore opens namespace ns on storage as bonyan's memory store.
func openStore(t *testing.T, storage *inmem.Storage, ns string, scrubber *secret.Scrubber) *memory.Store {
	t.Helper()
	s, err := memory.NewStore(storage.Open(ns), memory.Options{
		Namespace: ns,
		Policy:    registry.GuardPolicy(nil, nil),
		Retention: 30 * memWindow,
		Scrubber:  scrubber,
		Now:       func() time.Time { return memNow },
	})
	require.NoError(t, err)
	return s
}

func withMemory(s *memory.Store, scrubber *secret.Scrubber) core.Option {
	return core.WithMemory(&core.Memory{Store: s, Window: memWindow, Scrubber: scrubber, Now: func() time.Time { return memNow }})
}

// remembered is a query long-term memory may keep.
func remembered(text string) core.Query {
	return core.Query{Text: text, User: core.User{ID: "u1", Display: "Ada"}, Chat: "chat-1", Remember: true}
}

// raw is t's text, trusted or not.
func raw(t content.Text) string {
	switch v := t.(type) {
	case content.Untrusted:
		return v.Raw()
	case content.Trusted:
		return v.String()
	}
	return ""
}

func texts(ts []content.Text) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = raw(t)
	}
	return out
}

// recalledIn is the memory recalled into the last request.
func recalledIn(m *mockModel) []string {
	return raws(itemsIn(m, assemble.SectionMemory))
}

// session is u1's session in chat-1 in the current window.
var session = core.Session("chat-1", "u1", core.WindowStart(memNow, memWindow))

// A remembered turn and its answer are kept in the user's session for the
// chat and the current window.
func TestMemoryKeepsTheTurnAndItsAnswer(t *testing.T) {
	store := openStore(t, inmem.NewStorage(), "inst-a", nil)
	m := textModel("Run the install script.")
	reply, err := newEngine(m, grounded, nil, "scope", withMemory(store, nil)).Answer(context.Background(), remembered("how do I install it?"))
	require.NoError(t, err)
	require.Equal(t, "Run the install script.", reply.Text)

	got, err := store.History(context.Background(), "u1", session)
	require.NoError(t, err)
	assert.Equal(t, []string{"[Ada] how do I install it?", "Run the install script."}, texts(got))
	var origins []content.Kind
	for _, tx := range got {
		u, ok := tx.(content.Untrusted)
		require.True(t, ok, "nothing read back from memory is trusted")
		origins = append(origins, u.Provenance().Origin)
	}
	assert.Equal(t, []content.Kind{content.KindUser, content.KindModel}, origins, "the answer is kept as model output")
}

// failFirst is a backend whose first write fails.
type failFirst struct {
	memory.Backend
	writes int
}

func (b *failFirst) Write(ctx context.Context, r memory.Record) (string, error) {
	b.writes++
	if b.writes == 1 {
		return "", errors.New("service down")
	}
	return b.Backend.Write(ctx, r)
}

// When the question cannot be kept, the answer is not kept without it.
func TestMemoryKeepsNoAnswerWithoutItsQuestion(t *testing.T) {
	ctx := context.Background()
	backend := &failFirst{Backend: inmem.NewStorage().Open("inst-a")}
	store, err := memory.NewStore(backend, memory.Options{Namespace: "inst-a", Retention: 30 * memWindow, Now: func() time.Time { return memNow }})
	require.NoError(t, err)
	reply, err := newEngine(textModel("answer"), grounded, nil, "scope", withMemory(store, nil)).Answer(ctx, remembered("question"))
	require.NoError(t, err)
	assert.Equal(t, "answer", reply.Text, "a memory failure never costs the user the answer")
	got, err := store.History(ctx, "u1", session)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, 1, backend.writes)
}

// A query the runtime did not mark, or one missing its user or chat, keeps
// nothing and recalls nothing.
func TestMemoryOnlyForMarkedQueries(t *testing.T) {
	ctx := context.Background()
	for name, q := range map[string]core.Query{
		"not marked": {Text: "how do I install it?", User: core.User{ID: "u1"}, Chat: "chat-1"},
		"no chat":    {Text: "how do I install it?", User: core.User{ID: "u1"}, Remember: true},
		"no user":    {Text: "how do I install it?", Chat: "chat-1", Remember: true},
	} {
		t.Run(name, func(t *testing.T) {
			store := openStore(t, inmem.NewStorage(), "inst-a", nil)
			require.NoError(t, store.Remember(ctx, "u1", content.Provenance{Kind: content.KindUser}, "prefers the install script"))
			m := textModel("answer")
			_, err := newEngine(m, grounded, nil, "scope", withMemory(store, nil)).Answer(ctx, q)
			require.NoError(t, err)
			got, err := store.History(ctx, "u1", session)
			require.NoError(t, err)
			assert.Empty(t, got, "nothing is kept")
			assert.Empty(t, recalledIn(m), "nothing is recalled")
		})
	}
}

// What is known about the asker is recalled into the request, untrusted and
// inside its own section, never what is known about someone else.
func TestMemoryRecallsTheAskersFacts(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, inmem.NewStorage(), "inst-a", nil)
	require.NoError(t, store.Remember(ctx, "u1", content.Provenance{Kind: content.KindUser}, "Ada prefers the install script"))
	require.NoError(t, store.Remember(ctx, "u2", content.Provenance{Kind: content.KindUser}, "Bob prefers the install wizard"))

	m := textModel("answer")
	_, err := newEngine(m, grounded, nil, "scope", withMemory(store, nil)).Answer(ctx, remembered("which install do I prefer?"))
	require.NoError(t, err)
	assert.Equal(t, []string{"Ada prefers the install script"}, recalledIn(m), "only the asker's memory is recalled")
	assert.NotContains(t, systemOf(m), "Ada prefers", "recalled memory never sits in the trusted instructions")
}

// Two instances sharing one memory service each see only their own namespace:
// one never recalls or reads the other's records, and deleting a user in one
// leaves the other's untouched.
func TestMemoryNamespacesAreIsolated(t *testing.T) {
	ctx := context.Background()
	storage := inmem.NewStorage()
	a := openStore(t, storage, "inst-a", nil)
	b := openStore(t, storage, "inst-b", nil)

	_, err := newEngine(textModel("answer A"), grounded, nil, "scope", withMemory(a, nil)).Answer(ctx, remembered("remember the blue widget"))
	require.NoError(t, err)
	require.NoError(t, a.Remember(ctx, "u1", content.Provenance{Kind: content.KindUser}, "likes the blue widget"))

	// The same user in the same chat, through the other instance.
	m := textModel("answer B")
	_, err = newEngine(m, grounded, nil, "scope", withMemory(b, nil)).Answer(ctx, remembered("what widget do I like?"))
	require.NoError(t, err)
	assert.Empty(t, recalledIn(m), "the other instance's memory is never recalled")
	got, err := b.History(ctx, "u1", session)
	require.NoError(t, err)
	assert.Equal(t, []string{"[Ada] what widget do I like?", "answer B"}, texts(got), "each instance holds only its own turns")

	require.NoError(t, b.DeleteSubject(ctx, "u1"))
	got, err = a.History(ctx, "u1", session)
	require.NoError(t, err)
	assert.Equal(t, []string{"[Ada] remember the blue widget", "answer A"}, texts(got), "deleting in one instance leaves the other's records")
	facts, err := a.Recall(ctx, "u1", "blue widget", 5)
	require.NoError(t, err)
	assert.Equal(t, []string{"likes the blue widget"}, texts(facts))
}

// A resolved secret never reaches memory: the run scrubs the turn and its
// answer before they leave it, even over a store that scrubs nothing itself.
func TestMemoryKeepsNoResolvedSecret(t *testing.T) {
	ctx := context.Background()
	secrets := secret.NewResolver(mapSource{"KEY": "sk-live-123"})
	_, err := secrets.Scope("KEY").Resolve(ctx, "KEY")
	require.NoError(t, err)
	store := openStore(t, inmem.NewStorage(), "inst-a", nil)

	_, err = newEngine(textModel("noted sk-live-123"), grounded, nil, "scope", withMemory(store, secrets.Scrubber())).Answer(ctx, remembered("my key is sk-live-123"))
	require.NoError(t, err)
	got, err := store.History(ctx, "u1", session)
	require.NoError(t, err)
	require.Len(t, got, 2)
	for _, s := range texts(got) {
		assert.NotContains(t, s, "sk-live-123")
		assert.Contains(t, s, secret.Redacted)
	}
}

// A new window starts a new session, so a session can be deleted whole once
// retention has passed it.
func TestMemorySessionPerWindow(t *testing.T) {
	start := core.WindowStart(memNow, memWindow)
	assert.Equal(t, time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), start)
	assert.Equal(t, start, core.WindowStart(start, memWindow), "a boundary starts its own window")
	assert.Equal(t, start, core.WindowStart(start.Add(memWindow-time.Nanosecond), memWindow))
	assert.Equal(t, start.Add(memWindow), core.WindowStart(start.Add(memWindow), memWindow))

	assert.Equal(t, "chat-1/u1/1791072000", core.Session("chat-1", "u1", start))
	assert.NotEqual(t, core.Session("chat-1", "u1", start), core.Session("chat-1", "u1", start.Add(memWindow)))
	assert.NotEqual(t, core.Session("chat-1", "u1", start), core.Session("chat-2", "u1", start))
}

// mapSource is a secret source over a map.
type mapSource map[string]string

func (s mapSource) Lookup(_ context.Context, name string) (string, error) {
	v, ok := s[name]
	if !ok {
		return "", secret.ErrNotFound
	}
	return v, nil
}

// A group given a namespace of its own keeps its turns there and recalls only
// what that namespace holds; any other group uses the instance's namespace. A
// user in both has two separate memories.
func TestMemoryGroupNamespaces(t *testing.T) {
	ctx := context.Background()
	storage := inmem.NewStorage()
	inst := openStore(t, storage, "inst-a", nil)
	club := openStore(t, storage, "club", nil)
	mem := core.WithMemory(&core.Memory{Store: inst, Groups: map[string]*memory.Store{"chat-g": club}, Window: memWindow, Now: func() time.Time { return memNow }})
	require.NoError(t, inst.Remember(ctx, "u1", content.Provenance{Kind: content.KindUser}, "likes the red widget"))
	require.NoError(t, club.Remember(ctx, "u1", content.Provenance{Kind: content.KindUser}, "likes the green widget"))

	inGroup := remembered("which widget do I like?")
	inGroup.Chat = "chat-g"
	m := textModel("green")
	_, err := newEngine(m, grounded, nil, "scope", mem).Answer(ctx, inGroup)
	require.NoError(t, err)
	assert.Equal(t, []string{"likes the green widget"}, recalledIn(m), "the group recalls only its own namespace")
	groupSession := core.Session("chat-g", "u1", core.WindowStart(memNow, memWindow))
	got, err := club.History(ctx, "u1", groupSession)
	require.NoError(t, err)
	assert.Equal(t, []string{"[Ada] which widget do I like?", "green"}, texts(got))
	got, err = inst.History(ctx, "u1", groupSession)
	require.NoError(t, err)
	assert.Empty(t, got, "the group's turns stay out of the instance's namespace")

	m = textModel("red")
	_, err = newEngine(m, grounded, nil, "scope", mem).Answer(ctx, remembered("which widget do I like?"))
	require.NoError(t, err)
	assert.Equal(t, []string{"likes the red widget"}, recalledIn(m), "another group uses the instance's namespace")
	got, err = inst.History(ctx, "u1", session)
	require.NoError(t, err)
	assert.Equal(t, []string{"[Ada] which widget do I like?", "red"}, texts(got))
}

// A refused turn is not kept: neither the question nor the decline.
func TestMemoryKeepsNoRefusal(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, inmem.NewStorage(), "inst-a", nil)
	reply, err := newEngine(textModel(core.RefusalToken+" I can help with the widget."), grounded, nil, "scope", withMemory(store, nil)).Answer(ctx, remembered("what's the weather?"))
	require.NoError(t, err)
	require.True(t, reply.Refused)
	got, err := store.History(ctx, "u1", session)
	require.NoError(t, err)
	assert.Empty(t, got, "a refused turn is not kept")
}

// A question the vault cannot ground is refused even when memory holds its
// answer: memory personalises and never grounds (ADR 0023 §4). Nothing is
// asked of the model and nothing is kept.
func TestMemoryNeverGrounds(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, inmem.NewStorage(), "inst-a", nil)
	require.NoError(t, store.Remember(ctx, "u1", content.Provenance{Kind: content.KindUser}, "the widget's launch date is 12 May"))
	m := textModel("12 May")
	reply, err := newEngine(m, mockRetriever{}, nil, "scope", withMemory(store, nil)).Answer(ctx, remembered("when is the widget's launch date?"))
	require.NoError(t, err)
	assert.True(t, reply.Refused)
	assert.Zero(t, m.calls, "memory does not stand in for retrieval")
	got, err := store.History(ctx, "u1", session)
	require.NoError(t, err)
	assert.Empty(t, got)
}

// hookModel answers like m, running hook on every call first.
type hookModel struct {
	*mockModel
	hook func()
}

func (h hookModel) Chat(ctx context.Context, req bmodel.ChatRequest) (bmodel.ChatResponse, error) {
	if h.hook != nil {
		h.hook()
	}
	return h.mockModel.Chat(ctx, req)
}

// A user who withdraws while their turn is being answered has that turn kept
// nowhere; a turn they start after withdrawing, having consented again, is
// kept as usual. Another user's turn is unaffected.
func TestWithdrawDuringAnAnswer(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, inmem.NewStorage(), "inst-a", nil)
	mem := &core.Memory{Store: store, Window: memWindow, Now: func() time.Time { return memNow }}
	withdrawing := hookModel{mockModel: textModel("answer"), hook: func() { mem.Withdraw("u1") }}
	_, err := core.New(withdrawing, modelName, grounded, nil, "scope", core.WithMemory(mem)).Answer(ctx, remembered("started before"))
	require.NoError(t, err)
	got, err := store.History(ctx, "u1", session)
	require.NoError(t, err)
	assert.Empty(t, got, "a turn under way when its user withdrew is not kept")

	engine := newEngine(textModel("answer"), grounded, nil, "scope", core.WithMemory(mem))
	_, err = engine.Answer(ctx, remembered("started after"))
	require.NoError(t, err)
	got, err = store.History(ctx, "u1", session)
	require.NoError(t, err)
	assert.Equal(t, []string{"[Ada] started after", "answer"}, texts(got), "a turn started after the withdrawal is kept")

	other := core.Query{Text: "mine", User: core.User{ID: "u2"}, Chat: "chat-1", Remember: true}
	_, err = core.New(hookModel{mockModel: textModel("yours"), hook: func() { mem.Withdraw("u1") }}, modelName, grounded, nil, "scope", core.WithMemory(mem)).Answer(ctx, other)
	require.NoError(t, err)
	got, err = store.History(ctx, "u2", core.Session("chat-1", "u2", core.WindowStart(memNow, memWindow)))
	require.NoError(t, err)
	assert.Equal(t, []string{"mine", "yours"}, texts(got), "another user's withdrawal does not touch this turn")
}

// blockingWrites is a backend whose writes wait for release.
type blockingWrites struct {
	memory.Backend
	started chan struct{}
	release chan struct{}
}

func (b *blockingWrites) Write(ctx context.Context, r memory.Record) (string, error) {
	select {
	case b.started <- struct{}{}:
	default:
	}
	<-b.release
	return b.Backend.Write(ctx, r)
}

// Withdraw returns only once a keep already writing has finished, so an erase
// that follows it deletes what that keep wrote.
func TestWithdrawWaitsForAKeepUnderWay(t *testing.T) {
	ctx := context.Background()
	backend := &blockingWrites{Backend: inmem.NewStorage().Open("inst-a"), started: make(chan struct{}, 1), release: make(chan struct{})}
	store, err := memory.NewStore(backend, memory.Options{Namespace: "inst-a", Policy: registry.GuardPolicy(nil, nil), Retention: 30 * memWindow, Now: func() time.Time { return memNow }})
	require.NoError(t, err)
	mem := &core.Memory{Store: store, Window: memWindow, Now: func() time.Time { return memNow }}
	answered := make(chan struct{})
	go func() {
		_, _ = newEngine(textModel("answer"), grounded, nil, "scope", core.WithMemory(mem)).Answer(ctx, remembered("q"))
		close(answered)
	}()
	<-backend.started

	withdrawn := make(chan struct{})
	go func() {
		mem.Withdraw("u1")
		close(withdrawn)
	}()
	select {
	case <-withdrawn:
		t.Fatal("Withdraw returned while a keep was still writing")
	case <-time.After(50 * time.Millisecond):
	}
	close(backend.release)
	<-answered
	<-withdrawn
	got, err := store.History(ctx, "u1", session)
	require.NoError(t, err)
	assert.Len(t, got, 2, "the keep that was writing finished before Withdraw returned")
}

// Withdraw on no memory does nothing.
func TestWithdrawWithoutMemory(t *testing.T) {
	var mem *core.Memory
	assert.NotPanics(t, func() { mem.Withdraw("u1") })
}
