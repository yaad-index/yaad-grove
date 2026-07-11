package embed_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/embed"
)

// A 200 returns one vector per input in input order; the request carries the
// model, the inputs, and the bearer key.
func TestEmbedSuccess(t *testing.T) {
	var gotAuth, gotBody, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2]},{"index":1,"embedding":[0.3,0.4]}]}`))
	}))
	defer srv.Close()

	c := embed.New(embed.Config{BaseURL: srv.URL, Model: "bge-m3", APIKey: "k"})
	vecs, err := c.Embed(context.Background(), []string{"alpha", "beta"})
	require.NoError(t, err)
	require.Len(t, vecs, 2)
	assert.Equal(t, []float32{0.1, 0.2}, vecs[0])
	assert.Equal(t, []float32{0.3, 0.4}, vecs[1])

	assert.Equal(t, "/embeddings", gotPath)
	assert.Equal(t, "Bearer k", gotAuth)
	assert.Contains(t, gotBody, `"bge-m3"`)
	assert.Contains(t, gotBody, `"alpha"`)
}

// Out-of-order response data is reordered by index so vectors line up.
func TestEmbedOrdersByIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":1,"embedding":[9,9]},{"index":0,"embedding":[1,1]}]}`))
	}))
	defer srv.Close()

	c := embed.New(embed.Config{BaseURL: srv.URL, Model: "m"})
	vecs, err := c.Embed(context.Background(), []string{"a", "b"})
	require.NoError(t, err)
	assert.Equal(t, []float32{1, 1}, vecs[0], "index 0 first regardless of response order")
	assert.Equal(t, []float32{9, 9}, vecs[1])
}

// A non-2xx surfaces a StatusError with the status and a body excerpt.
func TestEmbedStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()

	c := embed.New(embed.Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.Embed(context.Background(), []string{"a"})
	var se *embed.StatusError
	require.ErrorAs(t, err, &se)
	assert.Equal(t, http.StatusInternalServerError, se.Status)
	assert.Contains(t, se.Snippet, "boom")
}

// A mismatched vector count is an error (never silently misaligned).
func TestEmbedCountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
	}))
	defer srv.Close()

	c := embed.New(embed.Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.Embed(context.Background(), []string{"a", "b"})
	assert.Error(t, err)
}

// A right-count-but-non-contiguous index set (duplicate/gapped) is rejected — it
// would otherwise misalign vectors with inputs silently.
func TestEmbedNonContiguousIndex(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}]}`))
	}))
	defer srv.Close()

	c := embed.New(embed.Config{BaseURL: srv.URL, Model: "m"})
	_, err := c.Embed(context.Background(), []string{"a", "b"})
	assert.Error(t, err, "duplicate index at the right count still errors, never misaligns")
}

// With no key, no Authorization header is sent — pinning the no-auth local case.
func TestEmbedNoKeyOmitsAuthHeader(t *testing.T) {
	var hasAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasAuth = r.Header["Authorization"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]}]}`))
	}))
	defer srv.Close()

	c := embed.New(embed.Config{BaseURL: srv.URL, Model: "m"}) // no APIKey
	_, err := c.Embed(context.Background(), []string{"a"})
	require.NoError(t, err)
	assert.False(t, hasAuth, "no Authorization header when no key is configured")
}

// Empty input makes no request.
func TestEmbedEmptyInput(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	c := embed.New(embed.Config{BaseURL: srv.URL, Model: "m"})
	vecs, err := c.Embed(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, vecs)
	assert.False(t, called, "no endpoint call for empty input")
}
