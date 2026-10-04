package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/yaad-index/bonyan/content"
	bmodel "github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/tool"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// recordingTools are core.Tools that record each call and answer or fail it.
type recordingTools struct {
	defs  []core.ToolDef
	calls []map[string]any
	out   string
	err   error
}

func (r *recordingTools) Defs() []core.ToolDef { return r.defs }

func (r *recordingTools) Call(_ context.Context, _ string, args map[string]any) (string, error) {
	r.calls = append(r.calls, args)
	return r.out, r.err
}

type serverMap map[string]string

func (m serverMap) Server(name string) string { return m[name] }

func someTools() *recordingTools {
	return &recordingTools{defs: []core.ToolDef{
		{Name: "search", Description: "searches the docs", Schema: json.RawMessage(`{"type":"object"}`)},
		{Name: "kb_enumerate", Description: "lists matches"},
	}, out: "found"}
}

// The definitions are the tools' own, schema included.
func TestAgentToolsDefinitions(t *testing.T) {
	a := ForAgent(someTools(), nil)
	assert.Equal(t, []bmodel.ToolDef{
		{Name: "search", Description: "searches the docs", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "kb_enumerate", Description: "lists matches"},
	}, a.Definitions())
	assert.Nil(t, ForAgent(nil, nil).Definitions())
}

// A call reaches the tool with its arguments decoded.
func TestAgentToolsCall(t *testing.T) {
	rt := someTools()
	a := ForAgent(rt, nil)
	out, err := a.Call(context.Background(), bmodel.ToolCall{ID: "c1", Name: "search", Arguments: json.RawMessage(`{"q":"trains","n":2}`)})
	require.NoError(t, err)
	assert.Equal(t, "found", out)
	assert.Equal(t, []map[string]any{{"q": "trains", "n": float64(2)}}, rt.calls)

	_, err = a.Call(context.Background(), bmodel.ToolCall{Name: "kb_enumerate"})
	require.NoError(t, err)
	assert.Nil(t, rt.calls[1], "no arguments")
}

// captureLog sends the default logger to a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// The engine's own tool's failure is its result, the error's text, so the
// model can correct the call; a server's tool's error is returned as it is,
// for bonyan to report by kind. Both are logged, the text only for the
// engine's own tool.
func TestAgentToolsFailure(t *testing.T) {
	log := captureLog(t)
	rt := someTools()
	rt.err = errors.New(`kb_enumerate: unknown dimension "solo" (declared: games, hosts)`)
	a := ForAgent(rt, serverMap{"search": "docs"})

	out, err := a.Call(context.Background(), bmodel.ToolCall{Name: "kb_enumerate", Arguments: json.RawMessage(`{"dimension":"solo","value":"x"}`)})
	require.NoError(t, err)
	assert.Equal(t, `error: kb_enumerate: unknown dimension "solo" (declared: games, hosts)`, out)
	assert.Contains(t, log.String(), `level=WARN msg="tool call failed" tool=kb_enumerate err="kb_enumerate: unknown dimension`)

	log.Reset()
	remote := errors.New("SERVER-TEXT-9d2")
	rt.err = remote
	out, err = a.Call(context.Background(), bmodel.ToolCall{Name: "search", Arguments: json.RawMessage(`{}`)})
	require.ErrorIs(t, err, remote)
	assert.Empty(t, out)
	assert.Contains(t, log.String(), `level=WARN msg="tool call failed" tool=search server=docs`)
	assert.NotContains(t, log.String(), "9d2", "a server's error text is not logged")
}

// A call whose context has ended stays an error, for the engine's own tool
// too: there is no run left to correct it in.
func TestAgentToolsFailureAfterCancel(t *testing.T) {
	captureLog(t)
	rt := someTools()
	rt.err = context.Canceled
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := ForAgent(rt, nil).Call(ctx, bmodel.ToolCall{Name: "kb_enumerate"})
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, out)
}

// A name no tool is advertised under is unknown, and arguments that are not a
// JSON object are invalid; neither reaches a tool.
func TestAgentToolsRefuseBeforeTheTool(t *testing.T) {
	rt := someTools()
	a := ForAgent(rt, nil)
	_, err := a.Call(context.Background(), bmodel.ToolCall{Name: "invented"})
	require.ErrorIs(t, err, tool.ErrUnknown)
	for _, args := range []string{`[1,2]`, `"text"`, `{`} {
		_, err = a.Call(context.Background(), bmodel.ToolCall{Name: "search", Arguments: json.RawMessage(args)})
		require.ErrorIs(t, err, tool.ErrInvalidArguments, args)
	}
	assert.Empty(t, rt.calls)
	_, err = ForAgent(nil, nil).Call(context.Background(), bmodel.ToolCall{Name: "search"})
	require.ErrorIs(t, err, tool.ErrUnknown)
}

// A tool a server serves gives remote tool output from that server; any
// other, the engine's own tool output. No tool needs approval.
func TestAgentToolsSourceAndServer(t *testing.T) {
	a := ForAgent(someTools(), serverMap{"search": "docs"})
	assert.Equal(t, content.KindRemoteTool, a.Source("search"))
	assert.Equal(t, "docs", a.Server("search"))
	assert.Equal(t, content.KindTool, a.Source("kb_enumerate"))
	assert.Equal(t, "", a.Server("kb_enumerate"))
	assert.False(t, a.NeedsApproval("search"))
	assert.Equal(t, content.KindTool, ForAgent(someTools(), nil).Source("search"), "with no servers, every tool is the engine's own")
}
