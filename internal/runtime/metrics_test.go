package runtime_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/metrics/metricstest"
	"github.com/yaad-index/yaad-grove/internal/runtime"
	"github.com/yaad-index/yaad-grove/internal/transport"
)

// Each answer the engine gives is counted by how it ended and where it was
// asked.
func TestAnswersAreCountedByOutcome(t *testing.T) {
	cases := []struct {
		name    string
		engine  *mockEngine
		outcome string
	}{
		{"answered", &mockEngine{reply: core.Reply{Text: "ok"}}, "answered"},
		{"refused", &mockEngine{reply: core.Reply{Text: "no", Refused: true}}, "refused"},
		{"at capacity", &mockEngine{err: fmt.Errorf("call: %w", budget.ErrOverBudget)}, "at_capacity"},
		{"error", &mockEngine{err: errors.New("boom")}, "error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, collect := metricstest.New(t)
			h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, tc.engine, nil, nil, nil, nil, nil, runtime.Policy{Metrics: m})
			_, _ = h(context.Background(), inbound)

			got := collect()
			want := []metricstest.Point{{Attrs: map[string]string{"grove.surface": "group", "grove.answer.outcome": tc.outcome}, Value: 1}}
			assert.Equal(t, want, got["grove.answers"].Points)
			require.Len(t, got["grove.answer.duration"].Points, 1)
			assert.Equal(t, int64(1), got["grove.answer.duration"].Points[0].Value)
		})
	}
}

func TestAnAdminDMIsCountedAsDM(t *testing.T) {
	m, collect := metricstest.New(t)
	policy := runtime.Policy{Admins: runtime.NewAdminSet([]string{"admin1"}), Metrics: m}
	h := runtime.NewHandler(&mockGate{}, &mockEngine{reply: core.Reply{Text: "ok"}}, nil, nil, nil, nil, &mockConsenter{consent: acl.ConsentUnknown}, policy)
	_, err := h(context.Background(), transport.Inbound{User: core.User{ID: "admin1"}, Surface: core.SurfaceDM, Text: "q", ReplyTo: "dm-1", Directed: true})
	require.NoError(t, err)
	assert.Equal(t, []metricstest.Point{{Attrs: map[string]string{"grove.surface": "dm", "grove.answer.outcome": "answered"}, Value: 1}},
		collect()["grove.answers"].Points)
}

// A message the engine never answers counts nothing: the gate's nudges,
// silences, throttles and refusals are not answers.
func TestOnlyTheEnginesAnswersAreCounted(t *testing.T) {
	m, collect := metricstest.New(t)
	for _, d := range []acl.Decision{acl.DecideNudge, acl.DecideSilent, acl.DecideRateLimited, acl.DecideRefuse, acl.DecideLogOnly} {
		h := runtime.NewHandler(&mockGate{decision: d}, &mockEngine{reply: core.Reply{Text: "ok"}}, nil, nil, nil, nil, nil, runtime.Policy{Metrics: m})
		_, _ = h(context.Background(), inbound)
	}
	h := runtime.NewHandler(&mockGate{decision: acl.DecideServe}, &mockEngine{reply: core.Reply{Text: "ok"}}, nil, nil, nil, nil, nil, runtime.Policy{Metrics: m})
	_, _ = h(context.Background(), inbound)

	assert.Equal(t, []metricstest.Point{{Attrs: map[string]string{"grove.surface": "group", "grove.answer.outcome": "answered"}, Value: 1}},
		collect()["grove.answers"].Points, "only the served message is counted")
}
