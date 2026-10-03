package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/content"
	bmodel "github.com/yaad-index/bonyan/model"
	"github.com/yaad-index/bonyan/tool"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// Servers names the server serving a tool: the Registry does. A tool it names
// no server for is the engine's own.
type Servers interface {
	Server(name string) string
}

// ForAgent presents t as the tools of a bonyan agent run: the same tools,
// definitions and calls, with each tool's results classified by where they
// come from: a tool a server in servers serves is remote tool output, from
// that server, and any other, such as kb_enumerate, is the program's own
// tool output. servers may be nil when every tool is the engine's own. No
// tool waits for approval.
func ForAgent(t core.Tools, servers Servers) *AgentTools {
	return &AgentTools{t: t, servers: servers}
}

// AgentTools is core.Tools as bonyan's agent.Tools and agent.Servers.
type AgentTools struct {
	t       core.Tools
	servers Servers
}

var (
	_ agent.Tools   = (*AgentTools)(nil)
	_ agent.Servers = (*AgentTools)(nil)
)

// Definitions describes the tools to the model.
func (a *AgentTools) Definitions() []bmodel.ToolDef {
	if a.t == nil {
		return nil
	}
	defs := a.t.Defs()
	out := make([]bmodel.ToolDef, len(defs))
	for i, d := range defs {
		out[i] = bmodel.ToolDef{Name: d.Name, Description: d.Description, Parameters: d.Schema}
	}
	return out
}

// Call runs one tool call. A name no tool is advertised under is
// tool.ErrUnknown, and arguments that are not a JSON object are
// tool.ErrInvalidArguments; the tool's own errors are returned as they are.
func (a *AgentTools) Call(ctx context.Context, call bmodel.ToolCall) (string, error) {
	if a.t == nil || !slices.ContainsFunc(a.t.Defs(), func(d core.ToolDef) bool { return d.Name == call.Name }) {
		return "", fmt.Errorf("%w: %q", tool.ErrUnknown, call.Name)
	}
	var args map[string]any
	if len(call.Arguments) > 0 {
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return "", fmt.Errorf("%w: the arguments are not a JSON object", tool.ErrInvalidArguments)
		}
	}
	return a.t.Call(ctx, call.Name, args)
}

// Source is the kind the trust policy classifies the named tool's results
// under.
func (a *AgentTools) Source(name string) content.Kind {
	if a.Server(name) != "" {
		return content.KindRemoteTool
	}
	return content.KindTool
}

// Server is the name of the server serving the named tool, or "".
func (a *AgentTools) Server(name string) string {
	if a.servers == nil {
		return ""
	}
	return a.servers.Server(name)
}

// NeedsApproval is false: no tool here waits for approval.
func (a *AgentTools) NeedsApproval(string) bool { return false }
