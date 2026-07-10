package runtime

import (
	"context"
	"errors"
	"log/slog"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/pending"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

// Phase-1 reply copy. Constants for now; a later config pass can override. The
// consent prompt is deliberately honest and short: it says the bot answers from a
// curated knowledge base, that continuing opts the user in, and that a minimal
// record is kept to do so.
const (
	consentPromptText = "Hi — I answer questions from a curated knowledge base. " +
		"If you keep chatting with me, you're opting in, and I'll keep a minimal record " +
		"(just enough to remember your choice), never the content of your messages until you do. " +
		"Reply to continue, or ignore this if you'd rather not."
	refuseText      = "Sorry, I can't help with that here."
	rateLimitedText = "You've hit the rate limit — please try again shortly."
	atCapacityText  = "I'm at capacity right now — please try again a little later."

	// Callback (button-click) acknowledgements, shown as an ephemeral toast (ADR
	// 0009). A clean resolve gets a generic "done" — T2 has no verbs to run yet;
	// T3 replaces this with the executed verb's result. Expired and already-used
	// are distinguished on purpose: same dead button, different cause.
	callbackDoneText     = "Done ✓"
	callbackExpiredText  = "This action has expired."
	callbackConsumedText = "Already completed."
	callbackErrorText    = "Something went wrong — please try again."
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

// NewHandler builds the transport.Handler the runtime hands to every transport
// (ADR 0007/0008): the boundary where the access/consent gate runs *ahead* of the
// engine, and each gate decision becomes a reply — or silence. The gate is always
// consulted first; the engine is only ever reached on DecideServe.
//
// A button click (in.Callback set) takes a separate path (ADR 0009): it resolves
// the token in callbacks and acknowledges the click — it is not a model query, so
// it never touches the engine. callbacks may be nil for a text-only bot; a click
// then can't be resolved and is treated as expired.
func NewHandler(gate checker, engine answerer, callbacks pending.Store) transport.Handler {
	return func(ctx context.Context, in transport.Inbound) (core.Reply, error) {
		if in.Callback != nil {
			return resolveCallback(ctx, callbacks, in.Callback)
		}
		decision, err := gate.Check(ctx, in.User, in.Surface)
		if err != nil {
			// Fail closed: never serve on an unknown gate state.
			slog.Warn("consent gate check failed; refusing", "err", err)
			return core.Reply{Text: refuseText, Refused: true}, nil
		}

		switch decision {
		case acl.DecideServe:
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

// resolveCallback handles a button click (ADR 0009 T2): it resolves the token and
// returns an ephemeral acknowledgement (Reply.Notice) — never a new message. A
// dead button reports expired vs already-completed distinctly. A cleanly resolved
// action gets a generic "done"; T2 has no verbs, so nothing executes here — that
// is where T3 inserts re-authorization + execution, keyed on in.User's tier.
func resolveCallback(ctx context.Context, callbacks pending.Store, cb *transport.Callback) (core.Reply, error) {
	if callbacks == nil {
		return core.Reply{Notice: callbackExpiredText}, nil
	}
	_, status, err := callbacks.Resolve(ctx, cb.Token)
	if err != nil {
		// Fail closed: a store error is not a licence to act — just acknowledge.
		slog.Warn("callback resolve failed", "err", err)
		return core.Reply{Notice: callbackErrorText}, nil
	}
	switch status {
	case pending.Resolved:
		return core.Reply{Notice: callbackDoneText}, nil
	case pending.Consumed:
		return core.Reply{Notice: callbackConsumedText}, nil
	case pending.Expired:
		return core.Reply{Notice: callbackExpiredText}, nil
	default:
		return core.Reply{Notice: callbackErrorText}, nil
	}
}
