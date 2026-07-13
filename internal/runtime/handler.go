package runtime

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/memory"
	"github.com/yaad-index/yaad-grove/internal/pending"
	"github.com/yaad-index/yaad-grove/internal/quarantine"
	"github.com/yaad-index/yaad-grove/internal/transcript"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

// Phase-1 reply copy. Constants for now; a later config pass can override.
const (
	refuseText      = "Sorry, I can't help with that here."
	rateLimitedText = "You've hit the rate limit — please try again shortly."
	atCapacityText  = "I'm at capacity right now — please try again a little later."

	// Callback (button-click) acknowledgements, shown as an ephemeral toast (ADR
	// 0009). Every outcome toasts — a click is never a silent drop. Expired and
	// already-used are distinguished on purpose: same dead button, different cause.
	callbackDoneText     = "Done ✓"
	callbackExpiredText  = "This action has expired."
	callbackConsumedText = "Already completed."
	callbackErrorText    = "Something went wrong — please try again."

	// Denials on a resolved callback (ADR 0009 T3). Each fails closed and toasts a
	// reason; none reaches the executor.
	callbackDeniedText      = "You don't have permission to do that."
	callbackUnknownVerbText = "That action is no longer available."
	callbackInvalidText     = "That action can't be completed as requested."
	callbackFailedText      = "That didn't go through — please try again."
)

// checker is the group access/consent gate the handler runs ahead of the engine;
// the concrete *acl.Gate satisfies it. It is an interface so the handler's
// decision-mapping is unit-testable with a mock gate.
type checker interface {
	Check(ctx context.Context, in acl.GateInput) (acl.Decision, error)
}

// answerer is the engine the handler serves through; the concrete *core.Engine
// satisfies it.
type answerer interface {
	Answer(ctx context.Context, q core.Query) (core.Reply, error)
}

// authorizer re-authorizes a button click against a verb's required tier at
// execution time, reading the current tier fresh; the concrete *acl.Gate
// satisfies it. Separate from checker: a click is authorized by authority, not
// by the consent/rate gate a message query passes.
type authorizer interface {
	Authorize(ctx context.Context, user core.User, minTier acl.Tier) (bool, error)
}

