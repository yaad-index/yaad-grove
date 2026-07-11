package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/pending"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

var echoAction = core.Action{Verb: "echo", Params: map[string]string{"say": "hi"}, Label: "Echo"}

func msg(chatID int64, chatType models.ChatType, text string, fromID int64, username string) *models.Message {
	return &models.Message{
		From: &models.User{ID: fromID, Username: username},
		Chat: models.Chat{ID: chatID, Type: chatType},
		Text: text,
	}
}

// running attaches a live library client pointed at url, as Run would, so Send /
// handleCallback can be exercised directly without the long-poll loop.
func running(t *testing.T, a *Adapter, url string) {
	t.Helper()
	b, err := bot.New(a.cfg.Token, bot.WithServerURL(url), bot.WithSkipGetMe())
	require.NoError(t, err)
	a.bot = b
}

// Message -> Inbound: private is a DM, an allowed group is a Group, a non-allowed
// group / empty text / channel / nil is dropped.
func TestToInbound(t *testing.T) {
	a := New(Config{AllowedGroups: []string{"999"}}, nil)

	in, ok := a.toInbound(msg(555, models.ChatTypePrivate, "hi", 42, "alice"))
	require.True(t, ok)
	assert.Equal(t, core.SurfaceDM, in.Surface)
	assert.Equal(t, "42", in.User.ID)
	assert.Equal(t, "alice", in.User.Display)
	assert.Equal(t, "hi", in.Text)
	assert.Equal(t, "555", in.ReplyTo)
	assert.Nil(t, in.Callback, "a text message carries no callback")

	in, ok = a.toInbound(msg(999, models.ChatTypeSupergroup, "yo", 7, "bob"))
	require.True(t, ok, "allowed group is served")
	assert.Equal(t, core.SurfaceGroup, in.Surface)
	assert.Equal(t, "999", in.ReplyTo)

	_, ok = a.toInbound(msg(111, models.ChatTypeGroup, "yo", 7, "bob"))
	assert.False(t, ok, "a group not in AllowedGroups is dropped")

	_, ok = a.toInbound(msg(555, models.ChatTypePrivate, "   ", 42, "alice"))
	assert.False(t, ok, "empty text is dropped")

	_, ok = a.toInbound(msg(555, models.ChatTypeChannel, "post", 42, ""))
	assert.False(t, ok, "a channel post is dropped")

	_, ok = a.toInbound(nil)
	assert.False(t, ok, "a nil message is dropped")
}

// The Display falls back to the first name when there is no username.
func TestToInboundDisplayFallback(t *testing.T) {
	a := New(Config{}, nil)
	m := msg(555, models.ChatTypePrivate, "hi", 42, "")
	m.From.FirstName = "Alice"
	in, ok := a.toInbound(m)
	require.True(t, ok)
	assert.Equal(t, "Alice", in.User.Display)
}

// An empty allow-list serves no groups (safe default); a DM is unaffected.
func TestGroupAllowedDefault(t *testing.T) {
	a := New(Config{}, nil)
	_, ok := a.toInbound(msg(123, models.ChatTypeGroup, "hi", 1, "u"))
	assert.False(t, ok, "empty allow-list serves no groups")
	_, ok = a.toInbound(msg(123, models.ChatTypePrivate, "hi", 1, "u"))
	assert.True(t, ok, "DMs are not gated by AllowedGroups")
}

// CapButtons tracks whether a callback store is present; the others are constant.
func TestSupportsCapButtons(t *testing.T) {
	assert.False(t, New(Config{}, nil).Supports(transport.CapButtons))
	assert.True(t, New(Config{}, pending.NewMemoryStore(time.Minute)).Supports(transport.CapButtons))
	assert.True(t, New(Config{}, nil).Supports(transport.CapReactions))
}

// A silent or empty reply sends nothing (and needs no running client); a real
// reply before Run surfaces an error rather than a nil-client panic.
func TestSendSilentAndEmptyDoNotSend(t *testing.T) {
	a := New(Config{Token: "tok"}, nil)
	require.NoError(t, a.Send(context.Background(), "555", core.Reply{Silent: true}))
	require.NoError(t, a.Send(context.Background(), "555", core.Reply{Text: "   "}))
	assert.Error(t, a.Send(context.Background(), "555", core.Reply{Text: "hi"}),
		"a real reply with no running transport is an error, not a panic")
}

