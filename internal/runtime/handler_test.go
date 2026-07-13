package runtime_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/pending"
	"github.com/yaad-index/yaad-grove/internal/quarantine"
	"github.com/yaad-index/yaad-grove/internal/runtime"
	"github.com/yaad-index/yaad-grove/internal/transcript"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

// transcriptMsg is a directed group message with the id/threading fields the
// transcript records.
var transcriptMsg = transport.Inbound{
	User: core.User{ID: "u1", Display: "alice"}, Surface: core.SurfaceGroup,
	Text: "hello", ReplyTo: "chat-1", Directed: true, MessageID: "m10", ReplyToMessageID: "m9",
}

// transcriptPolicy wires a Policy with only the transcript log set.
func transcriptPolicy(t *transcript.MemoryLog) runtime.Policy {
	return runtime.Policy{Transcript: t}
}

// A served message records the human turn then the bot's answer (ADR 0015), with
// the human turn carrying speaker + threading and the bot turn the reply text.
func TestHandlerTranscriptServedLogsHumanThenBot(t *testing.T) {
	tlog := &transcript.MemoryLog{}
	gate := &mockGate{decision: acl.DecideServe}
	engine := &mockEngine{reply: core.Reply{Text: "the answer"}}
	h := runtime.NewHandler(gate, engine, nil, nil, nil, nil, nil, transcriptPolicy(tlog))

	_, err := h(context.Background(), transcriptMsg)
	require.NoError(t, err)

	e := tlog.Entries()
	require.Len(t, e, 2, "one human turn, one bot turn")
	assert.Equal(t, transcript.RoleHuman, e[0].Role)
	assert.Equal(t, "u1", e[0].UserID)
	assert.Equal(t, "alice", e[0].Speaker)
	assert.Equal(t, "hello", e[0].Text)
	assert.Equal(t, "chat-1", e[0].ChatID)
	assert.Equal(t, "m10", e[0].MessageID)
	assert.Equal(t, "m9", e[0].ReplyTo)
	assert.Equal(t, transcript.RoleBot, e[1].Role)
	assert.Equal(t, "the answer", e[1].Text)
	assert.Equal(t, "chat-1", e[1].ChatID)
}

// A refusal is the bot's real serve-path reply, so it IS recorded as a bot turn —
// deliberately unlike the memory buffer, which drops refusals (ADR 0015).
func TestHandlerTranscriptRefusalIsBotTurn(t *testing.T) {
	tlog := &transcript.MemoryLog{}
	engine := &mockEngine{reply: core.Reply{Text: "that's outside what I can answer", Refused: true}}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, engine, nil, nil, nil, nil, nil, transcriptPolicy(tlog))

	_, err := h(context.Background(), transcriptMsg)
	require.NoError(t, err)

	e := tlog.Entries()
	require.Len(t, e, 2, "a refusal still records a bot turn")
	assert.Equal(t, transcript.RoleBot, e[1].Role)
	assert.Contains(t, e[1].Text, "outside what I can answer")
}

// A rate-limited directed message records the human turn plus a system marker
// (event rate_limited) so the human-turn-without-a-bot-turn gap self-explains; no
// bot turn (the throttle notice is operational, not the engine's answer).
func TestHandlerTranscriptRateLimitedMarker(t *testing.T) {
	tlog := &transcript.MemoryLog{}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideRateLimited}, &mockEngine{}, nil, nil, nil, nil, nil, transcriptPolicy(tlog))

	_, err := h(context.Background(), transcriptMsg)
	require.NoError(t, err)

	e := tlog.Entries()
	require.Len(t, e, 2)
	assert.Equal(t, transcript.RoleHuman, e[0].Role)
	assert.Equal(t, transcript.RoleSystem, e[1].Role)
	assert.Equal(t, transcript.EventRateLimited, e[1].Event)
	assert.Empty(t, e[1].Text, "a system marker carries no message text")
	for _, x := range e {
		assert.NotEqual(t, transcript.RoleBot, x.Role, "no bot turn for a throttled message")
	}
}

// Consented ambient chatter records the human turn only — it is not directed, so
// no answer is expected and there is no gap to mark (ADR 0015).
func TestHandlerTranscriptLogOnlyNoMarker(t *testing.T) {
	tlog := &transcript.MemoryLog{}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideLogOnly}, &mockEngine{}, nil, nil, nil, nil, nil, transcriptPolicy(tlog))

	_, err := h(context.Background(), transcriptMsg)
	require.NoError(t, err)

	e := tlog.Entries()
	require.Len(t, e, 1, "ambient chatter is a human turn only")
	assert.Equal(t, transcript.RoleHuman, e[0].Role)
}