// NewHandler builds the transport.Handler the runtime hands to every transport
// (ADR 0008/0012): the boundary that routes a message by surface and turns each
// gate decision into a reply — or silence.
//
// Surfaces split (ADR 0012). A DM is answered only for an admin (a private
// one-to-one, never logged); every other DM is consent management only, never a
// query. A group message goes through the consent gate, which decides from the
// user's consent and whether the message is directed at the bot: answer+log,
// log-only, a consent nudge, or nothing.
//
// A button click (in.Callback set) takes a separate path (ADR 0009): it resolves
// the token, re-authorizes the acting subject against the verb's required tier,
// and executes — it is not a model query, so it never touches the engine.
// callbacks may be nil for a text-only bot; a click then can't be resolved and is
// treated as expired.
func NewHandler(gate checker, engine answerer, callbacks pending.Store, registry *Registry, authz authorizer, qlog quarantine.Log, consent consenter, policy Policy) transport.Handler {
	return func(ctx context.Context, in transport.Inbound) (core.Reply, error) {
		if in.Callback != nil {
			return resolveCallback(ctx, callbacks, registry, authz, in), nil
		}
		// DM routing (ADR 0012). Consent commands run the consent flow for everyone,
		// including admins: an admin is consent-gated in the group like anyone, so
		// the DM is their only opt-in path — routing their /consent to the engine
		// would leave them unable to consent at all (#50). A non-command admin DM is
		// a genuine query, answered by the engine; every other DM is consent
		// management only. A nil consenter means no consent surface is wired (a
		// text-only bot), so a DM falls through to the gate below rather than being
		// dropped.
		if in.Surface == core.SurfaceDM && consent != nil {
			if policy.Admins.IsAdmin(in.User.ID) && !isConsentCommand(in.Text) {
				return answerRemembering(ctx, engine, policy.Memory, policy.Inject, in)
			}
			return dmConsentFlow(ctx, consent, policy.Memory, policy.Transcript != nil, in), nil
		}

		decision, err := gate.Check(ctx, acl.GateInput{User: in.User, Surface: in.Surface, Directed: in.Directed})
		if err != nil {
			// Fail closed: never serve on an unknown gate state.
			slog.Warn("consent gate check failed; refusing", "err", err)
			return core.Reply{Text: refuseText, Refused: true}, nil
		}

		// Consent-gated logging (ADR 0004/0012): every consented group message is
		// recorded — the answered directed one, the ambient log-only one, and even a
		// rate-limited one (still consented content). Directedness changes the reply,
		// not whether we log. Unconsented decisions (nudge/silent) and a store-error
		// refuse record nothing, holding ADR 0002's "record nothing without consent".
		// Logged before the reply so an Answer error still captures the message; a
		// log failure warns and never fails the reply.
		switch decision {
		case acl.DecideServe, acl.DecideLogOnly, acl.DecideRateLimited:
			logServed(ctx, qlog, in)
			// The consented human turn also enters the durable transcript (ADR 0015),
			// on the same consent gate as the quarantine log and across the same
			// outcomes. Prospective withdrawal needs no action here: once a user
			// withdraws, the gate stops classifying them as consented, so this branch
			// no longer fires for them — new entries simply stop, past ones stay.
			logTranscriptHuman(ctx, policy.Transcript, in)
		}

		switch decision {
		case acl.DecideServe:
			reply, err := answerRemembering(ctx, engine, policy.Memory, policy.Inject, in)
			// The bot's serve-path response — an answer OR a refusal — is the bot's real
			// reply to the query, so the transcript records it (ADR 0015). This is
			// deliberately unlike the memory buffer, which drops refusals as useless
			// prompt context: the transcript is an audit record, so a decline is worth
			// recording. A silent/empty reply or an engine error leaves no bot turn.
			if err == nil && !reply.Silent && reply.Text != "" {
				logTranscriptBot(ctx, policy.Transcript, in, reply.Text)
			}
			return reply, err
		case acl.DecideLogOnly:
			// Consented ambient chatter: logged + buffered as conversation context
			// (ADR 0014), no reply. Not a directed message, so no bot answer is expected
			// and its human-only transcript entry is not a gap — no marker.
			rememberUser(policy.Memory, in)
			return core.Reply{Silent: true}, nil
		case acl.DecideNudge:
			return nudgeReply(policy.Nudge), nil
		case acl.DecideRateLimited:
			// Still consented content — buffer it (ADR 0014), even though the answer
			// is throttled. The human turn was transcribed above but the throttle notice
			// is operational, not a bot turn — leaving a human-turn-without-a-bot-turn
			// gap. Emit a system marker so the transcript self-explains the silence
			// rather than reading like a bug (ADR 0015).
			rememberUser(policy.Memory, in)
			logTranscriptSystem(ctx, policy.Transcript, in, transcript.EventRateLimited)
			return core.Reply{Text: rateLimitedText}, nil
		case acl.DecideSilent:
			// Unconsented ambient chatter: nothing at all.
			return core.Reply{Silent: true}, nil
		case acl.DecideRefuse:
			return core.Reply{Text: refuseText, Refused: true}, nil
		default:
			// An unrecognized decision fails closed.
			slog.Warn("unknown gate decision; refusing", "decision", int(decision))
			return core.Reply{Text: refuseText, Refused: true}, nil
		}
	}
}

// answer runs the engine with the selected recent-conversation context (ADR 0014)
// and maps its outcome to a reply: a spend-ceiling breach (ADR 0006) degrades to
// a capacity notice rather than crashing; any other error propagates.
func answer(ctx context.Context, engine answerer, in transport.Inbound, history []core.HistoryTurn) (core.Reply, error) {
	reply, err := engine.Answer(ctx, core.Query{User: in.User, Surface: in.Surface, Text: in.Text, History: history})
	if err != nil {
		if errors.Is(err, budget.ErrOverBudget) {
			return core.Reply{Text: atCapacityText, Refused: true}, nil
		}
		return core.Reply{}, err
	}
	return reply, nil
}

