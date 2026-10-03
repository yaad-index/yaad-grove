package tools

import (
	"context"
	"encoding/json"
	"errors"
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

// A call reaches the tool with its arguments decoded; a tool's own error comes
// back as it is.
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

	failure := errors.New("the server failed")
	rt.err = failure
	_, err = a.Call(context.Background(), bmodel.ToolCall{Name: "search", Arguments: json.RawMessage(`{}`)})
	require.ErrorIs(t, err, failure)
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