// The transcript is group-only: a served DM records nothing (ADR 0015).
func TestHandlerTranscriptGroupOnly(t *testing.T) {
	tlog := &transcript.MemoryLog{}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, &mockEngine{reply: core.Reply{Text: "a"}}, nil, nil, nil, nil, nil, transcriptPolicy(tlog))

	_, err := h(context.Background(), transport.Inbound{User: core.User{ID: "d1"}, Surface: core.SurfaceDM, Text: "private", Directed: true})
	require.NoError(t, err)
	assert.Empty(t, tlog.Entries(), "a DM is never transcribed")
}

// Prospective withdrawal is automatic: once a user withdraws, the gate stops
// serving/logging them (a nudge/silent decision), so no new transcript entries are
// written — no purge needed, and past entries (written while consented) are never
// touched (ADR 0015).
func TestHandlerTranscriptProspectiveOnWithdrawal(t *testing.T) {
	tlog := &transcript.MemoryLog{}

	// While consented: served → human + bot recorded.
	served := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, &mockEngine{reply: core.Reply{Text: "a"}}, nil, nil, nil, nil, nil, transcriptPolicy(tlog))
	_, err := served(context.Background(), transcriptMsg)
	require.NoError(t, err)
	require.Len(t, tlog.Entries(), 2)

	// After withdrawal the gate no longer serves/logs (nudge for a directed msg):
	// no new transcript entries, and the earlier two remain.
	withdrawn := runtime.NewHandler(&mockGate{decision: acl.DecideNudge}, &mockEngine{}, nil, nil, nil, nil, nil, transcriptPolicy(tlog))
	_, err = withdrawn(context.Background(), transcriptMsg)
	require.NoError(t, err)
	assert.Len(t, tlog.Entries(), 2, "no new entries after withdrawal; past entries stay")
}

// A nil transcript log is a no-op — the bot runs without a transcript (default).
func TestHandlerTranscriptDisabledIsNoOp(t *testing.T) {
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, &mockEngine{reply: core.Reply{Text: "a"}}, nil, nil, nil, nil, nil, runtime.Policy{})
	_, err := h(context.Background(), transcriptMsg)
	require.NoError(t, err, "no transcript configured → served normally")
}

const testTTL = time.Minute

type mockGate struct {
	decision    acl.Decision
	err         error
	called      bool
	gotUser     core.User
	gotSurface  core.Surface
	gotDirected bool
}

func (m *mockGate) Check(_ context.Context, in acl.GateInput) (acl.Decision, error) {
	m.called = true
	m.gotUser, m.gotSurface, m.gotDirected = in.User, in.Surface, in.Directed
	return m.decision, m.err
}

type mockEngine struct {
	reply    core.Reply
	err      error
	called   bool
	gotQuery core.Query
}

func (m *mockEngine) Answer(_ context.Context, q core.Query) (core.Reply, error) {
	m.called = true
	m.gotQuery = q
	return m.reply, m.err
}

// A group message directed at the bot (the common answer path).
var inbound = transport.Inbound{
	User: core.User{ID: "u1"}, Surface: core.SurfaceGroup, Text: "hello", ReplyTo: "chat-1", Directed: true,
}

// Each group gate decision maps to the right reply; the gate is always checked
// first and the engine is reached only on DecideServe.
func TestHandlerDecisionMapping(t *testing.T) {
	cases := []struct {
		name        string
		decision    acl.Decision
		wantEngine  bool
		wantSilent  bool
		wantRefused bool
		wantTextSub string
	}{
		{"serve", acl.DecideServe, true, false, false, "engine answer"},
		{"log-only", acl.DecideLogOnly, false, true, false, ""},
		{"nudge", acl.DecideNudge, false, false, false, "opt in"},
		{"rate-limited", acl.DecideRateLimited, false, false, false, "rate limit"},
		{"silent", acl.DecideSilent, false, true, false, ""},
		{"refuse", acl.DecideRefuse, false, false, true, "can't help"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := &mockGate{decision: tc.decision}
			engine := &mockEngine{reply: core.Reply{Text: "engine answer"}}
			reply, err := runtime.NewHandler(gate, engine, nil, nil, nil, nil, nil, runtime.Policy{})(context.Background(), inbound)
			require.NoError(t, err)

			assert.True(t, gate.called, "gate is always checked first")
			assert.Equal(t, inbound.User, gate.gotUser)
			assert.Equal(t, inbound.Surface, gate.gotSurface)
			assert.Equal(t, inbound.Directed, gate.gotDirected, "directedness is passed to the gate")
			assert.Equal(t, tc.wantEngine, engine.called, "engine only on serve")
			assert.Equal(t, tc.wantSilent, reply.Silent)
			assert.Equal(t, tc.wantRefused, reply.Refused)
			if tc.wantTextSub != "" {
				assert.Contains(t, reply.Text, tc.wantTextSub)
			}
			if tc.wantSilent {
				assert.Empty(t, reply.Text, "a silent reply carries no text")
			}
			if tc.wantEngine {
				// The inbound was normalized into the engine's Query.
				assert.Equal(t, inbound.User, engine.gotQuery.User)
				assert.Equal(t, inbound.Surface, engine.gotQuery.Surface)
				assert.Equal(t, inbound.Text, engine.gotQuery.Text)
			}
		})
	}
}

