package model_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/bonyan/model/chatcompat"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/model"
	"github.com/yaad-index/yaad-grove/internal/tools"
)

var updateWire = flag.Bool("update-wire", false, "update the wire request golden file")

type wireRetriever []core.Chunk

func (r wireRetriever) Retrieve(context.Context, string) ([]core.Chunk, error) { return r, nil }

type wireTools struct{}

func (wireTools) Defs() []core.ToolDef {
	return []core.ToolDef{{Name: "search", Description: "search transcripts", Schema: json.RawMessage(`{"type":"object"}`)}}
}

func (wireTools) Call(context.Context, string, map[string]any) (string, error) { return "", nil }

// The request body the chat-completions client sends for a full query, as the
// deployed binary wires the model: persona, scope, retrieved chunks, a reply,
// recent conversation, the asker's name and a tool. Regenerate with
// `go test ./internal/model -run TestWireRequestGolden -update-wire` after an
// intended change.
func TestWireRequestGolden(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		body, err = io.ReadAll(r.Body)
		assert.NoError(t, err)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	chat, err := chatcompat.New(chatcompat.Options{BaseURL: srv.URL, Model: "wire-model"})
	require.NoError(t, err)

	tm := time.Date(2026, 7, 11, 9, 30, 0, 0, time.UTC)
	retriever := wireRetriever{
		{Source: "notes/a.md#Intro", Text: "The widget installs via the script."},
		{Source: "faq.md", Text: "Reset anytime."},
	}
	engine := core.New(model.NativeToolCalls(chat), "wire-model", retriever, tools.ForAgent(wireTools{}, nil), "You answer about the widget.", core.WithPersona("You are Grove."))
	reply, err := engine.Answer(context.Background(), core.Query{
		Text:         "how do I install it?",
		User:         core.User{Display: "Ada"},
		History:      []core.HistoryTurn{{Speaker: "Al", Text: "is it hard?", Time: tm, MessageID: "m1"}, {Bot: true, Text: "Not at all.", Time: tm.Add(time.Minute), MessageID: "m2", ReplyTo: "m1"}},
		ReplyContext: "Al: is it hard?",
	})
	require.NoError(t, err)
	require.Equal(t, "ok", reply.Text)

	var got bytes.Buffer
	require.NoError(t, json.Indent(&got, body, "", "  "))
	got.WriteString("\n")
	golden := filepath.Join("testdata", "request_wire.golden")
	if *updateWire {
		require.NoError(t, os.MkdirAll("testdata", 0o750))
		require.NoError(t, os.WriteFile(golden, got.Bytes(), 0o600))
		return
	}
	want, err := os.ReadFile(golden)
	require.NoError(t, err, "missing golden — run with -update-wire")
	assert.Equal(t, string(want), got.String())
}
