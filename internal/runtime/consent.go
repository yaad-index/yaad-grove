package runtime

import (
	"context"
	"log/slog"
	"strings"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/memory"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

// Consent-flow copy (ADR 0012). Phase-1 constants; per-instance / localized text
// is the i18n unit's job (#25). The disclosure is the informed-consent surface:
// it states BOTH that the bot answers directed messages AND that group messages
// are logged to the knowledge base — opting in covers both, so neither is hidden.
const (
	// The disclosure is assembled from an intro (what opting in covers), an optional
	// transcript line (added only when a transcript is active — ADR 0015), and a
	// closing tap instruction, so the tap line always reads last.
	consentDisclosureIntro = "Before I include you, here's what opting in means:\n" +
		"• In the group, I'll answer you when you reply to me or @mention me.\n" +
		"• Your group messages are added to the community knowledge base I answer from."
	// consentTranscriptLine states the durable-record posture so "prospective +
	// disclosed" is coherent: opting in is informed that past entries persist even
	// after withdrawal (ADR 0015). Included only when the transcript is active.
	consentTranscriptLine = "\n• Your messages are also kept in a lasting conversation record. Withdrawing stops new entries, but earlier ones stay."
	consentDisclosureTap  = "\nTap the button below (or send /consent) to opt in. You can withdraw anytime with /consent remove."
	// consentDisclosureText is the base disclosure with no transcript active — the
	// pre-0015 wording, unchanged.
	consentDisclosureText = consentDisclosureIntro + consentDisclosureTap
	consentGrantedText    = "You're opted in — thanks. You can withdraw anytime with /consent remove."
	consentAlreadyText    = "You're already opted in. Send /consent remove to withdraw."
	consentRemovedText    = "You're opted out — I no longer log your messages or answer you. Send /consent to opt back in anytime."
	consentErrorText      = "Something went wrong — please try again."
	consentOptInLabel     = "Opt in"
)

// consenter reads and writes a user's consent; *acl.Gate satisfies it. Used by
// the DM consent flow.
type consenter interface {
	ConsentOf(ctx context.Context, userID string) (acl.Consent, error)
	SetConsent(ctx context.Context, userID string, c acl.Consent) error
}

// isConsentCommand reports whether a DM is a consent-management command rather
// than a query. These route to the consent flow for everyone — including an
// admin, whose DM would otherwise be answered as a query, leaving them no way to
// opt in (an admin is consent-gated in the group like anyone, so the DM is their
// only consent path). #50.
func isConsentCommand(text string) bool {
	switch strings.TrimSpace(text) {
	case "/start", "/consent", "/consent remove":
		return true
	default:
		return false
	}
}

// dmConsentFlow handles a DM message as consent management (ADR 0012): the
// non-admin DM surface is consent-only. `/consent` grants directly (the text
// backup); `/start`, a bare message, or anything else shows the opt-in button
// with the disclosure when the user hasn't consented, or their status + the
// withdraw hint when they already have. A bare non-command DM is an implicit
// `/start`, so the surface never falls through to silence. `/consent remove`
// withdraws and, per ADR 0014, purges the user's turns from the conversation
// buffer everywhere (an actively-read buffer must stop shaping answers at once).
// The transcript (ADR 0015) is prospective and never read, so withdrawal just
// stops new entries — no purge here; transcriptActive only adds the durable-record
// line to the opt-in disclosure so consent is informed.
func dmConsentFlow(ctx context.Context, consent consenter, buf *memory.Buffer, transcriptActive bool, in transport.Inbound) core.Reply {
	switch strings.TrimSpace(in.Text) {
	case "/consent":
		if err := consent.SetConsent(ctx, in.User.ID, acl.ConsentGranted); err != nil {
			slog.Warn("consent grant failed", "err", err)
			return core.Reply{Text: consentErrorText}
		}
		return core.Reply{Text: consentGrantedText}
	case "/consent remove":
		// Self-withdrawal, always available (ADR 0012). Back to unconsented, so the
		// user can opt in again later.
		if err := consent.SetConsent(ctx, in.User.ID, acl.ConsentUnknown); err != nil {
			slog.Warn("consent removal failed", "err", err)
			return core.Reply{Text: consentErrorText}
		}
		// Purge their buffered turns everywhere (ADR 0014): the buffer is read into
		// prompts, so a withdrawn user's turns must stop shaping answers immediately.
		buf.PurgeUser(in.User.ID)
		return core.Reply{Text: consentRemovedText}
	}

	c, err := consent.ConsentOf(ctx, in.User.ID)
	if err != nil {
		slog.Warn("consent read failed", "err", err)
		return core.Reply{Text: consentErrorText}
	}
	if c == acl.ConsentGranted {
		return core.Reply{Text: consentAlreadyText}
	}
	disclosure := consentDisclosureText
	if transcriptActive {
		// Insert the durable-record line before the closing tap instruction, so the
		// disclosure reads intro → logging → transcript → tap.
		disclosure = consentDisclosureIntro + consentTranscriptLine + consentDisclosureTap
	}
	return core.Reply{
		Text:    disclosure,
		Actions: []core.Action{{Verb: verbConsentGrant, Label: consentOptInLabel}},
	}
}
