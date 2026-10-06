package core_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bmodel "github.com/yaad-index/bonyan/model"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// An answer is emitted as a run with its model and tool calls, and nothing
// any span or metric carries is something someone wrote: not the question,
// the asker's name, the vault, the instructions, the tool's arguments or
// result, or the answer.
func TestTelemetryCarriesNoContent(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	tel, err := core.NewTelemetry(tp, mp)
	require.NoError(t, err)

	mdl := &mockModel{replies: []bmodel.ChatResponse{
		{ToolCalls: []bmodel.ToolCall{{ID: "c1", Name: "search", Arguments: json.RawMessage(`{"q":"words-in-the-arguments"}`)}}},
		{Content: "words-in-the-answer"},
	}}
	tools := toolRegistry()
	tools.results["search"] = "words-in-the-result"
	reply, err := newEngine(mdl, mockRetriever{chunks: []core.Chunk{{Source: "a.md", Text: "words-in-the-vault"}}}, tools,
		"words-in-the-scope", core.WithTelemetry(tel)).Answer(context.Background(), core.Query{
		Text: "words-in-the-question",
		User: core.User{Display: "words-in-the-name"},
	})
	require.NoError(t, err)
	require.Equal(t, "words-in-the-answer", reply.Text)

	var names []string
	var values []string
	for _, s := range spans.Ended() {
		names = append(names, s.Name())
		for _, kv := range s.Attributes() {
			assert.NotContains(t, contentKeys, string(kv.Key), "span %s", s.Name())
			values = append(values, kv.Value.String())
		}
		values = append(values, s.Status().Description)
	}
	assert.Contains(t, names, "invoke_agent grove")
	assert.Contains(t, names, "chat "+modelName)
	assert.Contains(t, names, "execute_tool search")

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	var metrics []string
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			metrics = append(metrics, m.Name)
			for _, set := range attributeSets(t, m.Data) {
				for _, kv := range set.ToSlice() {
					values = append(values, kv.Value.String())
				}
			}
		}
	}
	assert.Contains(t, metrics, "gen_ai.client.token.usage")

	for _, v := range values {
		assert.NotContains(t, v, "words-in-", "an attribute carries content")
	}
}

// contentKeys are the GenAI conventions' attributes that carry content.
var contentKeys = []string{
	"gen_ai.system_instructions", "gen_ai.input.messages", "gen_ai.output.messages",
	"gen_ai.tool.call.arguments", "gen_ai.tool.call.result",
}

// attributeSets are the attribute sets of a metric's data points.
func attributeSets(t *testing.T, data metricdata.Aggregation) []attribute.Set {
	var sets []attribute.Set
	switch d := data.(type) {
	case metricdata.Histogram[float64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	case metricdata.Histogram[int64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	case metricdata.Sum[int64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	case metricdata.Sum[float64]:
		for _, p := range d.DataPoints {
			sets = append(sets, p.Attributes)
		}
	default:
		t.Fatalf("metric data %T is not read", data)
	}
	return sets
}
