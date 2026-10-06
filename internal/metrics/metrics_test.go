package metrics_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/metrics"
	"github.com/yaad-index/yaad-grove/internal/metrics/metricstest"
)

func TestAnAnswerIsCountedAndTimed(t *testing.T) {
	m, collect := metricstest.New(t)
	m.Answer(context.Background(), "group", metrics.Answered, 2*time.Second)
	m.Answer(context.Background(), "dm", metrics.Refused, time.Second)

	got := collect()
	answers := got["grove.answers"]
	assert.Equal(t, "{answer}", answers.Unit)
	assert.ElementsMatch(t, []metricstest.Point{
		{Attrs: map[string]string{"grove.surface": "group", "grove.answer.outcome": "answered"}, Value: 1},
		{Attrs: map[string]string{"grove.surface": "dm", "grove.answer.outcome": "refused"}, Value: 1},
	}, answers.Points)

	duration := got["grove.answer.duration"]
	assert.Equal(t, "s", duration.Unit)
	assert.ElementsMatch(t, []metricstest.Point{
		{Attrs: map[string]string{"grove.surface": "group", "grove.answer.outcome": "answered"}, Value: 1, Sum: 2},
		{Attrs: map[string]string{"grove.surface": "dm", "grove.answer.outcome": "refused"}, Value: 1, Sum: 1},
	}, duration.Points)
}

func TestRetrievalIsTimedByMode(t *testing.T) {
	m, collect := metricstest.New(t)
	m.Retrieval(context.Background(), "hybrid", 500*time.Millisecond)
	m.Chunks(context.Background(), 4)

	got := collect()
	assert.Equal(t, "s", got["grove.retrieval.duration"].Unit)
	assert.Equal(t, []metricstest.Point{{Attrs: map[string]string{"grove.retrieval.mode": "hybrid"}, Value: 1, Sum: 0.5}}, got["grove.retrieval.duration"].Points)
	assert.Equal(t, "{chunk}", got["grove.retrieval.chunks"].Unit)
	assert.Equal(t, []metricstest.Point{{Attrs: map[string]string{}, Value: 1, Sum: 4}}, got["grove.retrieval.chunks"].Points)
}

// An embedding call is measured under the GenAI conventions' names, with its
// input tokens when the endpoint reported them and an error's kind, never its
// text.
func TestAnEmbeddingCallUsesTheGenAINames(t *testing.T) {
	m, collect := metricstest.New(t)
	ctx := context.Background()
	m.Embedding(ctx, "embed-model", time.Second, 12, nil)
	m.Embedding(ctx, "embed-model", time.Second, -1, nil)
	m.Embedding(ctx, "embed-model", time.Second, -1, fmt.Errorf("call: %w", context.DeadlineExceeded))
	m.Embedding(ctx, "embed-model", time.Second, -1, errors.New("words-in-the-error"))

	ok := map[string]string{"gen_ai.operation.name": "embeddings", "gen_ai.request.model": "embed-model"}
	with := func(k, v string) map[string]string {
		out := map[string]string{k: v}
		for kk, vv := range ok {
			out[kk] = vv
		}
		return out
	}
	got := collect()
	duration := got["gen_ai.client.operation.duration"]
	assert.Equal(t, "s", duration.Unit)
	assert.ElementsMatch(t, []metricstest.Point{
		{Attrs: ok, Value: 2, Sum: 2},
		{Attrs: with("error.type", "timeout"), Value: 1, Sum: 1},
		{Attrs: with("error.type", "_OTHER"), Value: 1, Sum: 1},
	}, duration.Points)

	tokens := got["gen_ai.client.token.usage"]
	assert.Equal(t, "{token}", tokens.Unit)
	assert.Equal(t, []metricstest.Point{{Attrs: with("gen_ai.token.type", "input"), Value: 1, Sum: 12}}, tokens.Points)
}

func TestTheSpendIsObserved(t *testing.T) {
	m, collect := metricstest.New(t)
	remaining := int64(700)
	require.NoError(t, m.ObserveSpend(1000, func() int64 { return remaining }))

	got := collect()
	assert.Equal(t, "{token}", got["grove.spend.remaining"].Unit)
	assert.Equal(t, []metricstest.Point{{Attrs: map[string]string{}, Value: 700}}, got["grove.spend.remaining"].Points)
	assert.Equal(t, []metricstest.Point{{Attrs: map[string]string{}, Value: 1000}}, got["grove.spend.ceiling"].Points)

	remaining = 50
	assert.Equal(t, int64(50), collect()["grove.spend.remaining"].Points[0].Value)
}

func TestNilMetricsRecordNothing(t *testing.T) {
	var m *metrics.Metrics
	ctx := context.Background()
	assert.NotPanics(t, func() {
		m.Answer(ctx, "group", metrics.Answered, time.Second)
		m.Retrieval(ctx, "keyword", time.Second)
		m.Chunks(ctx, 1)
		m.Embedding(ctx, "x", time.Second, 1, nil)
		assert.NoError(t, m.ObserveSpend(1, func() int64 { return 1 }))
	})
}

// Each histogram carries its boundaries from its creation, whatever meter
// provider records it: seconds and token counts on the GenAI conventions'
// advised boundaries, chunks on powers of two up to 64.
func TestHistogramsHaveTheirBuckets(t *testing.T) {
	m, collect := metricstest.New(t)
	ctx := context.Background()
	m.Answer(ctx, "group", metrics.Answered, time.Second)
	m.Retrieval(ctx, "keyword", time.Second)
	m.Chunks(ctx, 3)
	m.Embedding(ctx, "embed-model", time.Second, 5, nil)

	got := collect()
	for name, want := range map[string][]float64{
		"grove.answer.duration":            metrics.DurationBuckets,
		"grove.retrieval.duration":         metrics.DurationBuckets,
		"gen_ai.client.operation.duration": metrics.DurationBuckets,
		"gen_ai.client.token.usage":        metrics.TokenBuckets,
		"grove.retrieval.chunks":           metrics.ChunkBuckets,
	} {
		assert.Equal(t, want, got[name].Bounds, name)
	}
	assert.Equal(t, 0.01, metrics.DurationBuckets[0])
	assert.Equal(t, 81.92, metrics.DurationBuckets[len(metrics.DurationBuckets)-1])
	assert.Equal(t, float64(67108864), metrics.TokenBuckets[len(metrics.TokenBuckets)-1])
}
