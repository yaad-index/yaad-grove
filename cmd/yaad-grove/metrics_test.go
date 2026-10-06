package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/bonyan/secret"

	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/metrics/metricstest"
)

// The metrics reach every place serve records from: the embedding client, the
// retriever and the engine.
func TestAnsweringRecordsMetrics(t *testing.T) {
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer model.Close()
	embeddings := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]string, len(req.Input))
		for i := range req.Input {
			data[i] = fmt.Sprintf(`{"index":%d,"embedding":[1,0]}`, i)
		}
		_, _ = fmt.Fprintf(w, `{"data":[%s],"usage":{"prompt_tokens":3}}`, strings.Join(data, ","))
	}))
	defer embeddings.Close()

	c := &ServeCmd{
		VaultDir: tempVault(t), Scope: "notes", Language: "en", ModelBaseURL: model.URL, ModelName: "m",
		MaxOutputTokens: 100, ContextSize: 8000, SimilarityThreshold: 0,
		EmbeddingBaseURL: embeddings.URL, EmbeddingModel: "embed-model",
	}
	meter, err := budget.New(budget.Config{Ceiling: 1000, Period: time.Hour}, &budget.MemoryStore{})
	require.NoError(t, err)
	m, collect := metricstest.New(t)
	a, err := c.buildAnswering(discard, meter, secret.NewResolver(secret.Env{}), nil, nil, m)
	require.NoError(t, err)
	_, err = a.engine.Answer(context.Background(), core.Query{Text: "hello world"})
	require.NoError(t, err)

	got := collect()
	assert.NotEmpty(t, got["gen_ai.client.operation.duration"].Points, "the embedding client records")
	require.Len(t, got["grove.retrieval.duration"].Points, 1, "the retriever records")
	assert.Equal(t, "hybrid", got["grove.retrieval.duration"].Points[0].Attrs["grove.retrieval.mode"])
	assert.Len(t, got["grove.retrieval.chunks"].Points, 1, "the engine records")
}