// react calls setMessageReaction on the triggering message with a single emoji
// reaction (the reaction-mode consent nudge, ADR 0012).
func TestReactSetsMessageReaction(t *testing.T) {
	var gotChat, gotMsg, gotReaction string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/setMessageReaction") {
			gotChat = r.FormValue("chat_id")
			gotMsg = r.FormValue("message_id")
			gotReaction = r.FormValue("reaction")
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	defer srv.Close()

	a := New(Config{Token: "tok"}, nil)
	running(t, a, srv.URL)

	require.NoError(t, a.react(context.Background(), 555, 42, "🤝"))
	assert.Equal(t, "555", gotChat)
	assert.Equal(t, "42", gotMsg)
	assert.Contains(t, gotReaction, "🤝", "the emoji rides in the reaction list")
	assert.Contains(t, gotReaction, "emoji", "encoded as an emoji reaction type")
}

// A reaction before Run surfaces an error rather than a nil-client panic.
func TestReactWithoutRunningTransportErrors(t *testing.T) {
	a := New(Config{Token: "tok"}, nil)
	assert.Error(t, a.react(context.Background(), 555, 42, "🤝"))
}

// Send renders Actions as an inline keyboard whose callback_data is a token that
// resolves to the action in the store.
func TestSendRendersActionsAsKeyboard(t *testing.T) {
	var replyMarkup string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			replyMarkup = r.FormValue("reply_markup")
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":555,"type":"private"},"text":"ok"}}`)
	}))
	defer srv.Close()

	store := pending.NewMemoryStore(time.Minute)
	a := New(Config{Token: "tok"}, store)
	running(t, a, srv.URL)

	require.NoError(t, a.Send(context.Background(), "555",
		core.Reply{Text: "pick one", Actions: []core.Action{echoAction}}))

	var markup struct {
		InlineKeyboard [][]struct {
			Text         string `json:"text"`
			CallbackData string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	require.NoError(t, json.Unmarshal([]byte(replyMarkup), &markup))
	require.Len(t, markup.InlineKeyboard, 1)
	require.Len(t, markup.InlineKeyboard[0], 1)
	btn := markup.InlineKeyboard[0][0]
	assert.Equal(t, "Echo", btn.Text)
	require.NotEmpty(t, btn.CallbackData, "the button carries a token")

	// The token resolves to the stored action — render and store agree.
	got, status, err := store.Resolve(context.Background(), btn.CallbackData)
	require.NoError(t, err)
	assert.Equal(t, pending.Resolved, status)
	assert.Equal(t, echoAction, got)
}

// Without a callback store, Actions degrade to an enumerated text list appended
// to the message, and no inline keyboard is sent.
func TestSendActionsTextFallbackWithoutStore(t *testing.T) {
	var text, replyMarkup string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			text = r.FormValue("text")
			replyMarkup = r.FormValue("reply_markup")
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"chat":{"id":555,"type":"private"},"text":"ok"}}`)
	}))
	defer srv.Close()

	a := New(Config{Token: "tok"}, nil) // no store
	running(t, a, srv.URL)

	require.NoError(t, a.Send(context.Background(), "555",
		core.Reply{Text: "pick one", Actions: []core.Action{echoAction}}))

	assert.Empty(t, replyMarkup, "no inline keyboard without a store")
	assert.Contains(t, text, "pick one")
	assert.Contains(t, text, "1. Echo", "actions appended as a text list")
}

// A button click round-trips: the callback inbound carries the token, query id,
// message handle, and the acting subject; the handler's Notice becomes the toast.
func TestCallbackResolvesAndAnswers(t *testing.T) {
	var answeredID, answeredText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			answeredID = r.FormValue("callback_query_id")
			answeredText = r.FormValue("text")
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	defer srv.Close()

	a := New(Config{Token: "tok", AllowedGroups: nil}, pending.NewMemoryStore(time.Minute))
	running(t, a, srv.URL)

	var gotIn transport.Inbound
	handler := func(_ context.Context, in transport.Inbound) (core.Reply, error) {
		gotIn = in
		return core.Reply{Notice: "echo done"}, nil
	}

	cq := &models.CallbackQuery{
		ID:   "cq1",
		From: models.User{ID: 42, Username: "alice"},
		Message: models.MaybeInaccessibleMessage{
			Message: &models.Message{ID: 5, Chat: models.Chat{ID: 555, Type: models.ChatTypePrivate}},
		},
		Data: "tok123",
	}
	a.handleCallback(context.Background(), handler, cq)

	require.NotNil(t, gotIn.Callback, "a click yields a callback inbound")
	assert.Equal(t, "tok123", gotIn.Callback.Token)
	assert.Equal(t, "cq1", gotIn.Callback.QueryID)
	assert.Equal(t, "5", gotIn.Callback.MessageID)
	assert.Equal(t, "42", gotIn.User.ID, "the acting subject rides in User")
	assert.Equal(t, core.SurfaceDM, gotIn.Surface)
	assert.Equal(t, "555", gotIn.ReplyTo)

	assert.Equal(t, "cq1", answeredID)
	assert.Equal(t, "echo done", answeredText, "the handler's Notice is the toast")
}

// A callback reply carrying Text edits the originating message to that status
// line (best-effort, after answering the toast); an empty Text edits nothing.
func TestCallbackEditsMessageOnStatus(t *testing.T) {
	var answered, edited bool
	var editChat, editText string
	var editMsgID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/answerCallbackQuery"):
			answered = true
		case strings.HasSuffix(r.URL.Path, "/editMessageText"):
			edited = true
			editChat = r.FormValue("chat_id")
			editMsgID = r.FormValue("message_id")
			editText = r.FormValue("text")
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	defer srv.Close()

	a := New(Config{Token: "tok"}, pending.NewMemoryStore(time.Minute))
	running(t, a, srv.URL)

	cq := &models.CallbackQuery{
		ID:      "cq1",
		From:    models.User{ID: 42},
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: 7, Chat: models.Chat{ID: 555, Type: models.ChatTypePrivate}}},
		Data:    "tok",
	}

	// Reply with a status line -> message edited to it.
	a.handleCallback(context.Background(), func(context.Context, transport.Inbound) (core.Reply, error) {
		return core.Reply{Notice: "Done ✓", Text: "Set target to trusted."}, nil
	}, cq)
	assert.True(t, answered)
	require.True(t, edited, "a status line edits the message")
	assert.Equal(t, "555", editChat)
	assert.Equal(t, "7", editMsgID)
	assert.Equal(t, "Set target to trusted.", editText)

	// Reply with no status line -> no edit.
	edited = false
	a.handleCallback(context.Background(), func(context.Context, transport.Inbound) (core.Reply, error) {
		return core.Reply{Notice: "Done ✓"}, nil
	}, cq)
	assert.False(t, edited, "no status line means no edit")
}

// A callback from a non-allowed group is dropped, but the click is still
// acknowledged (empty answer) so the client spinner clears.
func TestCallbackNonAllowedGroupDropped(t *testing.T) {
	var answered bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/answerCallbackQuery") {
			answered = true
		}
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
	}))
	defer srv.Close()

	a := New(Config{Token: "tok"}, pending.NewMemoryStore(time.Minute)) // empty allow-list
	running(t, a, srv.URL)

	called := false
	handler := func(_ context.Context, _ transport.Inbound) (core.Reply, error) {
		called = true
		return core.Reply{}, nil
	}
	cq := &models.CallbackQuery{
		ID:      "cq2",
		From:    models.User{ID: 42},
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{ID: 5, Chat: models.Chat{ID: 111, Type: models.ChatTypeGroup}}},
		Data:    "tok",
	}
	a.handleCallback(context.Background(), handler, cq)

	assert.False(t, called, "handler not reached for a disallowed-group click")
	assert.True(t, answered, "the click is still acknowledged")
}

// Run: an incoming message flows through toInbound -> handler -> Send end to end
// against a mock Bot API server. Race-free via channels.
func TestRunDeliversAndReplies(t *testing.T) {
	inboundCh := make(chan transport.Inbound, 1)
	sentCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			offset, _ := strconv.ParseInt(r.FormValue("offset"), 10, 64)
			if offset <= 100 {
				_, _ = io.WriteString(w, `{"ok":true,"result":[{"update_id":100,"message":`+
					`{"message_id":1,"from":{"id":42,"username":"alice"},"chat":{"id":555,"type":"private"},"text":"hi"}}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		case strings.HasSuffix(r.URL.Path, "/sendMessage"):
			text := r.FormValue("text")
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":2,"chat":{"id":555,"type":"private"},"text":"ok"}}`)
			select {
			case sentCh <- text:
			default:
			}
		default:
			_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
		}
	}))
	defer srv.Close()

	a := New(Config{Token: "tok"}, nil)
	a.serverURL = srv.URL
	a.skipGetMe = true
	handler := func(_ context.Context, in transport.Inbound) (core.Reply, error) {
		select {
		case inboundCh <- in:
		default:
		}
		return core.Reply{Text: "hello back"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Run(ctx, handler) }()

	select {
	case in := <-inboundCh:
		assert.Equal(t, "42", in.User.ID)
		assert.Equal(t, core.SurfaceDM, in.Surface)
		assert.Equal(t, "hi", in.Text)
	case <-time.After(3 * time.Second):
		t.Fatal("handler not called within timeout")
	}
	select {
	case text := <-sentCh:
		assert.Equal(t, "hello back", text)
	case <-time.After(3 * time.Second):
		t.Fatal("no send within timeout")
	}
}

