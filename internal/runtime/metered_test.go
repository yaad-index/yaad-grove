package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/runtime"
)

type spyModel struct {
	calls int
	usage int
	err   error
}

func (s *spyModel) Complete(context.Context, []core.Message, []core.ToolDef) (core.Completion, error) {
	s.calls++
	return core.Completion{Text: "ok", Usage: core.Usage{TotalTokens: s.usage}}, s.err
}

func newMeter(t *testing.T, ceiling int64) *budget.Meter {
	t.Helper()
	m, err := budget.New(budget.Config{Ceiling: ceiling, Period: time.Hour}, &budget.MemoryStore{})
	require.NoError(t, err)
	return m
}

// A successful completion records its TotalTokens against the meter.
func TestMeteredRecordsUsage(t *testing.T) {
	meter := newMeter(t, 100)
	spy := &spyModel{usage: 30}
	m := runtime.MeterModel(meter, spy)

	c, err := m.Complete(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "ok", c.Text)
	assert.Equal(t, 1, spy.calls)
	assert.Equal(t, int64(70), meter.Remaining(), "usage recorded against the meter")
}

// Over budget, the decorator returns the typed error and never calls the model.
func TestMeteredBlocksOverBudget(t *testing.T) {
	meter := newMeter(t, 10)
	require.NoError(t, meter.Record(10)) // exhaust the ceiling
	spy := &spyModel{usage: 5}
	m := runtime.MeterModel(meter, spy)

	_, err := m.Complete(context.Background(), nil, nil)
	require.ErrorIs(t, err, budget.ErrOverBudget)
	assert.Equal(t, 0, spy.calls, "no underlying call when over budget")
}

// A failed completion propagates the error and records nothing (no spend).
func TestMeteredPropagatesInnerError(t *testing.T) {
	meter := newMeter(t, 100)
	spy := &spyModel{err: errors.New("boom")}
	m := runtime.MeterModel(meter, spy)

	_, err := m.Complete(context.Background(), nil, nil)
	assert.Error(t, err)
	assert.Equal(t, int64(100), meter.Remaining(), "a failed call records no spend")
}
