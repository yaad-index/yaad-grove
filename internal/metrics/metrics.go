// Package metrics is grove's own metrics: what it answered, how retrieval
// went, its embedding calls and the spend ceiling. The model calls, tool calls
// and agent runs are bonyan's (core.NewTelemetry).
//
// Every name and attribute is defined here, and the README lists them. A name
// is the OpenTelemetry semantic conventions' where they have one, else
// grove.<area>.<thing>. An attribute's values are a small fixed set: never an
// id, a name, or anything someone wrote.
//
// A nil *Metrics records nothing, so a caller need not check whether export
// is on.
package metrics

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/semconv/v1.41.0/genaiconv"
)

// scope is the instrumentation scope grove's metrics are under.
const scope = "github.com/yaad-index/yaad-grove"

// grove's own names.
const (
	metricAnswers         = "grove.answers"
	metricAnswerDuration  = "grove.answer.duration"
	metricRetrievalTime   = "grove.retrieval.duration"
	metricRetrievalChunks = "grove.retrieval.chunks"
	metricSpendRemaining  = "grove.spend.remaining"
	metricSpendCeiling    = "grove.spend.ceiling"
	keyAnswerOutcome      = attribute.Key("grove.answer.outcome")
	keySurface            = attribute.Key("grove.surface")
	keyRetrievalMode      = attribute.Key("grove.retrieval.mode")
)

// Outcome is how an answer ended: grove.answer.outcome.
type Outcome string

// The outcomes of an answer.
const (
	// Answered is a reply that answers.
	Answered Outcome = "answered"
	// Refused is a decline: out of scope, nothing to ground on, or a refusal
	// the model gave.
	Refused Outcome = "refused"
	// AtCapacity is the spend ceiling's notice instead of an answer.
	AtCapacity Outcome = "at_capacity"
	// Failed is an error, so no reply.
	Failed Outcome = "error"
)

// Surface is where a question was asked: grove.surface, "group" or "dm".
type Surface string

// Metrics records grove's own metrics.
type Metrics struct {
	answers         metric.Int64Counter
	answerDuration  metric.Float64Histogram
	retrievalTime   metric.Float64Histogram
	retrievalChunks metric.Int64Histogram
	embedDuration   metric.Float64Histogram
	embedTokens     metric.Int64Histogram
	meter           metric.Meter
}

// New returns Metrics recording through mp.
func New(mp metric.MeterProvider) (*Metrics, error) {
	m := mp.Meter(scope)
	answers, err := m.Int64Counter(metricAnswers, metric.WithUnit("{answer}"),
		metric.WithDescription("Questions the engine was asked, by how the answer ended."))
	if err != nil {
		return nil, err
	}
	answerDuration, err := m.Float64Histogram(metricAnswerDuration, metric.WithUnit("s"),
		metric.WithDescription("From taking a question to its reply being ready."))
	if err != nil {
		return nil, err
	}
	retrievalTime, err := m.Float64Histogram(metricRetrievalTime, metric.WithUnit("s"),
		metric.WithDescription("How long retrieving a question's vault chunks took."))
	if err != nil {
		return nil, err
	}
	retrievalChunks, err := m.Int64Histogram(metricRetrievalChunks, metric.WithUnit("{chunk}"),
		metric.WithDescription("The vault chunks a question's answer was given, after the context-size guard."))
	if err != nil {
		return nil, err
	}
	embedDuration, err := genaiconv.NewClientOperationDuration(m)
	if err != nil {
		return nil, err
	}
	embedTokens, err := genaiconv.NewClientTokenUsage(m)
	if err != nil {
		return nil, err
	}
	return &Metrics{
		answers:         answers,
		answerDuration:  answerDuration,
		retrievalTime:   retrievalTime,
		retrievalChunks: retrievalChunks,
		embedDuration:   embedDuration.Inst(),
		embedTokens:     embedTokens.Inst(),
		meter:           m,
	}, nil
}

// Answer records a question the engine was asked on surface, how its answer
// ended, and how long it took.
func (m *Metrics) Answer(ctx context.Context, surface Surface, outcome Outcome, took time.Duration) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(keySurface.String(string(surface)), keyAnswerOutcome.String(string(outcome)))
	m.answers.Add(ctx, 1, attrs)
	m.answerDuration.Record(ctx, took.Seconds(), attrs)
}

// Retrieval records how long a retrieval in mode took.
func (m *Metrics) Retrieval(ctx context.Context, mode string, took time.Duration) {
	if m == nil {
		return
	}
	m.retrievalTime.Record(ctx, took.Seconds(), metric.WithAttributes(keyRetrievalMode.String(mode)))
}

// Chunks records how many vault chunks an answer was given.
func (m *Metrics) Chunks(ctx context.Context, n int) {
	if m == nil {
		return
	}
	m.retrievalChunks.Record(ctx, int64(n))
}

// Embedding records one embedding call to model: how long it took, its input
// tokens when the endpoint reported them (tokens < 0 when it did not), and its
// error, if any.
func (m *Metrics) Embedding(ctx context.Context, model string, took time.Duration, tokens int64, err error) {
	if m == nil {
		return
	}
	common := []attribute.KeyValue{semconv.GenAIOperationNameEmbeddings, semconv.GenAIRequestModel(model)}
	if err != nil {
		et := semconv.ErrorTypeOther
		if errors.Is(err, context.DeadlineExceeded) {
			et = semconv.ErrorTypeKey.String("timeout")
		}
		m.embedDuration.Record(ctx, took.Seconds(), metric.WithAttributes(append(common, et)...))
		return
	}
	m.embedDuration.Record(ctx, took.Seconds(), metric.WithAttributes(common...))
	if tokens >= 0 {
		m.embedTokens.Record(ctx, tokens, metric.WithAttributes(append(common, semconv.GenAITokenTypeInput)...))
	}
}

// ObserveSpend reports the spend ceiling and the tokens remaining under it,
// read from remaining at each collection.
func (m *Metrics) ObserveSpend(ceiling int64, remaining func() int64) error {
	if m == nil {
		return nil
	}
	rem, err := m.meter.Int64ObservableGauge(metricSpendRemaining, metric.WithUnit("{token}"),
		metric.WithDescription("Tokens left under the spend ceiling in the current period."))
	if err != nil {
		return err
	}
	ceil, err := m.meter.Int64ObservableGauge(metricSpendCeiling, metric.WithUnit("{token}"),
		metric.WithDescription("The spend ceiling: tokens allowed per period."))
	if err != nil {
		return err
	}
	_, err = m.meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		o.ObserveInt64(rem, remaining())
		o.ObserveInt64(ceil, ceiling)
		return nil
	}, rem, ceil)
	return err
}
