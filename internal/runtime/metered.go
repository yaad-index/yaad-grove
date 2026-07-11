// Package runtime is the composition boundary: it wires the core engine to the
// cross-cutting concerns that the engine deliberately does not depend on (ADR
// 0008). It imports core, budget, and model — none of which import it — so it is
// the one place the spend ceiling (and, in the transport unit, the consent gate)
// composes around the pure engine.
package runtime

import (
	"context"
	"log/slog"

	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
)

// meteredModel puts the global spend ceiling on the model-call path (ADR
// 0006/0008): it checks the meter before each completion and records the actual
// token usage after. It is a core.Model, injected as the engine's model, so the
// engine stays free of budget.
type meteredModel struct {
	inner core.Model
	meter *budget.Meter
}

// MeterModel wraps inner so every completion is gated and accounted against the
// spend meter. Over budget, it returns budget.ErrOverBudget without calling
// inner; on success it records the response's TotalTokens.
func MeterModel(meter *budget.Meter, inner core.Model) core.Model {
	return &meteredModel{inner: inner, meter: meter}
}

// Complete refuses when the spend ceiling is reached (no underlying call), else
// completes and records the usage. It gates every round of the tool-call loop
// (ADR 0011), so a multi-call answer is naturally bounded by the ceiling.
func (m *meteredModel) Complete(ctx context.Context, messages []core.Message, tools []core.ToolDef) (core.Completion, error) {
	if !m.meter.Allow() {
		return core.Completion{}, budget.ErrOverBudget
	}
	completion, err := m.inner.Complete(ctx, messages, tools)
	if err != nil {
		return core.Completion{}, err
	}
	// Record after a successful (already-paid) call. A record failure is a
	// persistence lag, not an in-memory undercount (the meter incremented before
	// the store write), so log it and still return the answer rather than discard a
	// paid completion (ADR 0008).
	if rerr := m.meter.Record(int64(completion.Usage.TotalTokens)); rerr != nil {
		slog.Warn("spend record failed after a successful completion", "err", rerr)
	}
	return completion, nil
}
