// Package model provides an OpenAI-compatible chat model client implementing
// core.Model. The engine is provider-neutral: it only needs an OpenAI-shaped
// chat completions API, so any compatible endpoint works, selected by config
// (base URL, API key, model name).
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// defaultTimeout bounds a single completion call; ctx cancellation/deadline still
// takes precedence when tighter.
const defaultTimeout = 60 * time.Second

// Config selects and authenticates an OpenAI-compatible endpoint.
type Config struct {
	BaseURL string // e.g. https://api.openai.com/v1 or any compatible gateway
	APIKey  string // supplied via env/secret, never inlined in config files
	Model   string // model name/id understood by the endpoint
}

// Client is an OpenAI-compatible chat model. It implements core.Model.
type Client struct {
	cfg  Config
	http *http.Client
}

// New returns a Client for the given endpoint config.
func New(cfg Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: defaultTimeout}}
}

// StatusError is a non-2xx response from the model endpoint. It carries the
// status and a bounded, secrets-free excerpt of the response body (the API key
// is only ever sent in the request header, never echoed here).
type StatusError struct {
	Status  int
	Snippet string
}

func (e *StatusError) Error() string {
	if e.Snippet == "" {
		return fmt.Sprintf("model: endpoint returned status %d", e.Status)
	}
	return fmt.Sprintf("model: endpoint returned status %d: %s", e.Status, e.Snippet)
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // a JSON string, per the OpenAI wire format
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function chatFunctionCall `json:"function"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type chatTool struct {
	Type     string           `json:"type"` // "function"
	Function chatToolFunction `json:"function"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []chatTool    `json:"tools,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content   string         `json:"content"`
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// Complete runs one round of the conversation with the available tools and
// returns either the first choice's text or its tool-call requests, plus token
// usage (ADR 0006/0011). It propagates ctx; a non-2xx response is a *StatusError;
// network and decode failures are wrapped. The API key is never logged or
// included in an error.
func (c *Client) Complete(ctx context.Context, messages []core.Message, tools []core.ToolDef) (core.Completion, error) {
	reqMessages, err := toChatMessages(messages)
	if err != nil {
		return core.Completion{}, err
	}
	body, err := json.Marshal(chatRequest{
		Model:    c.cfg.Model,
		Messages: reqMessages,
		Tools:    toChatTools(tools),
	})
	if err != nil {
		return core.Completion{}, fmt.Errorf("model: marshal request: %w", err)
	}

	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return core.Completion{}, fmt.Errorf("model: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return core.Completion{}, fmt.Errorf("model: request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return core.Completion{}, &StatusError{Status: resp.StatusCode, Snippet: strings.TrimSpace(string(snippet))}
	}

	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return core.Completion{}, fmt.Errorf("model: decode response: %w", err)
	}
	if len(out.Choices) == 0 {
		return core.Completion{}, errors.New("model: response contained no choices")
	}
	msg := out.Choices[0].Message
	toolCalls, err := fromChatToolCalls(msg.ToolCalls)
	if err != nil {
		return core.Completion{}, err
	}
	text := msg.Content
	// Native tool-call fallback (#88): some models emit a tool call in their native
	// syntax inside `content` instead of the structured `tool_calls` field. When the
	// structured field is empty, parse any native calls out of the content so the
	// tool loop still runs; either way strip the sentinels so they never reach a
	// user (a structured call plus a stray sentinel still gets scrubbed).
	if len(toolCalls) == 0 {
		text, toolCalls = parseNativeToolCalls(text)
	} else {
		text = stripToolSentinels(text)
	}
	return core.Completion{
		Text:      text,
		ToolCalls: toolCalls,
		Usage: core.Usage{
			PromptTokens:     out.Usage.PromptTokens,
			CompletionTokens: out.Usage.CompletionTokens,
			TotalTokens:      out.Usage.TotalTokens,
		},
	}, nil
}

// toChatMessages maps the neutral conversation to the wire format. An assistant
// turn's tool calls and a tool turn's tool_call_id must round-trip so the
// endpoint can correlate results to requests.
func toChatMessages(messages []core.Message) ([]chatMessage, error) {
	out := make([]chatMessage, 0, len(messages))
	for _, m := range messages {
		cm := chatMessage{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			args, err := json.Marshal(tc.Arguments)
			if err != nil {
				return nil, fmt.Errorf("model: marshal tool-call args: %w", err)
			}
			cm.ToolCalls = append(cm.ToolCalls, chatToolCall{
				ID:       tc.ID,
				Type:     "function",
				Function: chatFunctionCall{Name: tc.Name, Arguments: string(args)},
			})
		}
		out = append(out, cm)
	}
	return out, nil
}

// toChatTools maps the tool definitions to the wire format, passing each tool's
// JSON Schema through as the function parameters.
func toChatTools(tools []core.ToolDef) []chatTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]chatTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, chatTool{
			Type:     "function",
			Function: chatToolFunction{Name: t.Name, Description: t.Description, Parameters: t.Schema},
		})
	}
	return out
}

// fromChatToolCalls parses the model's tool-call requests, decoding each
// function's JSON-string arguments into a map for the tool call.
func fromChatToolCalls(calls []chatToolCall) ([]core.ToolCall, error) {
	if len(calls) == 0 {
		return nil, nil
	}
	out := make([]core.ToolCall, 0, len(calls))
	for _, c := range calls {
		args := map[string]any{}
		if a := strings.TrimSpace(c.Function.Arguments); a != "" {
			if err := json.Unmarshal([]byte(a), &args); err != nil {
				return nil, fmt.Errorf("model: decode tool-call args for %q: %w", c.Function.Name, err)
			}
		}
		out = append(out, core.ToolCall{ID: c.ID, Name: c.Function.Name, Arguments: args})
	}
	return out, nil
}

// compile-time assertion that Client satisfies core.Model.
var _ core.Model = (*Client)(nil)
