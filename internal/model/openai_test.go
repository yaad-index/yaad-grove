package model_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/model"
)

// msgs builds a system+user conversation.
func msgs(system, user string) []core.Message {
	return []core.Message{
		{Role: core.RoleSystem, Content: system},
		{Role: core.RoleUser, Content: user},
	}
}

// A mocked 200 returns the choice text and surfaces token usage; the request
// carries the bearer key and the two messages in order.
func TestCompleteSuccess(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		assert.Equal(t, "/chat/completions", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi there"}}],` +
			`"usage":{"prompt_tokens":12,"completion_tokens":5,"total_tokens":17}}`))
	}))
	defer srv.Close()

	c := model.New(model.Config{BaseURL: srv.URL, APIKey: "sk-test-123", Model: "gpt-x"})
	res, err := c.Complete(context.Background(), msgs("be terse", "hello"), nil)
	require.NoError(t, err)
	assert.Equal(t, "hi there", res.Text)
	assert.Equal(t, 12, res.Usage.PromptTokens)
	assert.Equal(t, 5, res.Usage.CompletionTokens)
	assert.Equal(t, 17, res.Usage.TotalTokens)

	assert.Equal(t, "Bearer sk-test-123", gotAuth)
	var sent struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal([]byte(gotBody), &sent))
	assert.Equal(t, "gpt-x", sent.Model)
	require.Len(t, sent.Messages, 2)
	assert.Equal(t, "system", sent.Messages[0].Role)
	assert.Equal(t, "be terse", sent.Messages[0].Content)
	assert.Equal(t, "user", sent.Messages[1].Role)
	assert.Equal(t, "hello", sent.Messages[1].Content)
}

// A non-2xx response is a typed *StatusError carrying the status + a body snippet.
func TestCompleteNon2xxTypedError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"upstream boom"}`))
	}))
	defer srv.Close()

	c := model.New(model.Config{BaseURL: srv.URL, APIKey: "sk-x", Model: "m"})
	_, err := c.Complete(context.Background(), msgs("s", "u"), nil)
	require.Error(t, err)
	var se *model.StatusError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, http.StatusInternalServerError, se.Status)
	assert.Contains(t, se.Snippet, "upstream boom")
}

// The API key never appears in an error (it is only ever sent in the request
// header).
func TestCompleteAPIKeyNeverLeaks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_api_key"}`))
	}))
	defer srv.Close()

	const secret = "sk-super-secret-XYZ"
	c := model.New(model.Config{BaseURL: srv.URL, APIKey: secret, Model: "m"})
	_, err := c.Complete(context.Background(), msgs("s", "u"), nil)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), secret)
}

// A malformed body is an error, not a panic; an empty choices list is an error.
func TestCompleteBadResponses(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"malformed json", `{not json`},
		{"no choices", `{"choices":[],"usage":{"total_tokens":3}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c := model.New(model.Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
			_, err := c.Complete(context.Background(), msgs("s", "u"), nil)
			assert.Error(t, err)
		})
	}
}

// ctx cancellation and deadline are respected.
func TestCompleteContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"x"}}]}`))
	}))
	defer srv.Close()
	c := model.New(model.Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Complete(ctx, msgs("s", "u"), nil)
	assert.Error(t, err)
}

func TestCompleteContextDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(150 * time.Millisecond)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"x"}}]}`))
	}))
	defer srv.Close()
	c := model.New(model.Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := c.Complete(ctx, msgs("s", "u"), nil)
	assert.Error(t, err)
}

// The request advertises the tools, and a response with tool_calls is parsed into
// the completion (id, name, and the JSON-string arguments decoded to a map).
func TestCompleteToolCalls(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"",` +
			`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":\"widgets\"}"}}]}}],` +
			`"usage":{"total_tokens":9}}`))
	}))
	defer srv.Close()

	c := model.New(model.Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	tools := []core.ToolDef{{Name: "search", Description: "search transcripts", Schema: json.RawMessage(`{"type":"object"}`)}}
	res, err := c.Complete(context.Background(), msgs("s", "u"), tools)
	require.NoError(t, err)

	require.Len(t, res.ToolCalls, 1)
	assert.Equal(t, "call_1", res.ToolCalls[0].ID)
	assert.Equal(t, "search", res.ToolCalls[0].Name)
	assert.Equal(t, "widgets", res.ToolCalls[0].Arguments["q"])

	// The request carried the tools array as OpenAI functions.
	var sent struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal([]byte(gotBody), &sent))
	require.Len(t, sent.Tools, 1)
	assert.Equal(t, "function", sent.Tools[0].Type)
	assert.Equal(t, "search", sent.Tools[0].Function.Name)
	assert.JSONEq(t, `{"type":"object"}`, string(sent.Tools[0].Function.Parameters))
}