// On startup Run drops the pre-online backlog: it calls deleteWebhook with
// drop_pending_updates=true before polling, so messages queued while the bot was
// offline aren't processed.
func TestRunDropsPendingBacklog(t *testing.T) {
	dropped := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/deleteWebhook"):
			select {
			case dropped <- r.FormValue("drop_pending_updates"):
			default:
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		default: // getUpdates and anything else: nothing pending
			_, _ = io.WriteString(w, `{"ok":true,"result":[]}`)
		}
	}))
	defer srv.Close()

	a := New(Config{Token: "tok"}, nil)
	a.serverURL = srv.URL
	a.skipGetMe = true
	handler := func(context.Context, transport.Inbound) (core.Reply, error) { return core.Reply{}, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Run(ctx, handler) }()

	select {
	case v := <-dropped:
		assert.Equal(t, "true", v, "startup drops the pending backlog")
	case <-time.After(3 * time.Second):
		t.Fatal("deleteWebhook(drop_pending_updates) was not called on startup")
	}
}

// Directed detection (ADR 0012): a reply to the bot, or an @mention / text_mention
// of it, is directed; ambient chatter is not. @mention offsets are UTF-16, so a
// mention mid-message (after an emoji) resolves correctly.
func TestIsDirected(t *testing.T) {
	a := New(Config{}, nil)
	a.botID = 100
	a.botUsername = "grovebot"

	assert.True(t, a.isDirected(&models.Message{Text: "thanks",
		ReplyToMessage: &models.Message{From: &models.User{ID: 100}}}), "reply to the bot is directed")
	assert.False(t, a.isDirected(&models.Message{Text: "thanks",
		ReplyToMessage: &models.Message{From: &models.User{ID: 200}}}), "reply to someone else is not")

	assert.True(t, a.isDirected(&models.Message{Text: "@grovebot help",
		Entities: []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 9}}}),
		"leading @mention of the bot is directed")

	// "🤝 @grovebot hi": the emoji is 2 UTF-16 units, so @grovebot starts at unit 3.
	assert.True(t, a.isDirected(&models.Message{Text: "🤝 @grovebot hi",
		Entities: []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 3, Length: 9}}}),
		"mid-message @mention after an emoji is directed (UTF-16 offset)")

	assert.False(t, a.isDirected(&models.Message{Text: "@otherbot help",
		Entities: []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 9}}}),
		"a mention of a different handle is not directed")

	assert.True(t, a.isDirected(&models.Message{Text: "hey there",
		Entities: []models.MessageEntity{{Type: models.MessageEntityTypeTextMention, Offset: 0, Length: 3, User: &models.User{ID: 100}}}}),
		"a text_mention of the bot is directed")

	assert.False(t, a.isDirected(&models.Message{Text: "just chatting in the group"}),
		"ambient chatter is not directed")
}

