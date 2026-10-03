// Package runtime is the composition boundary: it wires the core engine to the
// cross-cutting concerns that the engine deliberately does not depend on (ADR
// 0008). It imports core, budget, and model — none of which import it — so it is
// the one place the spend ceiling (and, in the transport unit, the consent gate)
// composes around the pure engine.
package runtime

import (
	"context"
	"log/slog"

	bmodel "github.com/yaad-index/bonyan/model"

	"github.com/yaad-index/yaad-grove/internal/budget"
)

// meteredChat puts the global spend ceiling on the model-call path (ADR
// 0006/0008): it checks the meter before each call and records the actual
// token usage after. It wraps the chat model the engine's agent run answers
// with, so the engine stays free of budget.
type meteredChat struct {
	inner bmodel.Chat
	meter *budget.Meter
}

// MeterChat wraps inner so every call is gated and accounted against the
// spend meter. Over budget, it returns budget.ErrOverBudget without calling
// inner; on success it records the reply's input and output tokens.
func MeterChat(meter *budget.Meter, inner bmodel.Chat) bmodel.Chat {
	return &meteredChat{inner: inner, meter: meter}
}

// Chat refuses when the spend ceiling is reached (no underlying call), else
// calls and records the usage. It gates every step of the run, tool calls
// included (ADR 0011), so a multi-call answer is bounded by the ceiling.
func (m *meteredChat) Chat(ctx context.Context, req bmodel.ChatRequest) (bmodel.ChatResponse, error) {
	if !m.meter.Allow() {
		return bmodel.ChatResponse{}, budget.ErrOverBudget
	}
	resp, err := m.inner.Chat(ctx, req)
	if err != nil {
		return bmodel.ChatResponse{}, err
	}
	// Record after a successful (already-paid) call. A record failure is a
	// persistence lag, not an in-memory undercount (the meter incremented before
	// the store write), so log it and still return the answer rather than discard a
	// paid reply (ADR 0008).
	var used int64
	if resp.Usage != nil {
		used = resp.Usage.InputTokens + resp.Usage.OutputTokens
	}
	if rerr := m.meter.Record(used); rerr != nil {
		slog.Warn("spend record failed after a successful completion", "err", rerr)
	}
	return resp, nil
}
