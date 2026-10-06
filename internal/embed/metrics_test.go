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
	"github.com/yaad-index/yaad-grove/internal/metrics/metricstest"
)

// Each call is timed, its input tokens are counted when the endpoint reports
// them, and a failed call is timed with its error's kind.
func TestEmbedCallsAreMeasured(t *testing.T) {
	bodies := []string{
		`{"data":[{"index":0,"embedding":[1]}],"usage":{"prompt_tokens":7}}`,
		`{"data":[{"index":0,"embedding":[1]}]}`,
	}
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls == len(bodies) {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = io.WriteString(w, bodies[calls])
		calls++
	}))
	defer srv.Close()

	m, collect := metricstest.New(t)
	c := embed.New(embed.Config{BaseURL: srv.URL, Model: "embed-model", Metrics: m})
	for range bodies {
		_, err := c.Embed(context.Background(), []string{"a"})
		require.NoError(t, err)
	}
	_, err := c.Embed(context.Background(), []string{"a"})
	require.Error(t, err)

	got := collect()
	base := map[string]string{"gen_ai.operation.name": "embeddings", "gen_ai.request.model": "embed-model"}
	failed := map[string]string{"gen_ai.operation.name": "embeddings", "gen_ai.request.model": "embed-model", "error.type": "_OTHER"}
	durations := got["gen_ai.client.operation.duration"].Points
	require.Len(t, durations, 2)
	counts := map[string]int64{}
	for _, p := range durations {
		counts[p.Attrs["error.type"]] = p.Value
		if p.Attrs["error.type"] == "" {
			assert.Equal(t, base, p.Attrs)
		} else {
			assert.Equal(t, failed, p.Attrs)
		}
	}
	assert.Equal(t, map[string]int64{"": 2, "_OTHER": 1}, counts)

	tokens := got["gen_ai.client.token.usage"].Points
	require.Len(t, tokens, 1)
	assert.Equal(t, int64(1), tokens[0].Value, "only the call that reported usage")
	assert.Equal(t, float64(7), tokens[0].Sum)
}
