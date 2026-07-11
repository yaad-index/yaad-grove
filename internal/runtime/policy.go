package runtime

import "strings"

// Policy is the surface-split answering policy the handler applies (ADR 0012):
// the admin allowlist (admins are answered in a DM) and the consent nudge shown
// to an unconsented user who directs a message at the bot in the group. Both are
// per-instance configuration; the zero Policy is a bot with no admins and an
// empty nudge (message-mode, no text).
type Policy struct {
	Admins AdminSet
	Nudge  Nudge
}

// AdminSet is the configured admin allowlist (ADR 0012): a user is an admin iff
// their platform id is in the set. Admin status is a DM-surface privilege only —
// an admin DM is answered by the engine, but in the group an admin is
// consent-gated like any other member. There is no separate elevated-user tier
// in v1.
type AdminSet map[string]bool

// NewAdminSet builds an AdminSet from configured ids, trimming blanks so a
// stray empty entry can't admit an unidentified user.
func NewAdminSet(ids []string) AdminSet {
	set := make(AdminSet, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			set[id] = true
		}
	}
	return set
}

// IsAdmin reports whether id is a configured admin. The nil set has no admins.
func (a AdminSet) IsAdmin(id string) bool { return a[id] }

// NudgeMode selects how the consent nudge is delivered to an unconsented user
// who directs a message at the bot in the group (ADR 0012).
type NudgeMode string

const (
	// NudgeMessage sends a short text reply with the opt-in instruction. The
	// default: it works on every transport.
	NudgeMessage NudgeMode = "message"
	// NudgeReaction attaches an emoji reaction to the user's message instead of a
	// text reply, where the transport supports reactions. It degrades to a message
	// where reactions are unavailable.
	NudgeReaction NudgeMode = "reaction"
)

// Nudge is the configurable consent nudge (ADR 0012). Mode picks a text reply
// (default) or an emoji reaction; Text is the message-mode copy and Emoji the
// reaction-mode glyph. A localized per-instance catalog is future work (#25).
type Nudge struct {
	Mode  NudgeMode
	Text  string
	Emoji string
}

// Nudge copy defaults (ADR 0012). Constants for now; the i18n unit (#25) will
// route these through a catalog.
const (
	// DefaultNudgeText is the message-mode opt-in instruction. It points at the DM
	// consent flow, the only place consent is granted.
	DefaultNudgeText = "To chat with me here, opt in first: DM me and tap the opt-in button (or send /consent)."
	// DefaultNudgeEmoji is the reaction-mode glyph — a handshake, an unobtrusive
	// "let's connect" that doesn't add noise to the group.
	DefaultNudgeEmoji = "🤝"
)

// resolve fills unset fields with the Phase-1 defaults, so a partial config (or
// the zero Nudge) still produces a usable nudge.
func (n Nudge) resolve() Nudge {
	if n.Mode == "" {
		n.Mode = NudgeMessage
	}
	if n.Text == "" {
		n.Text = DefaultNudgeText
	}
	if n.Emoji == "" {
		n.Emoji = DefaultNudgeEmoji
	}
	return n
}