// A gate error fails closed: a refusal, and the engine is never called.
func TestHandlerGateErrorFailsClosed(t *testing.T) {
	gate := &mockGate{err: errors.New("store down")}
	engine := &mockEngine{}
	reply, err := runtime.NewHandler(gate, engine, nil, nil, nil, nil, nil, runtime.Policy{})(context.Background(), inbound)
	require.NoError(t, err, "a gate error is handled, not surfaced as a crash")
	assert.True(t, reply.Refused)
	assert.False(t, engine.called, "engine not reached on a gate failure")
}

// Answer's ErrOverBudget becomes a graceful capacity reply, not a raw error.
func TestHandlerOverBudgetGraceful(t *testing.T) {
	gate := &mockGate{decision: acl.DecideServe}
	engine := &mockEngine{err: budget.ErrOverBudget}
	reply, err := runtime.NewHandler(gate, engine, nil, nil, nil, nil, nil, runtime.Policy{})(context.Background(), inbound)
	require.NoError(t, err, "over-budget is degraded gracefully")
	assert.Contains(t, reply.Text, "capacity")
	assert.True(t, reply.Refused)
}

// Any other Answer error propagates.
func TestHandlerAnswerErrorPropagates(t *testing.T) {
	gate := &mockGate{decision: acl.DecideServe}
	engine := &mockEngine{err: errors.New("boom")}
	_, err := runtime.NewHandler(gate, engine, nil, nil, nil, nil, nil, runtime.Policy{})(context.Background(), inbound)
	assert.Error(t, err)
}

// A served (consent-granted, directed) message is logged to the quarantine store
// with the user, surface, and text.
func TestHandlerLogsServedMessage(t *testing.T) {
	qlog := &quarantine.MemoryLog{}
	gate := &mockGate{decision: acl.DecideServe}
	engine := &mockEngine{reply: core.Reply{Text: "answer"}}
	h := runtime.NewHandler(gate, engine, nil, nil, nil, qlog, nil, runtime.Policy{})

	_, err := h(context.Background(), inbound)
	require.NoError(t, err)

	entries := qlog.Entries()
	require.Len(t, entries, 1)
	assert.Equal(t, "u1", entries[0].UserID)
	assert.Equal(t, "group", entries[0].Surface)
	assert.Equal(t, "hello", entries[0].Text)
	assert.Equal(t, "chat-1", entries[0].ChatID, "the chat id is logged for per-chat curation (#96)")
}

// A reply carries the replied-to message text into the engine's Query as
// reply-context, prefixed with its sender (ADR 0014) — buffer-independent.
func TestHandlerThreadsReplyContext(t *testing.T) {
	engine := &mockEngine{reply: core.Reply{Text: "ok"}}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, engine, nil, nil, nil, nil, nil, runtime.Policy{})

	in := transport.Inbound{
		User: core.User{ID: "u1"}, Surface: core.SurfaceGroup, Text: "what's this?", ReplyTo: "chat-1", Directed: true,
		ReplyToMessageID: "m9", ReplyToText: "the launch slips to Q3", ReplyToSender: "carol",
	}
	_, err := h(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, "carol: the launch slips to Q3", engine.gotQuery.ReplyContext, "replied-to text + sender reach the engine")

	// A non-reply carries no reply-context.
	plain := transport.Inbound{User: core.User{ID: "u1"}, Surface: core.SurfaceGroup, Text: "hi", ReplyTo: "chat-1", Directed: true}
	_, err = h(context.Background(), plain)
	require.NoError(t, err)
	assert.Empty(t, engine.gotQuery.ReplyContext, "a non-reply has no reply-context")
}

