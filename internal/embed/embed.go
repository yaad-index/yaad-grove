// Package embed provides an OpenAI-compatible embeddings client (ADR 0017). It
// mirrors internal/model: a provider-neutral Embedder over any /embeddings
// endpoint, selected by config (base URL, key, model). The semantic retriever
// uses it to embed vault chunks at index time and the query at serve time.
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// defaultTimeout bounds a single embeddings call; a tighter ctx deadline still
// takes precedence.
const defaultTimeout = 60 * time.Second

// Embedder turns texts into vectors (ADR 0017). It is one method on purpose, so
// tests inject a deterministic fake and CI never hits a live endpoint.
type Embedder interface {
	// Embed returns one vector per input text, in input order.
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// Config selects and authenticates an OpenAI-compatible /embeddings endpoint.
type Config struct {
	BaseURL string // e.g. https://api.openai.com/v1 or a local Ollama /v1 gateway
	APIKey  string // via env/secret; empty is fine for a no-auth local endpoint
	Model   string // embedding model id understood by the endpoint (e.g. bge-m3)
}

// Client is an OpenAI-compatible embeddings client implementing Embedder.
type Client struct {
	cfg  Config
	http *http.Client
}

// New returns a Client for the given endpoint config.
func New(cfg Config) *Client {
	return &Client{cfg: cfg, http: &http.Client{Timeout: defaultTimeout}}
}

// StatusError is a non-2xx response from the embeddings endpoint. It carries the
// status and a bounded, secrets-free excerpt of the body (the key rides only in
// the request header, never echoed here).
type StatusError struct {
	Status  int
	Snippet string
}

func (e *StatusError) Error() string {
	if e.Snippet == "" {
		return fmt.Sprintf("embed: endpoint returned status %d", e.Status)
	}
	return fmt.Sprintf("embed: endpoint returned status %d: %s", e.Status, e.Snippet)
}

type request struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type response struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns one vector per input text, in input order. An empty input makes
// no call. The response is reordered by its `index` field so vectors line up
// with inputs regardless of the endpoint's ordering.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(request{Model: c.cfg.Model, Input: texts})
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(c.cfg.BaseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &StatusError{Status: resp.StatusCode, Snippet: strings.TrimSpace(string(snippet))}
	}

	var out response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if len(out.Data) != len(texts) {
		return nil, fmt.Errorf("embed: got %d vectors for %d inputs", len(out.Data), len(texts))
	}
	sort.Slice(out.Data, func(i, j int) bool { return out.Data[i].Index < out.Data[j].Index })
	vecs := make([][]float32, len(out.Data))
	for i := range out.Data {
		vecs[i] = out.Data[i].Embedding
	}
	return vecs, nil
}
