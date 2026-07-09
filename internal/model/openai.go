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

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// Complete sends a system+user prompt to the chat-completions endpoint and
// returns the first choice's text plus the call's token usage (ADR 0006). It
// propagates ctx (cancellation + deadline); a non-2xx response is a *StatusError;
// network and decode failures are wrapped. The API key is never logged or
// included in an error.
func (c *Client) Complete(ctx context.Context, system, user string) (core.Completion, error) {
	body, err := json.Marshal(chatRequest{
		Model: c.cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
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
	return core.Completion{
		Text: out.Choices[0].Message.Content,
		Usage: core.Usage{
			PromptTokens:     out.Usage.PromptTokens,
			CompletionTokens: out.Usage.CompletionTokens,
			TotalTokens:      out.Usage.TotalTokens,
		},
	}, nil
}

// compile-time assertion that Client satisfies core.Model.
var _ core.Model = (*Client)(nil)