// answerRemembering is the memory-aware answer path (ADR 0014): it selects the
// recent-conversation context, records the sender's turn, answers, then records
// the bot's answer. Select runs BEFORE the sender's turn is appended, so the
// current message never appears in its own injected context. A refusal is not
// buffered (a canned out-of-scope line is not useful follow-up context); a
// nil/disabled buffer makes the whole thing a plain answer.
func answerRemembering(ctx context.Context, engine answerer, buf *memory.Buffer, injectN int, in transport.Inbound) (core.Reply, error) {
	history := selectHistory(buf, in, injectN)
	rememberUser(buf, in)
	reply, err := answer(ctx, engine, in, history)
	if err == nil && !reply.Refused {
		rememberBot(buf, in.ReplyTo, reply.Text)
	}
	return reply, err
}

// nudgeReply renders the consent nudge for an unconsented directed message (ADR
// 0012): reaction-mode attaches an emoji to the triggering message, message-mode
// sends the opt-in text. The reaction path is only ever produced for a transport
// that supports it — the composition boundary downgrades reaction-mode to
// message-mode when the transport can't react, so the opt-in is never dropped.
func nudgeReply(n Nudge) core.Reply {
	n = n.resolve()
	if n.Mode == NudgeReaction {
		return core.Reply{Reaction: n.Emoji}
	}
	return core.Reply{Text: n.Text}
}

// logServed appends a consented group message to the quarantine log (ADR
// 0004/0012). The caller invokes it only for consent-confirmed decisions
// (serve / log-only / rate-limited), so consent is established before this runs.
// A nil log disables logging; a log failure is warned, never surfaced, so it
// can't break a reply the user is owed.
func logServed(ctx context.Context, qlog quarantine.Log, in transport.Inbound) {
	// Group-only (ADR 0004): the growth corpus is community chatter. A served DM is
	// a private one-to-one exchange (typically an admin), not community content, so
	// it is never logged.
	if qlog == nil || in.Surface != core.SurfaceGroup {
		return
	}
	if err := qlog.Append(ctx, quarantine.Entry{
		Time:    time.Now(),
		ChatID:  in.ReplyTo, // the chat id — the same handle memory + the transcript key on
		UserID:  in.User.ID,
		Handle:  in.User.Display, // human-readable name for curation attribution (#99)
		Surface: surfaceLabel(in.Surface),
		Text:    in.Text,
	}); err != nil {
		slog.Warn("quarantine log append failed", "err", err)
	}
}

// logTranscriptHuman records a consented member's turn in the durable transcript
// (ADR 0015). Like the quarantine log it is group-only — the transcript is a
// record of community conversation, and a DM is a private one-to-one, never
// transcribed. A nil log disables the transcript; a write failure is warned, never
// surfaced, so it can't break a reply the user is owed. Caller invokes it only on
// the consent-confirmed decisions, so consent is established before this runs.
func logTranscriptHuman(ctx context.Context, tlog transcript.Log, in transport.Inbound) {
	if tlog == nil || in.Surface != core.SurfaceGroup {
		return
	}
	appendTranscript(ctx, tlog, transcript.Entry{
		Time:      time.Now(),
		Role:      transcript.RoleHuman,
		ChatID:    in.ReplyTo,
		UserID:    in.User.ID,
		Speaker:   in.User.Display,
		Text:      in.Text,
		MessageID: in.MessageID,
		ReplyTo:   in.ReplyToMessageID,
	})
}

// logTranscriptBot records the bot's serve-path response — an answer or a refusal
// (ADR 0015). Group-only, same as the human turn. The bot's own sent-message id is
// not threaded in v1 (ADR 0014's deferred bot-turn id), so the entry carries no
// MessageID.
func logTranscriptBot(ctx context.Context, tlog transcript.Log, in transport.Inbound, text string) {
	if tlog == nil || in.Surface != core.SurfaceGroup {
		return
	}
	appendTranscript(ctx, tlog, transcript.Entry{
		Time:   time.Now(),
		Role:   transcript.RoleBot,
		ChatID: in.ReplyTo,
		Text:   text,
	})
}