// The sender's display-name handle is logged for curation attribution (#99).
func TestHandlerLogsHandle(t *testing.T) {
	qlog := &quarantine.MemoryLog{}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, &mockEngine{reply: core.Reply{Text: "a"}}, nil, nil, nil, qlog, nil, runtime.Policy{})

	in := transport.Inbound{User: core.User{ID: "u1", Display: "alice"}, Surface: core.SurfaceGroup, Text: "hi", ReplyTo: "chat-1", Directed: true}
	_, err := h(context.Background(), in)
	require.NoError(t, err)

	entries := qlog.Entries()
	require.Len(t, entries, 1)
	assert.Equal(t, "alice", entries[0].Handle, "the sender's handle is logged")
}

// Consented ambient chatter (DecideLogOnly) is logged but draws no reply.
func TestHandlerLogOnlyLogsAndStaysSilent(t *testing.T) {
	qlog := &quarantine.MemoryLog{}
	engine := &mockEngine{}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideLogOnly}, engine, nil, nil, nil, qlog, nil, runtime.Policy{})

	reply, err := h(context.Background(), inbound)
	require.NoError(t, err)
	require.Len(t, qlog.Entries(), 1, "consented ambient chatter is logged")
	assert.True(t, reply.Silent, "log-only produces no reply")
	assert.False(t, engine.called, "ambient chatter is never answered")
}

// A reaction-mode nudge attaches an emoji to the triggering message and sends no
// text (ADR 0012); message-mode sends the opt-in text and no reaction.
func TestHandlerNudgeModes(t *testing.T) {
	reaction := runtime.Policy{Nudge: runtime.Nudge{Mode: runtime.NudgeReaction, Emoji: "🤝"}}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideNudge}, &mockEngine{}, nil, nil, nil, nil, nil, reaction)
	reply, err := h(context.Background(), inbound)
	require.NoError(t, err)
	assert.Equal(t, "🤝", reply.Reaction, "reaction-mode attaches the emoji")
	assert.Empty(t, reply.Text, "reaction-mode sends no text")

	// Message-mode (the zero-Policy default) sends text, no reaction.
	h = runtime.NewHandler(&mockGate{decision: acl.DecideNudge}, &mockEngine{}, nil, nil, nil, nil, nil, runtime.Policy{})
	reply, err = h(context.Background(), inbound)
	require.NoError(t, err)
	assert.Empty(t, reply.Reaction, "message-mode attaches no reaction")
	assert.Contains(t, reply.Text, "opt in", "message-mode sends the opt-in text")
}

// A rate-limited message is still consented content, so it is logged even though
// it isn't answered — directedness/rate change the reply, not whether we log.
func TestHandlerLogsRateLimitedMessage(t *testing.T) {
	qlog := &quarantine.MemoryLog{}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideRateLimited}, &mockEngine{}, nil, nil, nil, qlog, nil, runtime.Policy{})
	reply, err := h(context.Background(), inbound)
	require.NoError(t, err)
	assert.Len(t, qlog.Entries(), 1, "a rate-limited message is consented content — logged")
	assert.Contains(t, reply.Text, "rate limit")
}

// The quarantine log is group-only (ADR 0004): a served group message is logged,
// a served DM is not (a private one-to-one is not community content).
func TestHandlerLogsGroupOnly(t *testing.T) {
	qlog := &quarantine.MemoryLog{}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, &mockEngine{reply: core.Reply{Text: "a"}}, nil, nil, nil, qlog, nil, runtime.Policy{})

	_, err := h(context.Background(), transport.Inbound{User: core.User{ID: "g1"}, Surface: core.SurfaceGroup, Text: "community msg", Directed: true})
	require.NoError(t, err)
	require.Len(t, qlog.Entries(), 1, "a served group message is logged")

	// A DM with no consenter wired falls through to the gate (DecideServe here);
	// logServed still excludes it as a private one-to-one.
	_, err = h(context.Background(), transport.Inbound{User: core.User{ID: "d1"}, Surface: core.SurfaceDM, Text: "private msg", Directed: true})
	require.NoError(t, err)
	assert.Len(t, qlog.Entries(), 1, "a served DM is not logged")
}