// toInbound sets Directed: a DM is always directed; a group message is directed
// only when aimed at the bot.
func TestToInboundDirected(t *testing.T) {
	a := New(Config{AllowedGroups: []string{"999"}}, nil)
	a.botID = 100
	a.botUsername = "grovebot"

	in, ok := a.toInbound(msg(555, models.ChatTypePrivate, "hi", 42, "alice"))
	require.True(t, ok)
	assert.True(t, in.Directed, "a DM is always directed")

	in, ok = a.toInbound(msg(999, models.ChatTypeSupergroup, "just chatting", 7, "bob"))
	require.True(t, ok)
	assert.False(t, in.Directed, "ambient group chatter is not directed")

	in, ok = a.toInbound(&models.Message{
		From: &models.User{ID: 7}, Chat: models.Chat{ID: 999, Type: models.ChatTypeSupergroup},
		Text:     "@grovebot help",
		Entities: []models.MessageEntity{{Type: models.MessageEntityTypeMention, Offset: 0, Length: 9}},
	})
	require.True(t, ok)
	assert.True(t, in.Directed, "a group @mention is directed")
}

// The bot token never appears in an error surfaced from the transport.
func TestRedactStripsToken(t *testing.T) {
	a := New(Config{Token: "super-secret-token"}, nil)
	err := a.redact(errors.New(`Post "https://api.telegram.org/botsuper-secret-token/getUpdates": dial tcp: connection refused`))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "super-secret-token")
	assert.Contains(t, err.Error(), "«token»")
}
