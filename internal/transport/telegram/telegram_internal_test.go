package telegram

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

func upd(chatID int64, chatType models.ChatType, text string, fromID int64, username string) *models.Update {
	return &models.Update{ID: 1, Message: &models.Message{
		From: &models.User{ID: fromID, Username: username},
		Chat: models.Chat{ID: chatID, Type: chatType},
		Text: text,
	}}
}

// Update -> Inbound: private is a DM, an allowed group is a Group, a non-allowed
// group / empty text / non-message is dropped.
func TestToInbound(t *testing.T) {
	a := New(Config{AllowedGroups: []string{"999"}})

	in, ok := a.toInbound(upd(555, models.ChatTypePrivate, "hi", 42, "alice"))
	require.True(t, ok)
	assert.Equal(t, core.SurfaceDM, in.Surface)
	assert.Equal(t, "42", in.User.ID)
	assert.Equal(t, "alice", in.User.Display)
	assert.Equal(t, "hi", in.Text)
	assert.Equal(t, "555", in.ReplyTo)

	in, ok = a.toInbound(upd(999, models.ChatTypeSupergroup, "yo", 7, "bob"))
	require.True(t, ok, "allowed group is served")
	assert.Equal(t, core.SurfaceGroup, in.Surface)
	assert.Equal(t, "999", in.ReplyTo)

	_, ok = a.toInbound(upd(111, models.ChatTypeGroup, "yo", 7, "bob"))
	assert.False(t, ok, "a group not in AllowedGroups is dropped")

	_, ok = a.toInbound(upd(555, models.ChatTypePrivate, "   ", 42, "alice"))
	assert.False(t, ok, "empty text is dropped")

	_, ok = a.toInbound(upd(555, models.ChatTypeChannel, "post", 42, ""))
	assert.False(t, ok, "a channel post is dropped")

	_, ok = a.toInbound(&models.Update{ID: 2, Message: nil})
	assert.False(t, ok, "a non-message update (e.g. a callback) is dropped")
}

// The Display falls back to the first name when there is no username.
func TestToInboundDisplayFallback(t *testing.T) {
	a := New(Config{})
	u := upd(555, models.ChatTypePrivate, "hi", 42, "")
	u.Message.From.FirstName = "Alice"
	in, ok := a.toInbound(u)
	require.True(t, ok)
	assert.Equal(t, "Alice", in.User.Display)
}

// An empty allow-list serves no groups (safe default); a DM is unaffected by it.
func TestGroupAllowedDefault(t *testing.T) {
	a := New(Config{}) // no AllowedGroups
	_, ok := a.toInbound(upd(123, models.ChatTypeGroup, "hi", 1, "u"))
	assert.False(t, ok, "empty allow-list serves no groups")
	_, ok = a.toInbound(upd(123, models.ChatTypePrivate, "hi", 1, "u"))
	assert.True(t, ok, "DMs are not gated by AllowedGroups")
}

// A silent or empty reply sends nothing (and needs no running client); a real
// reply before Run surfaces an error rather than panicking on a nil client.
func TestSendSilentAndEmptyDoNotSend(t *testing.T) {
	a := New(Config{Token: "tok"})
	require.NoError(t, a.Send(context.Background(), "555", core.Reply{Silent: true}))
	require.NoError(t, a.Send(context.Background(), "555", core.Reply{Text: "   "}))
	assert.Error(t, a.Send(context.Background(), "555", core.Reply{Text: "hi"}),
		"a real reply with no running transport is an error, not a panic")
}

// Run: an incoming message flows through toInbound -> handler -> Send end to
// end against a mock Bot API server. The library owns offset acking; this
// asserts the round-trip. Everything is signalled through channels so the test
// is race-free under -race.
func TestRunDeliversAndReplies(t *testing.T) {
	inboundCh := make(chan transport.Inbound, 1)
	sentCh := make(chan string, 1)
	// The library encodes Bot API calls as multipart/form-data, so read params
	// via r.FormValue rather than parsing the body as JSON.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			offset, _ := strconv.ParseInt(r.FormValue("offset"), 10, 64)
			// Deliver the message once; the library advances its offset past it.
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

	a := New(Config{Token: "tok"})
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
		assert.Equal(t, "alice", in.User.Display)
		assert.Equal(t, core.SurfaceDM, in.Surface)
		assert.Equal(t, "hi", in.Text)
		assert.Equal(t, "555", in.ReplyTo)
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

// The bot token never appears in an error surfaced from the transport (the Bot
// API carries it in the request URL path).
func TestRedactStripsToken(t *testing.T) {
	a := New(Config{Token: "super-secret-token"})
	err := a.redact(errors.New(`Post "https://api.telegram.org/botsuper-secret-token/getUpdates": dial tcp: connection refused`))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "super-secret-token")
	assert.Contains(t, err.Error(), "«token»")
}
