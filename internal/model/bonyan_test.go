package model

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bmodel "github.com/yaad-index/bonyan/model"
)

// cannedChat answers every call with one response, or one error.
type cannedChat struct {
	resp bmodel.ChatResponse
	err  error
}

func (c cannedChat) Chat(context.Context, bmodel.ChatRequest) (bmodel.ChatResponse, error) {
	return c.resp, c.err
}

// A tool call written in the model's native syntax inside the text is
// recovered as a structured call, and its syntax leaves the text.
func TestNativeToolCallsRecoversACallInTheText(t *testing.T) {
	text := "let me check that for you\n\nfunction" + nativeToolSep + "search\n" + `{"q":"acme-game"}` + "\n" + nativeToolEnd
	got, err := NativeToolCalls(cannedChat{resp: bmodel.ChatResponse{Content: text, StopReason: bmodel.StopEnd}}).Chat(context.Background(), bmodel.ChatRequest{})
	require.NoError(t, err)
	require.Len(t, got.ToolCalls, 1)
	assert.Equal(t, "search", got.ToolCalls[0].Name)
	assert.JSONEq(t, `{"q":"acme-game"}`, string(got.ToolCalls[0].Arguments))
	assert.NotEmpty(t, got.ToolCalls[0].ID)
	assert.Equal(t, bmodel.StopToolCalls, got.StopReason)
	assert.Equal(t, "let me check that for you", got.Content)
	assertNoSentinels(t, got.Content)
}

// A structured call stands, with a stray sentinel in the text scrubbed; a
// reply with no native syntax, and an error, pass unchanged.
func TestNativeToolCallsLeavesTheRestAlone(t *testing.T) {
	structured := bmodel.ChatResponse{
		Content:    "calling it" + nativeToolEnd,
		ToolCalls:  []bmodel.ToolCall{{ID: "c1", Name: "search", Arguments: json.RawMessage(`{}`)}},
		StopReason: bmodel.StopToolCalls,
	}
	got, err := NativeToolCalls(cannedChat{resp: structured}).Chat(context.Background(), bmodel.ChatRequest{})
	require.NoError(t, err)
	assert.Equal(t, structured.ToolCalls, got.ToolCalls)
	assert.Equal(t, "calling it", got.Content)

	plain := bmodel.ChatResponse{Content: "the answer", StopReason: bmodel.StopEnd, Usage: &bmodel.Usage{InputTokens: 3}}
	got, err = NativeToolCalls(cannedChat{resp: plain}).Chat(context.Background(), bmodel.ChatRequest{})
	require.NoError(t, err)
	assert.Equal(t, plain, got)

	failure := errors.New("down")
	_, err = NativeToolCalls(cannedChat{err: failure}).Chat(context.Background(), bmodel.ChatRequest{})
	require.ErrorIs(t, err, failure)
}
