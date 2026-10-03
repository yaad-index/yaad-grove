package runtime_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bmodel "github.com/yaad-index/bonyan/model"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/runtime"
	"github.com/yaad-index/yaad-grove/internal/tools"
)

// stepModel asks for a tool on its first call and answers on the next, each
// call using the given tokens.
type stepModel struct {
	calls  int
	tokens int64
}

func (m *stepModel) Chat(context.Context, bmodel.ChatRequest) (bmodel.ChatResponse, error) {
	m.calls++
	usage := &bmodel.Usage{InputTokens: m.tokens, OutputTokens: 0}
	if m.calls == 1 {
		return bmodel.ChatResponse{ToolCalls: []bmodel.ToolCall{{ID: "c1", Name: "search", Arguments: json.RawMessage(`{}`)}}, StopReason: bmodel.StopToolCalls, Usage: usage}, nil
	}
	return bmodel.ChatResponse{Content: "answer", StopReason: bmodel.StopEnd, Usage: usage}, nil
}

type searchTool struct{}

func (searchTool) Defs() []core.ToolDef {
	return []core.ToolDef{{Name: "search", Description: "search", Schema: json.RawMessage(`{"type":"object"}`)}}
}

func (searchTool) Call(context.Context, string, map[string]any) (string, error) { return "found", nil }

type oneChunk struct{}

func (oneChunk) Retrieve(context.Context, string) ([]core.Chunk, error) {
	return []core.Chunk{{Source: "a.md", Text: "widget info"}}, nil
}

func meteredEngine(meter *budget.Meter, mdl bmodel.Chat) *core.Engine {
	return core.New(runtime.MeterChat(meter, mdl), "m", oneChunk{}, tools.ForAgent(searchTool{}, nil), "scope")
}

// With the spend ceiling already reached, an answer through the real meter and
// agent run makes no model call and ends in the over-budget error, which the
// handler turns into the capacity reply.
func TestOverBudgetBeforeTheFirstCall(t *testing.T) {
	meter := newMeter(t, 10)
	require.NoError(t, meter.Record(10))
	mdl := &stepModel{tokens: 1}

	_, err := meteredEngine(meter, mdl).Answer(context.Background(), core.Query{Text: "q"})
	require.ErrorIs(t, err, budget.ErrOverBudget)
	assert.Equal(t, 0, mdl.calls)

	reply, err := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, meteredEngine(meter, mdl), nil, nil, nil, nil, nil, runtime.Policy{})(context.Background(), inbound)
	require.NoError(t, err)
	assert.True(t, reply.Refused)
	assert.Contains(t, reply.Text, "capacity")
	assert.Equal(t, 0, mdl.calls)
}

// A run whose first step spends the rest of the ceiling is stopped before its
// next step: the tool's result never reaches a second model call.
func TestOverBudgetBetweenSteps(t *testing.T) {
	meter := newMeter(t, 10)
	mdl := &stepModel{tokens: 10}

	_, err := meteredEngine(meter, mdl).Answer(context.Background(), core.Query{Text: "q"})
	require.ErrorIs(t, err, budget.ErrOverBudget)
	assert.Equal(t, 1, mdl.calls, "the second step was refused by the meter")
}
