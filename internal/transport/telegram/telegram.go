// Package telegram is the Phase-1 transport adapter. It is one pluggable
// implementation of transport.Transport and the only platform wired at first;
// Discord and Slack adapters follow the same boundary later (ADR 0001,
// roadmap). Nothing here leaks into the core.
package telegram

import (
	"context"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

// Config holds the Telegram bot credentials and scope.
type Config struct {
	// Token is the bot token, supplied via env/secret, never inlined in config.
	Token string
	// AllowedGroups scopes which group chats the bot serves (membership is the
	// group's own boundary; this pins which groups count as "the community").
	AllowedGroups []string
}

// Adapter implements transport.Transport for Telegram.
type Adapter struct {
	cfg Config
}

// New returns a Telegram adapter for cfg.
func New(cfg Config) *Adapter {
	return &Adapter{cfg: cfg}
}

// Name identifies this adapter.
func (a *Adapter) Name() string { return "telegram" }

// Supports reports Telegram's optional capabilities. Telegram has reactions and
// message edits; both map onto the platform-neutral feature set.
func (a *Adapter) Supports(c transport.Capability) bool {
	switch c {
	case transport.CapReactions, transport.CapEditMessage:
		return true
	default:
		return false
	}
}

// Run long-polls Telegram and dispatches each message to handler.
//
// Scaffold: no polling yet.
func (a *Adapter) Run(ctx context.Context, handler transport.Handler) error {
	return core.ErrNotImplemented
}

// Send delivers reply to the chat identified by replyTo.
//
// Scaffold: no send yet.
func (a *Adapter) Send(ctx context.Context, replyTo string, reply core.Reply) error {
	return core.ErrNotImplemented
}

// compile-time assertion that Adapter satisfies transport.Transport.
var _ transport.Transport = (*Adapter)(nil)