// An admin DM is answered by the engine and, being a private one-to-one, is
// never logged (ADR 0012 + #38's group-only guard).
func TestHandlerAdminDMAnsweredNotLogged(t *testing.T) {
	qlog := &quarantine.MemoryLog{}
	engine := &mockEngine{reply: core.Reply{Text: "admin answer"}}
	consent := &mockConsenter{consent: acl.ConsentGranted}
	policy := runtime.Policy{Admins: runtime.NewAdminSet([]string{"admin1"})}
	h := runtime.NewHandler(&mockGate{}, engine, nil, nil, nil, qlog, consent, policy)

	reply, err := h(context.Background(), transport.Inbound{User: core.User{ID: "admin1"}, Surface: core.SurfaceDM, Text: "q", Directed: true})
	require.NoError(t, err)
	assert.True(t, engine.called, "an admin DM is answered by the engine")
	assert.Equal(t, "admin answer", reply.Text)
	assert.Empty(t, qlog.Entries(), "an admin DM is a private 1:1, never logged")
}

// An admin's consent commands run the consent flow, never the engine (#50): the
// DM is an admin's only opt-in path (they're consent-gated in the group), so
// /consent must grant rather than be answered as a query. A non-command admin DM
// still reaches the engine.
func TestHandlerAdminDMConsentCommands(t *testing.T) {
	policy := runtime.Policy{Admins: runtime.NewAdminSet([]string{"admin1"})}
	dm := func(text string) transport.Inbound {
		return transport.Inbound{User: core.User{ID: "admin1"}, Surface: core.SurfaceDM, Text: text, Directed: true}
	}

	// /consent from an admin grants consent through the flow, not the engine.
	engine := &mockEngine{reply: core.Reply{Text: "ANSWER"}}
	consent := &mockConsenter{consent: acl.ConsentUnknown}
	h := runtime.NewHandler(&mockGate{}, engine, nil, nil, nil, nil, consent, policy)
	reply, err := h(context.Background(), dm("/consent"))
	require.NoError(t, err)
	assert.False(t, engine.called, "an admin's /consent runs the consent flow, not the engine")
	assert.Equal(t, acl.ConsentGranted, consent.consent, "the admin is opted in")
	assert.Contains(t, reply.Text, "opted in")

	// /start and /consent remove likewise route to the flow, never the engine.
	for _, cmd := range []string{"/start", "/consent remove"} {
		engine := &mockEngine{reply: core.Reply{Text: "ANSWER"}}
		h := runtime.NewHandler(&mockGate{}, engine, nil, nil, nil, nil, &mockConsenter{consent: acl.ConsentGranted}, policy)
		_, err := h(context.Background(), dm(cmd))
		require.NoError(t, err)
		assert.False(t, engine.called, "an admin's %q runs the consent flow, not the engine", cmd)
	}

	// A genuine (non-command) admin query still reaches the engine.
	engine = &mockEngine{reply: core.Reply{Text: "ANSWER"}}
	h = runtime.NewHandler(&mockGate{}, engine, nil, nil, nil, nil, &mockConsenter{consent: acl.ConsentGranted}, policy)
	reply, err = h(context.Background(), dm("what is X?"))
	require.NoError(t, err)
	assert.True(t, engine.called, "a non-command admin DM is answered")
	assert.Equal(t, "ANSWER", reply.Text)
}

// A non-admin DM never reaches the engine — it is consent management only, even
// for a message that looks like a query (ADR 0012).
func TestHandlerNonAdminDMIsConsentOnly(t *testing.T) {
	engine := &mockEngine{reply: core.Reply{Text: "ANSWER"}}
	consent := &mockConsenter{consent: acl.ConsentGranted}
	policy := runtime.Policy{Admins: runtime.NewAdminSet([]string{"admin1"})}
	h := runtime.NewHandler(&mockGate{}, engine, nil, nil, nil, nil, consent, policy)

	reply, err := h(context.Background(), transport.Inbound{User: core.User{ID: "rando"}, Surface: core.SurfaceDM, Text: "what is X?", Directed: true})
	require.NoError(t, err)
	assert.False(t, engine.called, "a non-admin DM never reaches the engine")
	assert.NotEqual(t, "ANSWER", reply.Text)
}

