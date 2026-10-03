package model

import (
	"context"
	"encoding/json"
	"fmt"

	bmodel "github.com/yaad-index/bonyan/model"
)

// NativeToolCalls wraps a bonyan chat model so a tool call the model wrote in
// its native syntax inside the reply's text, rather than as a structured tool
// call, is recovered and run, and its syntax never reaches the user (#88). It
// is the same parser the engine's own client applies: a reply that already
// carries structured tool calls keeps them, with any stray syntax scrubbed from
// its text, and one holding no native syntax passes unchanged.
func NativeToolCalls(inner bmodel.Chat) bmodel.Chat { return nativeChat{inner: inner} }

type nativeChat struct{ inner bmodel.Chat }

func (n nativeChat) Chat(ctx context.Context, req bmodel.ChatRequest) (bmodel.ChatResponse, error) {
	resp, err := n.inner.Chat(ctx, req)
	if err != nil {
		return resp, err
	}
	if len(resp.ToolCalls) > 0 {
		// A structured call stands; a stray sentinel beside it is still
		// scrubbed, as the engine's own client does.
		resp.Content = stripToolSentinels(resp.Content)
		return resp, nil
	}
	text, calls := parseNativeToolCalls(resp.Content)
	resp.Content = text
	for _, c := range calls {
		args, err := json.Marshal(c.Arguments)
		if err != nil {
			return bmodel.ChatResponse{}, fmt.Errorf("model: a native tool call's arguments: %w", err)
		}
		resp.ToolCalls = append(resp.ToolCalls, bmodel.ToolCall{ID: c.ID, Name: c.Name, Arguments: args})
	}
	if len(resp.ToolCalls) > 0 {
		resp.StopReason = bmodel.StopToolCalls
	}
	return resp, nil
}
