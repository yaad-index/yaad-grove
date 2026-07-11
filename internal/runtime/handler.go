package runtime

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/pending"
	"github.com/yaad-index/yaad-grove/internal/quarantine"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

// Phase-1 reply copy. Constants for now; a later config pass can override. The
// consent prompt is deliberately honest and short: it says the bot answers from a
// curated knowledge base, that continuing opts the user in, and that a minimal
// record is kept to do so.
const (
	consentPromptText = "Hi — I answer questions from a curated knowledge base. " +
		"To opt in, just reply with any message; ignore this to stay out. " +
		"I keep only a minimal record (enough to remember your choice), and never the content of " +
		"your messages until you opt in."
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

// checker is the access/consent gate the handler runs ahead of the engine; the
// concrete *acl.Gate satisfies it. It is an interface so the handler's
// decision-mapping is unit-testable with a mock gate.
type checker interface {
	Check(ctx context.Context, user core.User, surface core.Surface) (acl.Decision, error)
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
// (ADR 0007/0008): the boundary where the access/consent gate runs *ahead* of the
// engine, and each gate decision becomes a reply — or silence. The gate is always
// consulted first; the engine is only ever reached on DecideServe.
//
// A button click (in.Callback set) takes a separate path (ADR 0009): it resolves
// the token, re-authorizes the acting subject against the verb's required tier,
// and executes — it is not a model query, so it never touches the engine.
// callbacks may be nil for a text-only bot; a click then can't be resolved and is
// treated as expired.
func NewHandler(gate checker, engine answerer, callbacks pending.Store, registry *Registry, authz authorizer, qlog quarantine.Log) transport.Handler {
	return func(ctx context.Context, in transport.Inbound) (core.Reply, error) {
		if in.Callback != nil {
			return resolveCallback(ctx, callbacks, registry, authz, in), nil
		}
		decision, err := gate.Check(ctx, in.User, in.Surface)
		if err != nil {
			// Fail closed: never serve on an unknown gate state.
			slog.Warn("consent gate check failed; refusing", "err", err)
			return core.Reply{Text: refuseText, Refused: true}, nil
		}

		switch decision {
		case acl.DecideServe:
			// Consent-gated logging (ADR 0004): DecideServe is the ONLY path where
			// consent is confirmed, so it is the only place a message is recorded —
			// ADR 0002's "record nothing without consent" is enforced by this
			// placement. Logged before answering so an Answer error still captures
			// the message; a log failure warns and never fails the reply.
			logServed(ctx, qlog, in)
			reply, aerr := engine.Answer(ctx, core.Query{User: in.User, Surface: in.Surface, Text: in.Text})
			if aerr != nil {
				if errors.Is(aerr, budget.ErrOverBudget) {
					// The spend ceiling was hit mid-call — degrade gracefully, don't crash.
					return core.Reply{Text: atCapacityText, Refused: true}, nil
				}
				return core.Reply{}, aerr
			}
			return reply, nil
		case acl.DecideAskConsent:
			return core.Reply{Text: consentPromptText}, nil
		case acl.DecideRateLimited:
			return core.Reply{Text: rateLimitedText}, nil
		case acl.DecideSilent:
			// Throttled-unconsented (ADR 0007): send nothing at all.
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

// logServed appends a consented message to the quarantine log (ADR 0004). It is
// called only from the DecideServe branch — consent is confirmed there. A nil log
// disables logging; a log failure is warned, never surfaced, so it can't break a
// reply the user is owed.
func logServed(ctx context.Context, qlog quarantine.Log, in transport.Inbound) {
	// Group-only (ADR 0004): the growth corpus is community chatter. A served DM is
	// a private one-to-one exchange (typically an admin), not community content, so
	// it is never logged.
	if qlog == nil || in.Surface != core.SurfaceGroup {
		return
	}
	if err := qlog.Append(ctx, quarantine.Entry{
		Time:    time.Now(),
		UserID:  in.User.ID,
		Surface: surfaceLabel(in.Surface),
		Text:    in.Text,
	}); err != nil {
		slog.Warn("quarantine log append failed", "err", err)
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
