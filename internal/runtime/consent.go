package runtime

import (
	"context"
	"log/slog"
	"strings"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

// Consent-flow copy (ADR 0012). Phase-1 constants; per-instance / localized text
// is the i18n unit's job (#25). The disclosure is the informed-consent surface:
// it states BOTH that the bot answers directed messages AND that group messages
// are logged to the knowledge base — opting in covers both, so neither is hidden.
const (
	consentDisclosureText = "Before I include you, here's what opting in means:\n" +
		"• In the group, I'll answer you when you reply to me or @mention me.\n" +
		"• Your group messages are added to the community knowledge base I answer from.\n" +
		"Tap the button below (or send /consent) to opt in. You can withdraw anytime with /consent remove."
	consentGrantedText = "You're opted in — thanks. You can withdraw anytime with /consent remove."
	consentAlreadyText = "You're already opted in. Send /consent remove to withdraw."
	consentRemovedText = "You're opted out — I no longer log your messages or answer you. Send /consent to opt back in anytime."
	consentErrorText   = "Something went wrong — please try again."
	consentOptInLabel  = "Opt in"
)

// consenter reads and writes a user's consent; *acl.Gate satisfies it. Used by
// the DM consent flow.
type consenter interface {
	ConsentOf(ctx context.Context, userID string) (acl.Consent, error)
	SetConsent(ctx context.Context, userID string, c acl.Consent) error
}

// dmConsentFlow handles a DM message as consent management (ADR 0012): the
// non-admin DM surface is consent-only. `/consent` grants directly (the text
// backup); `/start`, a bare message, or anything else shows the opt-in button
// with the disclosure when the user hasn't consented, or their status + the
// withdraw hint when they already have. A bare non-command DM is an implicit
// `/start`, so the surface never falls through to silence. (Admin DM answering is
// wired ahead of this in unit d; `/consent remove` in unit c.)
func dmConsentFlow(ctx context.Context, consent consenter, in transport.Inbound) core.Reply {
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
	return core.Reply{
		Text:    consentDisclosureText,
		Actions: []core.Action{{Verb: verbConsentGrant, Label: consentOptInLabel}},
	}
}