// A model that emits its NATIVE tool-call syntax in `content` (empty structured
// tool_calls) is parsed into a real tool call, and no sentinel reaches the text
// (#88) — the deployed deepseek symptom.
func TestCompleteParsesNativeToolCall(t *testing.T) {
	nativeContent := "let me look that up\n\n" +
		"function<|tool_sep|>search\n" +
		`{"q":"acme-game"}` + "\n" +
		"<|tool_call_end|>"
	// Marshal the whole response so the native newlines/quotes are correctly escaped.
	body, err := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"message": map[string]any{"role": "assistant", "content": nativeContent}},
		},
		"usage": map[string]any{"total_tokens": 7},
	})
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := model.New(model.Config{BaseURL: srv.URL, APIKey: "k", Model: "deepseek"})
	res, err := c.Complete(context.Background(), msgs("s", "u"), nil)
	require.NoError(t, err)

	require.Len(t, res.ToolCalls, 1, "the native call is parsed into a structured tool call")
	assert.Equal(t, "search", res.ToolCalls[0].Name)
	assert.Equal(t, "acme-game", res.ToolCalls[0].Arguments["q"])
	assert.NotContains(t, res.Text, "<|tool_sep|>", "no sentinel reaches the user")
	assert.NotContains(t, res.Text, "<|tool_call_end|>")
}

// The exact deployed 2026-07-22 leak (#88 resurfacing): garbage prefix, a ```json
// arg fence, and a doubled <|tool_call_end|><|tool_calls_end|> close. #90 dropped
// the call and leaked the plural sentinel; end-to-end the call must execute and
// the reply must be sentinel-free.
func TestCompleteParsesDeepseekNativeVariant(t *testing.T) {
	nativeContent := "غنfunction<|tool_sep|>get_hotness\n" +
		"```json\n" + `{"count":50}` + "\n```" +
		"<|tool_call_end|><|tool_calls_end|>"
	body, err := json.Marshal(map[string]any{
		"choices": []map[string]any{
			{"message": map[string]any{"role": "assistant", "content": nativeContent}},
		},
		"usage": map[string]any{"total_tokens": 9},
	})
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := model.New(model.Config{BaseURL: srv.URL, APIKey: "k", Model: "deepseek"})
	res, err := c.Complete(context.Background(), msgs("s", "u"), nil)
	require.NoError(t, err)

	require.Len(t, res.ToolCalls, 1, "get_hotness executes instead of leaking as text")
	assert.Equal(t, "get_hotness", res.ToolCalls[0].Name)
	assert.Equal(t, float64(50), res.ToolCalls[0].Arguments["count"])
	assert.NotContains(t, res.Text, "<|tool_call_end|>")
	assert.NotContains(t, res.Text, "<|tool_calls_end|>", "the plural close sentinel is scrubbed too")
}

// An assistant tool-call turn and the tool-result turn round-trip their
// correlation ids on the wire (OpenAI rejects unmatched tool results).
func TestCompleteRoundTripsToolMessages(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"done"}}],"usage":{"total_tokens":3}}`))
	}))
	defer srv.Close()

	c := model.New(model.Config{BaseURL: srv.URL, APIKey: "k", Model: "m"})
	convo := []core.Message{
		{Role: core.RoleSystem, Content: "s"},
		{Role: core.RoleUser, Content: "u"},
		{Role: core.RoleAssistant, ToolCalls: []core.ToolCall{{ID: "call_1", Name: "search", Arguments: map[string]any{"q": "x"}}}},
		{Role: core.RoleTool, ToolCallID: "call_1", Content: "result text"},
	}
	_, err := c.Complete(context.Background(), convo, nil)
	require.NoError(t, err)

	assert.Contains(t, gotBody, `"id":"call_1"`, "assistant tool-call carries its id")
	assert.Contains(t, gotBody, `"tool_call_id":"call_1"`, "tool result correlates to the call")
	assert.Contains(t, gotBody, `"arguments":"{\"q\":\"x\"}"`, "args serialized as a JSON string")
}
