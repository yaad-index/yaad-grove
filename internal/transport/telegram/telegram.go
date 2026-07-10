// Package telegram is the Phase-1 transport adapter. It is one pluggable
// implementation of transport.Transport and the only platform wired at first;
// Discord and Slack adapters follow the same boundary later (ADR 0001,
// roadmap). Nothing here leaks into the core.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

const (
	defaultBaseURL  = "https://api.telegram.org"
	longPollSeconds = 30
	maxBackoff      = 30 * time.Second
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
	cfg     Config
	http    *http.Client
	baseURL string
}

// New returns a Telegram adapter for cfg.
func New(cfg Config) *Adapter {
	return &Adapter{
		cfg:     cfg,
		http:    &http.Client{Timeout: (longPollSeconds + 15) * time.Second},
		baseURL: defaultBaseURL,
	}
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

type tgUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	First    string `json:"first_name"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type tgMessage struct {
	MessageID int64   `json:"message_id"`
	From      *tgUser `json:"from"`
	Chat      tgChat  `json:"chat"`
	Text      string  `json:"text"`
}

type update struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

type getUpdatesResponse struct {
	OK          bool     `json:"ok"`
	Result      []update `json:"result"`
	Description string   `json:"description"`
}

// Run long-polls Telegram and dispatches each message to handler until ctx is
// cancelled. It tracks the update offset across the loop (each batch acks the
// previous), ctx-cancels the poll, and backs off on a transient error.
func (a *Adapter) Run(ctx context.Context, handler transport.Handler) error {
	var offset int64
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		updates, err := a.getUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("telegram getUpdates failed; backing off", "err", err, "backoff", backoff.String())
			if !sleepCtx(ctx, backoff) {
				return ctx.Err()
			}
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		backoff = time.Second
		for _, u := range updates {
			offset = u.UpdateID + 1
			in, ok := a.toInbound(u)
			if !ok {
				continue
			}
			reply, herr := handler(ctx, in)
			if herr != nil {
				slog.Error("telegram handler failed", "err", herr)
				continue
			}
			if serr := a.Send(ctx, in.ReplyTo, reply); serr != nil {
				slog.Error("telegram send failed", "err", serr)
			}
		}
	}
}

// Send delivers reply to the chat identified by replyTo (its opaque chat id). A
// silent or empty reply sends nothing — matching the gate's Silent -> no-send
// contract (ADR 0007).
func (a *Adapter) Send(ctx context.Context, replyTo string, reply core.Reply) error {
	if reply.Silent || strings.TrimSpace(reply.Text) == "" {
		return nil
	}
	chatID, err := strconv.ParseInt(replyTo, 10, 64)
	if err != nil {
		return fmt.Errorf("telegram: invalid chat id %q: %w", replyTo, err)
	}
	body, err := json.Marshal(map[string]any{"chat_id": chatID, "text": reply.Text})
	if err != nil {
		return fmt.Errorf("telegram: marshal sendMessage: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.method("sendMessage"), bytes.NewReader(body))
	if err != nil {
		return a.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return a.redact(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram: sendMessage status %d", resp.StatusCode)
	}
	return nil
}

// getUpdates long-polls for updates newer than offset.
func (a *Adapter) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	url := fmt.Sprintf("%s?offset=%d&timeout=%d", a.method("getUpdates"), offset, longPollSeconds)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, a.redact(err)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, a.redact(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("telegram: getUpdates status %d", resp.StatusCode)
	}
	var out getUpdatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("telegram: decode getUpdates: %w", err)
	}
	if !out.OK {
		return nil, fmt.Errorf("telegram: getUpdates not ok: %s", out.Description)
	}
	return out.Result, nil
}

// toInbound maps a Telegram update to a transport.Inbound, or ok=false to drop it
// (a non-text or edited update, a channel post, or a group not in AllowedGroups).
func (a *Adapter) toInbound(u update) (transport.Inbound, bool) {
	m := u.Message
	if m == nil || m.From == nil || strings.TrimSpace(m.Text) == "" {
		return transport.Inbound{}, false
	}
	var surface core.Surface
	switch m.Chat.Type {
	case "private":
		surface = core.SurfaceDM
	case "group", "supergroup":
		surface = core.SurfaceGroup
		if !a.groupAllowed(m.Chat.ID) {
			return transport.Inbound{}, false
		}
	default:
		return transport.Inbound{}, false
	}
	display := m.From.Username
	if display == "" {
		display = m.From.First
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

// method builds a Bot API method URL. The token lives in the path (as the API
// requires), so callers must never log a raw URL or an unredacted transport
// error — see redact.
func (a *Adapter) method(name string) string {
	return fmt.Sprintf("%s/bot%s/%s", a.baseURL, a.cfg.Token, name)
}

// redact strips the bot token from an error's text — net/http errors embed the
// request URL, which carries the token — so it never reaches a log or a caller.
func (a *Adapter) redact(err error) error {
	if err == nil || a.cfg.Token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), a.cfg.Token, "«token»"))
}

// sleepCtx sleeps for d or until ctx is done; it returns false if ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// compile-time assertion that Adapter satisfies transport.Transport.
var _ transport.Transport = (*Adapter)(nil)
