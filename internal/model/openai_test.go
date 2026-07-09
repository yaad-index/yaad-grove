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

	"github.com/yaad-index/yaad-grove/internal/model"
)

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
	res, err := c.Complete(context.Background(), "be terse", "hello")
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
	_, err := c.Complete(context.Background(), "s", "u")
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
	_, err := c.Complete(context.Background(), "s", "u")
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
			_, err := c.Complete(context.Background(), "s", "u")
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
	_, err := c.Complete(ctx, "s", "u")
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
	_, err := c.Complete(ctx, "s", "u")
	assert.Error(t, err)
}
