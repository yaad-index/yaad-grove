// Package telegram is the Phase-1 transport adapter. It is one pluggable
// implementation of transport.Transport and the only platform wired at first;
// Discord and Slack adapters follow the same boundary later (ADR 0001,
// roadmap). Nothing here leaks into the core.
//
// The Bot API is driven through github.com/go-telegram/bot: a maintained,
// full-coverage client that is itself dependency-free, so the interactive
// affordances the control plane needs (inline keyboards, callbacks, reactions)
// arrive without re-implementing each endpoint (ADR 0005/0009). This unit is at
// text parity: long-poll receive plus a text reply; the library owns the update
// loop, offset acking, and clean context-cancel shutdown.
package telegram

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

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
	// serverURL overrides the Bot API base; empty uses the library default. Set
	// only by tests to point the client at a mock server.
	serverURL string
	// skipGetMe skips the token-validating getMe call at startup; set only by
	// tests so construction needs no live credentials.
	skipGetMe bool
	// bot is the live client, created in Run and used by Send. Send is only ever
	// reached from Run's dispatch, after Run has assigned it (happens-before the
	// library's handler goroutines), so no lock is needed.
	bot *bot.Bot
}

// New returns a Telegram adapter for cfg. It is a pure constructor: the Bot API
// client (and its startup getMe) is built in Run, which owns the run lifecycle.
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

// Run long-polls Telegram and dispatches each message to handler until ctx is
// cancelled. The library owns the update loop — offset acking and a clean
// stop on ctx.Done — so this maps each update to a transport.Inbound, runs the
// handler, and sends the reply back.
func (a *Adapter) Run(ctx context.Context, handler transport.Handler) error {
	dispatch := func(ctx context.Context, _ *bot.Bot, u *models.Update) {
		in, ok := a.toInbound(u)
		if !ok {
			return
		}
		reply, err := handler(ctx, in)
		if err != nil {
			slog.Error("telegram handler failed", "err", err)
			return
		}
		if err := a.Send(ctx, in.ReplyTo, reply); err != nil {
			slog.Error("telegram send failed", "err", err)
		}
	}

	opts := []bot.Option{
		bot.WithDefaultHandler(dispatch),
		bot.WithErrorsHandler(func(err error) {
			// A cancelled poll during shutdown is expected, not an error.
			if ctx.Err() != nil {
				return
			}
			slog.Warn("telegram update poll failed", "err", a.redact(err))
		}),
	}
	if a.serverURL != "" {
		opts = append(opts, bot.WithServerURL(a.serverURL))
	}
	if a.skipGetMe {
		opts = append(opts, bot.WithSkipGetMe())
	}

	b, err := bot.New(a.cfg.Token, opts...)
	if err != nil {
		return a.redact(err)
	}
	a.bot = b

	b.Start(ctx) // blocks until ctx is cancelled
	return ctx.Err()
}

// Send delivers reply to the chat identified by replyTo (its opaque chat id). A
// silent or empty reply sends nothing — matching the gate's Silent -> no-send
// contract (ADR 0007).
func (a *Adapter) Send(ctx context.Context, replyTo string, reply core.Reply) error {
	if reply.Silent || strings.TrimSpace(reply.Text) == "" {
		return nil
	}
	if a.bot == nil {
		return errors.New("telegram: transport not running")
	}
	chatID, err := strconv.ParseInt(replyTo, 10, 64)
	if err != nil {
		return errors.New("telegram: invalid chat id " + strconv.Quote(replyTo))
	}
	if _, err := a.bot.SendMessage(ctx, &bot.SendMessageParams{ChatID: chatID, Text: reply.Text}); err != nil {
		return a.redact(err)
	}
	return nil
}

// toInbound maps a Telegram update to a transport.Inbound, or ok=false to drop
// it. Only plain text messages are in scope this unit; an edited message, a
// channel post, a callback query, or a group not in AllowedGroups is dropped.
func (a *Adapter) toInbound(u *models.Update) (transport.Inbound, bool) {
	m := u.Message
	if m == nil || m.From == nil || strings.TrimSpace(m.Text) == "" {
		return transport.Inbound{}, false
	}
	var surface core.Surface
	switch m.Chat.Type {
	case models.ChatTypePrivate:
		surface = core.SurfaceDM
	case models.ChatTypeGroup, models.ChatTypeSupergroup:
		surface = core.SurfaceGroup
		if !a.groupAllowed(m.Chat.ID) {
			return transport.Inbound{}, false
		}
	default:
		return transport.Inbound{}, false
	}
	display := m.From.Username
	if display == "" {
		display = m.From.FirstName
	}
	return transport.Inbound{
		User:    core.User{ID: strconv.FormatInt(m.From.ID, 10), Display: display},
		Surface: surface,
		Text:    m.Text,
		ReplyTo: strconv.FormatInt(m.Chat.ID, 10),
	}, true
}

// groupAllowed reports whether a group chat is in AllowedGroups. An empty
// allow-list serves no groups (the safe default: a bot added to a random group
// stays silent until the operator lists it).
func (a *Adapter) groupAllowed(chatID int64) bool {
	id := strconv.FormatInt(chatID, 10)
	for _, g := range a.cfg.AllowedGroups {
		if g == id {
			return true
		}
	}
	return false
}

// redact strips the bot token from an error's text — the Bot API carries the
// token in the request URL path, which net/http embeds in transport errors — so
// it never reaches a log or a caller.
func (a *Adapter) redact(err error) error {
	if err == nil || a.cfg.Token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), a.cfg.Token, "«token»"))
}

// compile-time assertion that Adapter satisfies transport.Transport.
var _ transport.Transport = (*Adapter)(nil)