// logTranscriptSystem records an operational marker that explains why a directed
// turn has no bot answer (ADR 0015) — currently only the rate-limited case.
// Group-only. It carries no user or text, just the event.
func logTranscriptSystem(ctx context.Context, tlog transcript.Log, in transport.Inbound, event string) {
	if tlog == nil || in.Surface != core.SurfaceGroup {
		return
	}
	appendTranscript(ctx, tlog, transcript.Entry{
		Time:   time.Now(),
		Role:   transcript.RoleSystem,
		ChatID: in.ReplyTo,
		Event:  event,
	})
}

// appendTranscript writes one entry, warning (never failing) on error — the
// transcript is a best-effort record, like the quarantine log.
func appendTranscript(ctx context.Context, tlog transcript.Log, e transcript.Entry) {
	if err := tlog.Append(ctx, e); err != nil {
		slog.Warn("transcript append failed", "err", err, "role", e.Role)
	}
}

// surfaceLabel renders a surface for the log in human-readable form.
func surfaceLabel(s core.Surface) string {
	switch s {
	case core.SurfaceDM:
		return "dm"
	case core.SurfaceGroup:
		return "group"
	default:
		return "unknown"
	}
}

// resolveCallback handles a button click (ADR 0009): it resolves the token and,
// on a fresh resolve, executes the verb — always returning an ephemeral
// acknowledgement (Reply.Notice), never a new message. A dead button reports
// expired vs already-completed distinctly; every path toasts, none is silent.
func resolveCallback(ctx context.Context, callbacks pending.Store, registry *Registry, authz authorizer, in transport.Inbound) core.Reply {
	if callbacks == nil {
		return core.Reply{Notice: callbackExpiredText}
	}
	action, status, err := callbacks.Resolve(ctx, in.Callback.Token)
	if err != nil {
		// Fail closed: a store error is not a licence to act — just acknowledge.
		slog.Warn("callback resolve failed", "err", err)
		return core.Reply{Notice: callbackErrorText}
	}
	switch status {
	case pending.Resolved:
		return executeAction(ctx, registry, authz, in.User, action)
	case pending.Consumed:
		return core.Reply{Notice: callbackConsumedText}
	case pending.Expired:
		return core.Reply{Notice: callbackExpiredText}
	default:
		return core.Reply{Notice: callbackErrorText}
	}
}

// executeAction runs a resolved verb behind the re-authorization spine (ADR
// 0009). Order is deliberate: lookup -> authorize -> validate -> execute. A
// resolved token proves the button was shown, not that the subject still has
// authority, so authorization reads the CURRENT tier and precedes param
// validation (so an unauthorized clicker can't probe valid param shapes). Every
// denial fails closed and toasts a reason; the executor is reached only on the
// authorized, valid path. On success the toast is the source of truth and any
// status line rides in Text for the transport's best-effort message edit.
func executeAction(ctx context.Context, registry *Registry, authz authorizer, subject core.User, action core.Action) core.Reply {
	if registry == nil {
		return core.Reply{Notice: callbackUnknownVerbText}
	}
	verb, ok := registry.Lookup(action.Verb)
	if !ok {
		return core.Reply{Notice: callbackUnknownVerbText}
	}

	authorized, err := authz.Authorize(ctx, subject, verb.MinTier)
	if err != nil {
		slog.Warn("callback authorize failed; denying", "verb", action.Verb, "err", err)
		return core.Reply{Notice: callbackDeniedText} // fail closed
	}
	if !authorized {
		return core.Reply{Notice: callbackDeniedText}
	}

	if verb.Validate != nil {
		if err := verb.Validate(action.Params); err != nil {
			return core.Reply{Notice: callbackInvalidText}
		}
	}

	statusLine, err := verb.Execute(ctx, subject, action.Params)
	if err != nil {
		// The action did not commit; the token is already single-use-consumed, so a
		// fresh action must be re-offered rather than auto-retried.
		slog.Error("callback verb failed", "verb", action.Verb, "err", err)
		return core.Reply{Notice: callbackFailedText}
	}
	return core.Reply{Notice: callbackDoneText, Text: statusLine}
}
