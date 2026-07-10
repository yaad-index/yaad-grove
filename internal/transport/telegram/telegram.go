// Package telegram is the Phase-1 transport adapter. It is one pluggable
// implementation of transport.Transport and the only platform wired at first;
// Discord and Slack adapters follow the same boundary later (ADR 0001,
// roadmap). Nothing here leaks into the core.
//
// The Bot API is driven through github.com/go-telegram/bot: a maintained,
// full-coverage client that is itself dependency-free, so the interactive
// affordances the control plane needs (inline keyboards, callbacks, reactions)
// arrive without re-implementing each endpoint (ADR 0005/0009).
//
// Beyond text (T1), this adapter renders a Reply's Actions as an inline keyboard
// and ingests the resulting button clicks (ADR 0009 T2). A rendered button
// carries only an opaque token; the action itself lives in the pending store,
// which the runtime resolves when the click arrives. The token is a UI handle,
// never authority — re-authorization is the runtime's job (T3).
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
	"github.com/yaad-index/yaad-grove/internal/pending"
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
	// callbacks mints a token per rendered button and is resolved by the runtime
	// on a click. Nil disables buttons: Actions degrade to a text list.
	callbacks pending.Store
	// serverURL overrides the Bot API base; empty uses the library default. Set
	// only by tests to point the client at a mock server.
	serverURL string
	// skipGetMe skips the token-validating getMe call at startup; set only by
	// tests so construction needs no live credentials.
	skipGetMe bool
	// bot is the live client, created in Run and used by Send/callbacks. It is
	// only ever reached from Run's dispatch, after Run has assigned it
	// (happens-before the library's handler goroutines), so no lock is needed.
	bot *bot.Bot
}

// New returns a Telegram adapter for cfg. callbacks is the pending-action store
// used to render and resolve buttons; pass nil for a text-only bot (Actions then
// degrade to an enumerated text list). It is a pure constructor: the Bot API
// client is built in Run, which owns the run lifecycle.
func New(cfg Config, callbacks pending.Store) *Adapter {
	return &Adapter{cfg: cfg, callbacks: callbacks}
}

// Name identifies this adapter.
func (a *Adapter) Name() string { return "telegram" }

// Supports reports Telegram's optional capabilities. Reactions and edits are
// always available; buttons need the callback store, without which Actions
// degrade to text.
func (a *Adapter) Supports(c transport.Capability) bool {
	switch c {
	case transport.CapReactions, transport.CapEditMessage:
		return true
	case transport.CapButtons:
		return a.callbacks != nil
	default:
		return false
	}
}

// Run long-polls Telegram and dispatches each update to handler until ctx is
// cancelled. The library owns the update loop — offset acking and a clean stop
// on ctx.Done. A text message flows through toInbound -> handler -> Send; a
// button click flows through the callback path.
func (a *Adapter) Run(ctx context.Context, handler transport.Handler) error {
	dispatch := func(ctx context.Context, _ *bot.Bot, u *models.Update) {
		switch {
		case u.CallbackQuery != nil:
			a.handleCallback(ctx, handler, u.CallbackQuery)
		case u.Message != nil:
			in, ok := a.toInbound(u.Message)
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

// Send delivers reply to the chat identified by replyTo. A silent reply, or an
// empty one with no actions, sends nothing (the gate's Silent -> no-send
// contract, ADR 0007). Actions render as an inline keyboard; without a callback
// store they degrade to a text list appended to the message.
func (a *Adapter) Send(ctx context.Context, replyTo string, reply core.Reply) error {
	if reply.Silent {
		return nil
	}
	if strings.TrimSpace(reply.Text) == "" && len(reply.Actions) == 0 {
		return nil
	}
	if a.bot == nil {
		return errors.New("telegram: transport not running")
	}
	chatID, err := strconv.ParseInt(replyTo, 10, 64)
	if err != nil {
		return errors.New("telegram: invalid chat id " + strconv.Quote(replyTo))
	}

	params := &bot.SendMessageParams{ChatID: chatID, Text: reply.Text}
	if len(reply.Actions) > 0 {
		if markup, err := a.renderActions(ctx, reply.Actions); err == nil {
			params.ReplyMarkup = markup
		} else {
			// Can't mint tokens — surface the actions as text rather than drop them.
			slog.Warn("telegram: rendering actions as text", "err", a.redact(err))
			params.Text = reply.Text + transport.ActionsAsText(reply.Actions)
		}
	}
	if _, err := a.bot.SendMessage(ctx, params); err != nil {
		return a.redact(err)
	}
	return nil
}

// renderActions mints a token per action (stored server-side) and lays the
// buttons out one per row, carrying only the token in callback_data.
func (a *Adapter) renderActions(ctx context.Context, actions []core.Action) (models.InlineKeyboardMarkup, error) {
	if a.callbacks == nil {
		return models.InlineKeyboardMarkup{}, errors.New("telegram: no callback store")
	}
	rows := make([][]models.InlineKeyboardButton, 0, len(actions))
	for _, act := range actions {
		token, err := a.callbacks.Put(ctx, act)
		if err != nil {
			return models.InlineKeyboardMarkup{}, err
		}
		rows = append(rows, []models.InlineKeyboardButton{{Text: act.Label, CallbackData: token}})
	}
	return models.InlineKeyboardMarkup{InlineKeyboard: rows}, nil
}

// handleCallback turns a button click into a callback inbound, runs the handler,
// and answers the query with the handler's Notice (the toast). A click that
// can't be mapped still gets an empty answer so the client's spinner clears.
func (a *Adapter) handleCallback(ctx context.Context, handler transport.Handler, cq *models.CallbackQuery) {
	in, ok := a.toCallbackInbound(cq)
	if !ok {
		_ = a.answerCallback(ctx, cq.ID, "")
		return
	}
	reply, err := handler(ctx, in)
	if err != nil {
		slog.Error("telegram callback handler failed", "err", err)
		_ = a.answerCallback(ctx, in.Callback.QueryID, "")
		return
	}
	// The toast is the source of truth — answer it first.
	if err := a.answerCallback(ctx, in.Callback.QueryID, reply.Notice); err != nil {
		slog.Error("telegram answerCallbackQuery failed", "err", a.redact(err))
	}
	// Best-effort status edit: replace the message and drop its keyboard. The
	// effect already committed, so an edit failure is logged, not surfaced — it
	// must never imply the action didn't take (ADR 0009).
	if strings.TrimSpace(reply.Text) != "" {
		if err := a.editToStatus(ctx, in.ReplyTo, in.Callback.MessageID, reply.Text); err != nil {
			slog.Warn("telegram status edit failed (action already committed)", "err", a.redact(err))
		}
	}
}

// editToStatus replaces a message's text with a status line and removes its
// inline keyboard (omitting ReplyMarkup clears it).
func (a *Adapter) editToStatus(ctx context.Context, chatID, messageID, text string) error {
	chat, err := strconv.ParseInt(chatID, 10, 64)
	if err != nil {
		return errors.New("telegram: invalid chat id " + strconv.Quote(chatID))
	}
	msg, err := strconv.Atoi(messageID)
	if err != nil {
		return errors.New("telegram: invalid message id " + strconv.Quote(messageID))
	}
	if _, err := a.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
		ChatID:    chat,
		MessageID: msg,
		Text:      text,
	}); err != nil {
		return a.redact(err)
	}
	return nil
}

// answerCallback acknowledges a click, optionally showing text as a toast.
func (a *Adapter) answerCallback(ctx context.Context, queryID, text string) error {
	if _, err := a.bot.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{
		CallbackQueryID: queryID,
		Text:            text,
	}); err != nil {
		return a.redact(err)
	}
	return nil
}

