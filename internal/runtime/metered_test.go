package runtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bmodel "github.com/yaad-index/bonyan/model"

	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/runtime"
)

type spyModel struct {
	calls int
	usage int64
	err   error
}

func (s *spyModel) Chat(context.Context, bmodel.ChatRequest) (bmodel.ChatResponse, error) {
	s.calls++
	return bmodel.ChatResponse{Content: "ok", Usage: &bmodel.Usage{InputTokens: s.usage - s.usage/3, OutputTokens: s.usage / 3}}, s.err
}

func newMeter(t *testing.T, ceiling int64) *budget.Meter {
	t.Helper()
	m, err := budget.New(budget.Config{Ceiling: ceiling, Period: time.Hour}, &budget.MemoryStore{})
	require.NoError(t, err)
	return m
}

// A successful call records its input and output tokens against the meter.
func TestMeteredRecordsUsage(t *testing.T) {
	meter := newMeter(t, 100)
	spy := &spyModel{usage: 30}
	m := runtime.MeterChat(meter, spy)

	c, err := m.Chat(context.Background(), bmodel.ChatRequest{})
	require.NoError(t, err)
	assert.Equal(t, "ok", c.Content)
	assert.Equal(t, 1, spy.calls)
	assert.Equal(t, int64(70), meter.Remaining(), "usage recorded against the meter")
}

// Over budget, the decorator returns the typed error and never calls the model.
func TestMeteredBlocksOverBudget(t *testing.T) {
	meter := newMeter(t, 10)
	require.NoError(t, meter.Record(10)) // exhaust the ceiling
	spy := &spyModel{usage: 5}
	m := runtime.MeterChat(meter, spy)

	_, err := m.Chat(context.Background(), bmodel.ChatRequest{})
	require.ErrorIs(t, err, budget.ErrOverBudget)
	assert.Equal(t, 0, spy.calls, "no underlying call when over budget")
}

// A failed completion propagates the error and records nothing (no spend).
func TestMeteredPropagatesInnerError(t *testing.T) {
	meter := newMeter(t, 100)
	spy := &spyModel{err: errors.New("boom")}
	m := runtime.MeterChat(meter, spy)

	_, err := m.Chat(context.Background(), bmodel.ChatRequest{})
	assert.Error(t, err)
	assert.Equal(t, int64(100), meter.Remaining(), "a failed call records no spend")
}
