package runtime

import (
	"context"
	"errors"
	"log/slog"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
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
func NewHandler(gate checker, engine answerer) transport.Handler {
	return func(ctx context.Context, in transport.Inbound) (core.Reply, error) {
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
