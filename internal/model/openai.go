// Package model provides an OpenAI-compatible chat model client implementing
// core.Model. The engine is provider-neutral: it only needs an OpenAI-shaped
// chat completions API, so any compatible endpoint works, selected by config
// (base URL, API key, model name).
package model

import (
	"context"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// Config selects and authenticates an OpenAI-compatible endpoint.
type Config struct {
	BaseURL string // e.g. https://api.openai.com/v1 or any compatible gateway
	APIKey  string // supplied via env/secret, never inlined in config files
	Model   string // model name/id understood by the endpoint
}

// Client is an OpenAI-compatible chat model. It implements core.Model.
type Client struct {
	cfg Config
}

// New returns a Client for the given endpoint config.
func New(cfg Config) *Client {
	return &Client{cfg: cfg}
}

// Complete sends a system+user prompt and returns the model's text.
//
// Scaffold: no HTTP yet. Phase 1 will POST to {BaseURL}/chat/completions with
// the two messages and return the first choice.
func (c *Client) Complete(ctx context.Context, system, user string) (string, error) {
	return "", core.ErrNotImplemented
}

// compile-time assertion that Client satisfies core.Model.
var _ core.Model = (*Client)(nil)
