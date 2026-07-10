package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

func msg(chatID int64, chatType, text string, fromID int64, username string) update {
	return update{UpdateID: 1, Message: &tgMessage{
		From: &tgUser{ID: fromID, Username: username},
		Chat: tgChat{ID: chatID, Type: chatType},
		Text: text,
	}}
}

// Update -> Inbound: private is a DM, an allowed group is a Group, a non-allowed
// group / empty text / non-message is dropped.
func TestToInbound(t *testing.T) {
	a := New(Config{AllowedGroups: []string{"999"}})

	in, ok := a.toInbound(msg(555, "private", "hi", 42, "alice"))
	require.True(t, ok)
	assert.Equal(t, core.SurfaceDM, in.Surface)
	assert.Equal(t, "42", in.User.ID)
	assert.Equal(t, "alice", in.User.Display)
	assert.Equal(t, "hi", in.Text)
	assert.Equal(t, "555", in.ReplyTo)

	in, ok = a.toInbound(msg(999, "supergroup", "yo", 7, "bob"))
	require.True(t, ok, "allowed group is served")
	assert.Equal(t, core.SurfaceGroup, in.Surface)
	assert.Equal(t, "999", in.ReplyTo)

	_, ok = a.toInbound(msg(111, "group", "yo", 7, "bob"))
	assert.False(t, ok, "a group not in AllowedGroups is dropped")

	_, ok = a.toInbound(msg(555, "private", "   ", 42, "alice"))
	assert.False(t, ok, "empty text is dropped")

	_, ok = a.toInbound(update{UpdateID: 2, Message: nil})
	assert.False(t, ok, "a non-message update is dropped")

	_, ok = a.toInbound(msg(555, "channel", "post", 42, ""))
	assert.False(t, ok, "a channel post is dropped")
}

// An empty allow-list serves no groups (safe default); a DM is unaffected by it.
func TestGroupAllowedDefault(t *testing.T) {
	a := New(Config{}) // no AllowedGroups
	_, ok := a.toInbound(msg(123, "group", "hi", 1, "u"))
	assert.False(t, ok, "empty allow-list serves no groups")
	_, ok = a.toInbound(msg(123, "private", "hi", 1, "u"))
	assert.True(t, ok, "DMs are not gated by AllowedGroups")
}

// A silent or empty reply makes no HTTP call; a normal reply posts chat_id + text.
func TestSend(t *testing.T) {
	var called bool
	var gotChatID int64
	var gotText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		b, _ := io.ReadAll(r.Body)
		var m struct {
			ChatID int64  `json:"chat_id"`
			Text   string `json:"text"`
		}
		_ = json.Unmarshal(b, &m)
		gotChatID, gotText = m.ChatID, m.Text
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()
	a := New(Config{Token: "tok"})
	a.baseURL = srv.URL

	require.NoError(t, a.Send(context.Background(), "555", core.Reply{Silent: true}))
	require.NoError(t, a.Send(context.Background(), "555", core.Reply{Text: "  "}))
	assert.False(t, called, "silent/empty reply makes no HTTP call")

	require.NoError(t, a.Send(context.Background(), "555", core.Reply{Text: "hello back"}))
	assert.True(t, called)
	assert.Equal(t, int64(555), gotChatID)
	assert.Equal(t, "hello back", gotText)
}

// Run: getUpdates -> Inbound -> handler -> Send end to end, and the offset
// advances (the next poll acks the processed update). Everything is signalled
// through channels so the test is race-free under -race.
func TestRunProcessesAndAdvancesOffset(t *testing.T) {
	inboundCh := make(chan transport.Inbound, 1)
	sentCh := make(chan string, 1)
	offsetCh := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "getUpdates"):
			off := r.URL.Query().Get("offset")
			if off == "" || off == "0" {
				_, _ = w.Write([]byte(`{"ok":true,"result":[{"update_id":100,"message":` +
					`{"message_id":1,"from":{"id":42,"username":"alice"},"chat":{"id":555,"type":"private"},"text":"hi"}}]}`))
				return
			}
			select {
			case offsetCh <- off:
			default:
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":[]}`))
		case strings.Contains(r.URL.Path, "sendMessage"):
			b, _ := io.ReadAll(r.Body)
			var m struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(b, &m)
			_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
			select {
			case sentCh <- m.Text:
			default:
			}
		}
	}))
	defer srv.Close()

	a := New(Config{Token: "tok"})
	a.baseURL = srv.URL
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
		assert.Equal(t, "555", in.ReplyTo)
	case <-time.After(2 * time.Second):
		t.Fatal("handler not called within timeout")
	}
	select {
	case text := <-sentCh:
		assert.Equal(t, "hello back", text)
	case <-time.After(2 * time.Second):
		t.Fatal("no send within timeout")
	}
	select {
	case off := <-offsetCh:
		assert.Equal(t, "101", off, "the next poll acks the processed update (offset advanced)")
	case <-time.After(2 * time.Second):
		t.Fatal("offset did not advance within timeout")
	}
}

// The bot token never appears in a transport error (it lives in the URL path).
func TestRedactStripsToken(t *testing.T) {
	a := New(Config{Token: "super-secret-token"})
	// Point at an unroutable base so http.Client.Do fails with a url.Error carrying
	// the URL (and thus the token) — redact must scrub it.
	a.baseURL = "http://127.0.0.1:0"
	_, err := a.getUpdates(context.Background(), 0)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "super-secret-token")
}