// Admin is a DM-surface privilege only: in the group an unconsented admin is
// consent-gated like anyone else — nudged, not answered (ADR 0012). Locks in the
// no-admin-special-case-in-group choice over a real gate.
func TestHandlerAdminInGroupIsConsentGated(t *testing.T) {
	ctx := context.Background()
	aclStore, err := acl.OpenBolt(filepath.Join(t.TempDir(), "acl.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = aclStore.Close() })
	gate := acl.NewGate(aclStore, acl.TierDefault)
	engine := &mockEngine{reply: core.Reply{Text: "ANSWER"}}
	// admin1 is a configured admin but has not consented.
	policy := runtime.Policy{Admins: runtime.NewAdminSet([]string{"admin1"})}
	h := runtime.NewHandler(gate, engine, nil, nil, nil, nil, gate, policy)

	reply, err := h(ctx, transport.Inbound{User: core.User{ID: "admin1"}, Surface: core.SurfaceGroup, Text: "hi bot", Directed: true})
	require.NoError(t, err)
	assert.False(t, engine.called, "an unconsented admin in the group is not answered")
	assert.Contains(t, reply.Text, "opt in", "an unconsented admin in the group is nudged like anyone")
}

// The unconsented/error decisions log nothing — consent is not established, so
// ADR 0002's "record nothing without consent" holds.
func TestHandlerDoesNotLogWhenNotConsented(t *testing.T) {
	for _, decision := range []acl.Decision{
		acl.DecideNudge, acl.DecideSilent, acl.DecideRefuse,
	} {
		qlog := &quarantine.MemoryLog{}
		h := runtime.NewHandler(&mockGate{decision: decision}, &mockEngine{}, nil, nil, nil, qlog, nil, runtime.Policy{})
		_, err := h(context.Background(), inbound)
		require.NoError(t, err)
		assert.Empty(t, qlog.Entries(), "decision %d logs nothing", decision)
	}
}

// Named: a user who has explicitly DECLINED consent logs nothing — end to end
// through the real gate. This is the clearest "record nothing without consent"
// case: the user actively refused.
func TestHandlerDeclinedConsentLogsNothing(t *testing.T) {
	ctx := context.Background()
	aclStore, err := acl.OpenBolt(filepath.Join(t.TempDir(), "acl.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = aclStore.Close() })
	require.NoError(t, aclStore.Put(ctx, acl.Record{UserID: "u1", Consent: acl.ConsentDeclined}))
	gate := acl.NewGate(aclStore, acl.TierDefault)

	qlog := &quarantine.MemoryLog{}
	h := runtime.NewHandler(gate, &mockEngine{}, nil, nil, nil, qlog, nil, runtime.Policy{})

	_, err = h(ctx, transport.Inbound{User: core.User{ID: "u1"}, Surface: core.SurfaceGroup, Text: "secret", Directed: true})
	require.NoError(t, err)
	assert.Empty(t, qlog.Entries(), "a declined user's message is never recorded")
}

// A nil log disables logging without a panic on the serve path.
func TestHandlerNilLogNoOp(t *testing.T) {
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, &mockEngine{reply: core.Reply{Text: "a"}}, nil, nil, nil, nil, nil, runtime.Policy{})
	_, err := h(context.Background(), inbound)
	require.NoError(t, err)
}

// A button click is not a community message and is never logged.
func TestHandlerCallbackDoesNotLog(t *testing.T) {
	qlog := &quarantine.MemoryLog{}
	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "echo"})
	h := runtime.NewHandler(&mockGate{}, &mockEngine{}, store,
		registryWith("echo", runtime.EchoVerb()), &mockAuthz{authorized: true}, qlog, nil, runtime.Policy{})

	_, err := h(context.Background(), callbackInbound(token))
	require.NoError(t, err)
	assert.Empty(t, qlog.Entries(), "a callback is not logged")
}

// callbackInbound is a button-click inbound (user u1) carrying token.
func callbackInbound(token string) transport.Inbound {
	return transport.Inbound{
		User:    core.User{ID: "u1"},
		Surface: core.SurfaceDM,
		Callback: &transport.Callback{
			Token: token, QueryID: "cq1", MessageID: "5",
		},
	}
}

// mockAuthz records the re-authorization call and returns a canned verdict.
type mockAuthz struct {
	authorized bool
	err        error
	called     bool
	gotMinTier acl.Tier
}

func (m *mockAuthz) Authorize(_ context.Context, _ core.User, minTier acl.Tier) (bool, error) {
	m.called = true
	m.gotMinTier = minTier
	return m.authorized, m.err
}

// spyVerb counts validate/execute calls so a test can prove which stages ran.
type spyVerb struct {
	execCalls     int
	validateCalls int
	gotSubject    core.User
	gotParams     map[string]string
	execErr       error
	validateErr   error
	status        string
}

func (s *spyVerb) verb(minTier acl.Tier) runtime.Verb {
	return runtime.Verb{
		MinTier:  minTier,
		Validate: func(map[string]string) error { s.validateCalls++; return s.validateErr },
		Execute: func(_ context.Context, subj core.User, p map[string]string) (string, error) {
			s.execCalls++
			s.gotSubject, s.gotParams = subj, p
			return s.status, s.execErr
		},
	}
}

func registryWith(name string, v runtime.Verb) *runtime.Registry {
	r := runtime.NewRegistry()
	r.Register(name, v)
	return r
}

func putToken(t *testing.T, store pending.Store, a core.Action) string {
	t.Helper()
	tok, err := store.Put(context.Background(), a)
	require.NoError(t, err)
	return tok
}

// Happy path: an authorized subject with valid params runs the executor once and
// gets a success toast plus a status line; the gate and engine are untouched.
func TestHandlerCallbackHappyPath(t *testing.T) {
	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "act", Params: map[string]string{"k": "v"}})
	spy := &spyVerb{status: "did the thing"}
	authz := &mockAuthz{authorized: true}
	gate, engine := &mockGate{}, &mockEngine{}

	h := runtime.NewHandler(gate, engine, store, registryWith("act", spy.verb(acl.TierAdmin)), authz, nil, nil, runtime.Policy{})
	reply, err := h(context.Background(), callbackInbound(token))
	require.NoError(t, err)

	assert.Equal(t, 1, spy.execCalls, "executor runs exactly once")
	assert.Equal(t, acl.TierAdmin, authz.gotMinTier, "re-authorized against the verb's tier")
	assert.Equal(t, "u1", spy.gotSubject.ID, "executor gets the acting subject")
	assert.Contains(t, reply.Notice, "Done")
	assert.Equal(t, "did the thing", reply.Text, "the status line rides in Text for the edit")
	assert.False(t, gate.called, "a click never consults the consent gate")
	assert.False(t, engine.called, "a click never reaches the engine")
}

