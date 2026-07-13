// Package transport defines the platform-neutral adapter boundary. A transport
// is a pluggable, cleanly extractable module: the core carries zero transport
// dependencies, and "plug anywhere" means adding another adapter, not touching
// core (ADR 0001).
//
// Features are defined here once in platform-neutral terms through a capability
// interface, never written against any one platform's API. Where a platform
// lacks a capability (say reactions), the feature degrades gracefully — falls
// back to commands — rather than becoming platform-only.
package transport

import (
	"context"
	"strconv"
	"strings"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// Inbound is a message a transport received, normalized toward core.Query. The
// transport fills identity and surface; the runtime turns it into a core.Query
// after the access/consent gates pass.
type Inbound struct {
	User    core.User
	Surface core.Surface
	Text    string
	// ReplyTo is an opaque, transport-owned handle for where to send the reply
	// (chat id, thread, etc.). The core never interprets it.
	ReplyTo string
	// Directed is true when the message is aimed at the bot: in a group, a reply to
	// one of the bot's messages or an @mention of it; a DM is always directed. It
	// distinguishes a message meant for the bot from ambient community chatter,
	// which the bot logs but never answers (ADR 0012).
	Directed bool
	// MessageID is the platform id of this message, and ReplyToMessageID the id of
	// the message it replies to (empty if none). They key the conversation buffer's
	// turns and its reply-to threading (ADR 0014).
	MessageID        string
	ReplyToMessageID string
	// ReplyToBot is true when this message is a reply to one of the bot's own
	// messages — the follow-up-gate signal for conversation memory (ADR 0014),
	// distinct from Directed (which also covers @mentions).
	ReplyToBot bool
	// ReplyToText is the text of the message this one replies to, when the platform
	// delivers it inline (ADR 0014). It lets the bot answer a reply about an
	// arbitrary earlier message it never buffered — the replied-to content is
	// injected as context straight from the update, buffer-independent. Empty when
	// the message isn't a reply or the platform doesn't inline the parent (e.g. a
	// cross-chat reply). ReplyToSender is that message's author display name, if known.
	ReplyToText   string
	ReplyToSender string
	// Callback is set when this inbound is a button click (a Telegram
	// callback_query) rather than a text message; it is nil for a normal message.
	// The acting user and surface — the subject the runtime re-authorizes against
	// (ADR 0009) — ride in User/Surface as usual, so a callback needs no separate
	// identity.
	Callback *Callback
}

// Callback carries the click-specific parts of a button-press inbound. Together
// with the Inbound's User, Surface, and ReplyTo (the chat), it is everything the
// runtime needs to resolve the action, acknowledge the click, and (later, ADR
// 0009 T3) edit the originating message in place — with no fetch-back.
type Callback struct {
	// Token keys the pending action in the callback store; the runtime resolves
	// it (only the token, never the action, rides in the button — ADR 0009).
	Token string
	// QueryID answers the click within the platform's acknowledgement window
	// (Telegram answerCallbackQuery, ~30s); it cannot be fetched after the fact,
	// so it must ride in the inbound.
	QueryID string
	// MessageID, with the Inbound's ReplyTo (chat), locates the message the
	// button is on, so the keyboard can later be edited to a status line.
	MessageID string
}

// Handler processes one inbound message and is supplied by the runtime. A
// transport calls it for each message it receives.
type Handler func(ctx context.Context, in Inbound) (core.Reply, error)

// Capability names an optional, platform-varying feature. The core asks a
// transport what it supports and degrades gracefully when it does not.
type Capability int

const (
	// CapReactions: the transport can attach emoji reactions to a message.
	CapReactions Capability = iota
	// CapEditMessage: the transport can edit a previously sent message.
	CapEditMessage
	// CapButtons: the transport can render a Reply's Actions as interactive
	// buttons (a Telegram inline keyboard). Where absent, the actions degrade to
	// an enumerated text list via ActionsAsText (ADR 0009).
	CapButtons
)

// ActionsAsText renders a Reply's actions as an enumerated list, the graceful
// fallback an adapter without CapButtons appends to the message text so the
// affordances are at least visible where they can't be tapped. It returns the
// empty string for no actions.
func ActionsAsText(actions []core.Action) string {
	if len(actions) == 0 {
		return ""
	}
	var b strings.Builder
	for i, a := range actions {
		b.WriteString("\n")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(". ")
		b.WriteString(a.Label)
	}
	return b.String()
}

// Transport is one platform adapter. Implementations live in sub-packages
// (transport/telegram, later transport/discord, ...). The interface stays
// platform-neutral so no feature may assume the transport.
type Transport interface {
	// Name identifies the adapter for logs and config.
	Name() string
	// Supports reports whether an optional capability is available here.
	Supports(c Capability) bool
	// Run receives messages and dispatches each to handler until ctx is done.
	Run(ctx context.Context, handler Handler) error
	// Send delivers a reply to the place identified by replyTo.
	Send(ctx context.Context, replyTo string, reply core.Reply) error
}
