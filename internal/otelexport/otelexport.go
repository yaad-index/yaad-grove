// Package otelexport sends the process's spans and metrics to an OpenTelemetry
// collector over OTLP. It is configured only by the standard OTEL_* environment
// variables, and is off unless an OTLP endpoint is set in them.
package otelexport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// Exporter holds the providers spans and metrics are made with, and sends
// what they make to the collector.
type Exporter struct {
	// TracerProvider and MeterProvider are no-ops for a signal whose export
	// is off.
	TracerProvider trace.TracerProvider
	MeterProvider  metric.MeterProvider
	// Traces and Metrics say which signals are exported.
	Traces, Metrics bool

	shutdown []func(context.Context) error
}

// Setup returns the exporter the environment configures, for the service
// named service at version. It returns nil when neither signal is exported:
// no OTLP endpoint is set for it, or OTEL_SDK_DISABLED is true.
//
// The exporters read their endpoint, headers, timeout, compression and
// certificate from the OTEL_EXPORTER_OTLP_* variables themselves; Setup reads
// only which of them are set and the protocol. OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES override the resource's service name and version.
func Setup(ctx context.Context, service, version string) (*Exporter, error) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true") {
		return nil, nil
	}
	traces, metrics := endpointSet("TRACES"), endpointSet("METRICS")
	if !traces && !metrics {
		return nil, nil
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(service), semconv.ServiceVersion(version)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("telemetry resource: %w", err)
	}

	e := &Exporter{TracerProvider: tracenoop.NewTracerProvider(), MeterProvider: noop.NewMeterProvider(), Traces: traces, Metrics: metrics}
	if traces {
		exp, err := traceExporter(ctx)
		if err != nil {
			return nil, err
		}
		tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
		e.TracerProvider = tp
		e.shutdown = append(e.shutdown, tp.Shutdown)
	}
	if metrics {
		exp, err := metricExporter(ctx)
		if err != nil {
			_ = e.Shutdown(ctx)
			return nil, err
		}
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)), sdkmetric.WithResource(res))
		e.MeterProvider = mp
		e.shutdown = append(e.shutdown, mp.Shutdown)
	}
	return e, nil
}

// Shutdown sends what is still buffered and stops both providers.
func (e *Exporter) Shutdown(ctx context.Context) error {
	var errs []error
	for _, f := range e.shutdown {
		errs = append(errs, f(ctx))
	}
	return errors.Join(errs...)
}

// endpointSet reports whether an OTLP endpoint is set for signal, either its
// own or the one shared by every signal.
func endpointSet(signal string) bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT") != ""
}

// The protocols the standard names that grove exports with. http/protobuf is
// the standard's default; http/json has no Go exporter.
const (
	protocolGRPC = "grpc"
	protocolHTTP = "http/protobuf"
)

// protocol is the OTLP protocol for signal: its own variable, else the shared
// one, else the standard's default.
func protocol(signal string) (string, error) {
	p := os.Getenv("OTEL_EXPORTER_OTLP_" + signal + "_PROTOCOL")
	if p == "" {
		p = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	switch p = strings.TrimSpace(p); p {
	case "":
		return protocolHTTP, nil
	case protocolGRPC, protocolHTTP:
		return p, nil
	}
	return "", fmt.Errorf("OTLP protocol %q is not supported for %s: use %q or %q", p, strings.ToLower(signal), protocolHTTP, protocolGRPC)
}

func traceExporter(ctx context.Context) (sdktrace.SpanExporter, error) {
	p, err := protocol("TRACES")
	if err != nil {
		return nil, err
	}
	if p == protocolGRPC {
		return otlptracegrpc.New(ctx)
	}
	return otlptracehttp.New(ctx)
}

func metricExporter(ctx context.Context) (sdkmetric.Exporter, error) {
	p, err := protocol("METRICS")
	if err != nil {
		return nil, err
	}
	if p == protocolGRPC {
		return otlpmetricgrpc.New(ctx)
	}
	return otlpmetrichttp.New(ctx)
}
