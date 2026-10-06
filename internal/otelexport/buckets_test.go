package otelexport_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/bonyan/budget"
	bmodel "github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/telemetry"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	colmetric "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"

	"github.com/yaad-index/yaad-grove/internal/metrics"
	"github.com/yaad-index/yaad-grove/internal/otelexport"
)

type oneReply struct{}

func (oneReply) Chat(context.Context, bmodel.ChatRequest) (bmodel.ChatResponse, error) {
	return bmodel.ChatResponse{Content: "ok", StopReason: bmodel.StopEnd, Usage: &bmodel.Usage{InputTokens: 3, OutputTokens: 2}}, nil
}

// The agent library's model-call histograms, created without boundaries, get
// the GenAI conventions' advised ones from the exporter's meter provider.
func TestModelCallHistogramsGetTheAdvisedBuckets(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	tel, err := telemetry.New(telemetry.Options{MeterProvider: otelexport.NewMeterProvider(reader)})
	require.NoError(t, err)
	_, err = tel.Chat(oneReply{}, "m", budget.Price{}, nil).Chat(context.Background(), bmodel.ChatRequest{MaxOutputTokens: 10})
	require.NoError(t, err)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	got := map[string][]float64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			switch d := m.Data.(type) {
			case metricdata.Histogram[float64]:
				got[m.Name] = d.DataPoints[0].Bounds
			case metricdata.Histogram[int64]:
				got[m.Name] = d.DataPoints[0].Bounds
			}
		}
	}
	assert.Equal(t, metrics.DurationBuckets, got["gen_ai.client.operation.duration"])
	assert.Equal(t, metrics.TokenBuckets, got["gen_ai.client.token.usage"])
}

// Setup's meter provider is the one with the views: a model-call histogram
// sent over OTLP carries the advised boundaries.
func TestExportedHistogramsCarryTheAdvisedBuckets(t *testing.T) {
	var mu sync.Mutex
	var bounds []float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/metrics" {
			body, _ := io.ReadAll(r.Body)
			var req colmetric.ExportMetricsServiceRequest
			if proto.Unmarshal(body, &req) == nil {
				for _, rm := range req.ResourceMetrics {
					for _, sm := range rm.ScopeMetrics {
						for _, m := range sm.Metrics {
							if h := m.GetHistogram(); m.Name == "gen_ai.client.operation.duration" && h != nil && len(h.DataPoints) > 0 {
								mu.Lock()
								bounds = h.DataPoints[0].ExplicitBounds
								mu.Unlock()
							}
						}
					}
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	e, err := otelexport.Setup(context.Background(), "yaad-grove", "v1")
	require.NoError(t, err)
	tel, err := telemetry.New(telemetry.Options{MeterProvider: e.MeterProvider, TracerProvider: e.TracerProvider})
	require.NoError(t, err)
	_, err = tel.Chat(oneReply{}, "m", budget.Price{}, nil).Chat(context.Background(), bmodel.ChatRequest{MaxOutputTokens: 10})
	require.NoError(t, err)
	require.NoError(t, e.Shutdown(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, metrics.DurationBuckets, bounds)
}
