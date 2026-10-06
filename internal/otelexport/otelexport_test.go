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
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/yaad-index/yaad-grove/internal/otelexport"
)

// clearEnv unsets every variable Setup reads, so the host's own cannot
// change a test.
func clearEnv(t *testing.T) {
	for _, k := range []string{
		"OTEL_SDK_DISABLED", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
		"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL",
	} {
		t.Setenv(k, "")
	}
}

func TestExportIsOffWithoutAnEndpoint(t *testing.T) {
	clearEnv(t)
	e, err := otelexport.Setup(context.Background(), "yaad-grove", "v1")
	require.NoError(t, err)
	assert.Nil(t, e)
}

func TestExportIsOffWhenTheSDKIsDisabled(t *testing.T) {
	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	e, err := otelexport.Setup(context.Background(), "yaad-grove", "v1")
	require.NoError(t, err)
	assert.Nil(t, e)
}

// A signal's own endpoint turns on that signal only.
func TestASignalsOwnEndpointExportsOnlyIt(t *testing.T) {
	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:1/v1/traces")
	e, err := otelexport.Setup(context.Background(), "yaad-grove", "v1")
	require.NoError(t, err)
	require.NotNil(t, e)
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })
	assert.True(t, e.Traces)
	assert.False(t, e.Metrics)

	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://127.0.0.1:1/v1/metrics")
	e, err = otelexport.Setup(context.Background(), "yaad-grove", "v1")
	require.NoError(t, err)
	require.NotNil(t, e)
	t.Cleanup(func() { _ = e.Shutdown(context.Background()) })
	assert.False(t, e.Traces)
	assert.True(t, e.Metrics)
}

func TestAnUnsupportedProtocolIsRefused(t *testing.T) {
	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/json")
	_, err := otelexport.Setup(context.Background(), "yaad-grove", "v1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"http/json"`)

	// A signal's own protocol wins over the shared one.
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_PROTOCOL", "http/json")
	_, err = otelexport.Setup(context.Background(), "yaad-grove", "v1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "metrics")
}

// A span reaches the endpoint over OTLP/HTTP, under the service's name and
// version, by the time Shutdown returns.
func TestASpanReachesTheEndpoint(t *testing.T) {
	var mu sync.Mutex
	var got []*coltrace.ExportTraceServiceRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		var req coltrace.ExportTraceServiceRequest
		assert.NoError(t, proto.Unmarshal(body, &req))
		mu.Lock()
		got = append(got, &req)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	e, err := otelexport.Setup(context.Background(), "yaad-grove", "v1.2.3")
	require.NoError(t, err)
	require.NotNil(t, e)
	_, span := e.TracerProvider.Tracer("test").Start(context.Background(), "a-span")
	span.End()
	require.NoError(t, e.Shutdown(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 1)
	rs := got[0].ResourceSpans
	require.Len(t, rs, 1)
	attrs := map[string]string{}
	for _, kv := range rs[0].Resource.Attributes {
		attrs[kv.Key] = kv.Value.GetStringValue()
	}
	assert.Equal(t, "yaad-grove", attrs["service.name"])
	assert.Equal(t, "v1.2.3", attrs["service.version"])
	require.Len(t, rs[0].ScopeSpans, 1)
	require.Len(t, rs[0].ScopeSpans[0].Spans, 1)
	assert.Equal(t, "a-span", rs[0].ScopeSpans[0].Spans[0].Name)
}

// OTEL_SERVICE_NAME overrides the service name grove gives.
func TestTheEnvironmentNamesTheService(t *testing.T) {
	var mu sync.Mutex
	var name string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req coltrace.ExportTraceServiceRequest
		if r.URL.Path == "/v1/traces" && proto.Unmarshal(body, &req) == nil && len(req.ResourceSpans) > 0 {
			for _, kv := range req.ResourceSpans[0].Resource.Attributes {
				if kv.Key == "service.name" {
					mu.Lock()
					name = kv.Value.GetStringValue()
					mu.Unlock()
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	clearEnv(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_SERVICE_NAME", "grove-test-instance")
	e, err := otelexport.Setup(context.Background(), "yaad-grove", "v1")
	require.NoError(t, err)
	_, span := e.TracerProvider.Tracer("test").Start(context.Background(), "a-span")
	span.End()
	require.NoError(t, e.Shutdown(context.Background()))
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, "grove-test-instance", name)
}