// The denial matrix: unknown verb, under-tier, authorize error, and invalid
// params each fail closed with a toast and never reach the executor.
func TestHandlerCallbackDenials(t *testing.T) {
	cases := []struct {
		name        string
		authorized  bool
		authzErr    error
		validateErr error
		verbName    string // registered name; action always uses "act"
		wantNotice  string
		wantAuthz   bool // was Authorize consulted?
	}{
		{"unknown-verb", true, nil, nil, "other", "That action is no longer available.", false},
		{"under-tier", false, nil, nil, "act", "You don't have permission to do that.", true},
		{"authorize-error", false, errors.New("store down"), nil, "act", "You don't have permission to do that.", true},
		{"invalid-params", true, nil, errors.New("bad"), "act", "That action can't be completed as requested.", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := pending.NewMemoryStore(testTTL)
			token := putToken(t, store, core.Action{Verb: "act"})
			spy := &spyVerb{validateErr: tc.validateErr}
			authz := &mockAuthz{authorized: tc.authorized, err: tc.authzErr}

			h := runtime.NewHandler(&mockGate{}, &mockEngine{}, store,
				registryWith(tc.verbName, spy.verb(acl.TierAdmin)), authz, nil, nil, runtime.Policy{})
			reply, err := h(context.Background(), callbackInbound(token))
			require.NoError(t, err)

			assert.Equal(t, tc.wantNotice, reply.Notice)
			assert.Equal(t, 0, spy.execCalls, "the executor is never reached on a denial")
			assert.Equal(t, tc.wantAuthz, authz.called, "authorize is consulted only after a known verb")
		})
	}
}

// Authorize precedes validate: an unauthorized clicker never reaches param
// validation, so it can't probe valid param shapes from a validation error.
func TestHandlerCallbackAuthorizeBeforeValidate(t *testing.T) {
	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "act"})
	spy := &spyVerb{}
	authz := &mockAuthz{authorized: false}

	h := runtime.NewHandler(&mockGate{}, &mockEngine{}, store, registryWith("act", spy.verb(acl.TierAdmin)), authz, nil, nil, runtime.Policy{})
	_, err := h(context.Background(), callbackInbound(token))
	require.NoError(t, err)

	assert.True(t, authz.called)
	assert.Equal(t, 0, spy.validateCalls, "validation never runs for an unauthorized click")
}