// toInbound maps a Telegram message to a transport.Inbound, or ok=false to drop
// it: a non-text message, or a group not in AllowedGroups.
func (a *Adapter) toInbound(m *models.Message) (transport.Inbound, bool) {
	if m == nil || m.From == nil || strings.TrimSpace(m.Text) == "" {
		return transport.Inbound{}, false
	}
	surface, ok := a.surfaceFor(m.Chat)
	if !ok {
		return transport.Inbound{}, false
	}
	return transport.Inbound{
		User:    userOf(m.From),
		Surface: surface,
		Text:    m.Text,
		ReplyTo: strconv.FormatInt(m.Chat.ID, 10),
	}, true
}

// toCallbackInbound maps a callback_query to a callback inbound, or ok=false to
// drop it: no token, an undeterminable chat, or a group not in AllowedGroups.
func (a *Adapter) toCallbackInbound(cq *models.CallbackQuery) (transport.Inbound, bool) {
	if cq.Data == "" {
		return transport.Inbound{}, false
	}
	chat, messageID, ok := callbackMessage(cq)
	if !ok {
		return transport.Inbound{}, false
	}
	surface, ok := a.surfaceFor(chat)
	if !ok {
		return transport.Inbound{}, false
	}
	from := cq.From
	return transport.Inbound{
		User:    userOf(&from),
		Surface: surface,
		ReplyTo: strconv.FormatInt(chat.ID, 10),
		Callback: &transport.Callback{
			Token:     cq.Data,
			QueryID:   cq.ID,
			MessageID: strconv.Itoa(messageID),
		},
	}, true
}

// surfaceFor maps a chat to a surface, applying the group allow-list. A channel
// or a non-allowed group yields ok=false (dropped).
func (a *Adapter) surfaceFor(chat models.Chat) (core.Surface, bool) {
	switch chat.Type {
	case models.ChatTypePrivate:
		return core.SurfaceDM, true
	case models.ChatTypeGroup, models.ChatTypeSupergroup:
		if !a.groupAllowed(chat.ID) {
			return 0, false
		}
		return core.SurfaceGroup, true
	default:
		return 0, false
	}
}

// callbackMessage extracts the chat and message id a callback is attached to,
// from either the accessible message or the inaccessible stub.
func callbackMessage(cq *models.CallbackQuery) (models.Chat, int, bool) {
	switch {
	case cq.Message.Message != nil:
		return cq.Message.Message.Chat, cq.Message.Message.ID, true
	case cq.Message.InaccessibleMessage != nil:
		im := cq.Message.InaccessibleMessage
		return im.Chat, im.MessageID, true
	default:
		return models.Chat{}, 0, false
	}
}

// userOf builds a platform-neutral User; Display falls back to the first name.
func userOf(u *models.User) core.User {
	display := u.Username
	if display == "" {
		display = u.FirstName
	}
	return core.User{ID: strconv.FormatInt(u.ID, 10), Display: display}
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