// Executor error: a failure toast, the token stays consumed (no auto-retry).
func TestHandlerCallbackExecutorError(t *testing.T) {
	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "act"})
	spy := &spyVerb{execErr: errors.New("boom")}
	authz := &mockAuthz{authorized: true}

	h := runtime.NewHandler(&mockGate{}, &mockEngine{}, store, registryWith("act", spy.verb(acl.TierAdmin)), authz, nil, nil, runtime.Policy{})
	reply, err := h(context.Background(), callbackInbound(token))
	require.NoError(t, err)
	assert.Equal(t, "That didn't go through — please try again.", reply.Notice)
	assert.Equal(t, 1, spy.execCalls)

	// The token was consumed on this resolve; a second click reports so.
	reply, err = h(context.Background(), callbackInbound(token))
	require.NoError(t, err)
	assert.Equal(t, "Already completed.", reply.Notice)
}

// Non-resolved statuses (consumed, expired, no store) toast and never execute.
func TestHandlerCallbackNonResolved(t *testing.T) {
	spy := &spyVerb{}
	authz := &mockAuthz{authorized: true}
	reg := registryWith("act", spy.verb(acl.TierAdmin))

	// No store -> expired.
	h := runtime.NewHandler(&mockGate{}, &mockEngine{}, nil, reg, authz, nil, nil, runtime.Policy{})
	reply, err := h(context.Background(), callbackInbound("tok"))
	require.NoError(t, err)
	assert.Equal(t, "This action has expired.", reply.Notice)

	// Consumed after one resolve.
	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "act"})
	h = runtime.NewHandler(&mockGate{}, &mockEngine{}, store, reg, authz, nil, nil, runtime.Policy{})
	_, err = h(context.Background(), callbackInbound(token))
	require.NoError(t, err)
	reply, err = h(context.Background(), callbackInbound(token))
	require.NoError(t, err)
	assert.Equal(t, "Already completed.", reply.Notice)

	assert.Equal(t, 1, spy.execCalls, "only the single fresh resolve executed")
}

// The load-bearing rule end to end over a real acl gate: a tier demotion between
// render and click denies, and the privileged effect never commits.
func TestHandlerCallbackDemotionDenies(t *testing.T) {
	ctx := context.Background()
	aclStore, err := acl.OpenBolt(filepath.Join(t.TempDir(), "acl.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = aclStore.Close() })
	gate := acl.NewGate(aclStore, acl.TierDefault)
	require.NoError(t, gate.SetTier(ctx, "u1", acl.TierAdmin)) // clicker is admin at render

	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "set_tier", Params: map[string]string{"user": "target", "tier": "trusted"}})
	h := runtime.NewHandler(nil, nil, store, runtime.DefaultRegistry(gate), gate, nil, nil, runtime.Policy{})

	// Demote the clicker after the button was shown, before the click.
	require.NoError(t, gate.SetTier(ctx, "u1", acl.TierDefault))

	reply, err := h(ctx, callbackInbound(token))
	require.NoError(t, err)
	assert.Equal(t, "You don't have permission to do that.", reply.Notice)

	rec, err := aclStore.Get(ctx, "target")
	require.NoError(t, err)
	assert.NotEqual(t, acl.TierTrusted, rec.Tier, "the privileged effect never committed")
}

// The happy path over the real gate + DefaultRegistry: an admin sets a target's
// tier, the effect commits, and the status line reflects it.
func TestHandlerCallbackSetTierHappyPath(t *testing.T) {
	ctx := context.Background()
	aclStore, err := acl.OpenBolt(filepath.Join(t.TempDir(), "acl.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = aclStore.Close() })
	gate := acl.NewGate(aclStore, acl.TierDefault)
	require.NoError(t, gate.SetTier(ctx, "u1", acl.TierAdmin))

	store := pending.NewMemoryStore(testTTL)
	token := putToken(t, store, core.Action{Verb: "set_tier", Params: map[string]string{"user": "target", "tier": "trusted"}})
	h := runtime.NewHandler(nil, nil, store, runtime.DefaultRegistry(gate), gate, nil, nil, runtime.Policy{})

	reply, err := h(ctx, callbackInbound(token))
	require.NoError(t, err)
	assert.Contains(t, reply.Notice, "Done")
	assert.Contains(t, reply.Text, "trusted")

	rec, err := aclStore.Get(ctx, "target")
	require.NoError(t, err)
	assert.Equal(t, acl.TierTrusted, rec.Tier, "the tier change committed")
}
