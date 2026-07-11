// Package tools is the engine's tool surface. Tools are external, reached over
// MCP: yaad-grove is an MCP client/host, and each instance's config lists the
// MCP servers to connect. Their advertised tools become that bot's callable
// tools, scoped per instance (ADR 0001). Nothing tool-specific is baked into
// the engine.
//
// The MCP client is the official github.com/modelcontextprotocol/go-sdk. MCP is
// a stateful JSON-RPC protocol (initialize handshake, capability negotiation,
// request-id correlation over stdio) that will keep evolving, so it's the
// library-first case (ADR 0005) — the same call as the Telegram transport. The
// SDK sits behind core.Tools, so it is swappable in one package.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yaad-index/yaad-grove/internal/core"
)

const clientName = "yaad-grove"

// clientVersion is reported to MCP servers as the client implementation version.
var clientVersion = "dev"

// ServerConfig points at one MCP server to connect for an instance.
type ServerConfig struct {
	Name    string   // logical name for logs/scoping
	Command string   // executable to launch the stdio server
	Args    []string // launch args
}

// toolRef binds an advertised tool to the session that serves it. Description
// and InputSchema are kept for the model-facing tool definitions the engine
// needs (the tool-call loop, a later unit) — the schema is passed through to the
// model as-is; the MCP server validates arguments on its end.
type toolRef struct {
	session     *mcp.ClientSession
	description string
	inputSchema any
}

// Registry is an MCP client host: it connects the configured servers and exposes
// their tools through core.Tools. It keeps the tool surface tiny and explicit —
// the structural half of the grounding guarantee (ADR 0001).
type Registry struct {
	servers []ServerConfig
	client  *mcp.Client

	mu       sync.RWMutex
	sessions []*mcp.ClientSession
	tools    map[string]toolRef // tool name -> serving session
	names    []string           // tool names, in discovery order
}

// New returns a Registry for the given MCP servers. Connection is deferred to
// Connect so wiring stays cheap.
func New(servers []ServerConfig) *Registry {
	return &Registry{
		servers: servers,
		client:  mcp.NewClient(&mcp.Implementation{Name: clientName, Version: clientVersion}, nil),
		tools:   make(map[string]toolRef),
	}
}

// Connect dials each configured MCP server over stdio and enumerates its tools.
// On any failure it tears down the sessions already opened, so a partial connect
// never leaves live subprocesses behind.
func (r *Registry) Connect(ctx context.Context) error {
	for _, s := range r.servers {
		transport := &mcp.CommandTransport{Command: exec.CommandContext(ctx, s.Command, s.Args...)}
		if err := r.connect(ctx, transport); err != nil {
			_ = r.Close()
			return fmt.Errorf("tools: connect %q: %w", s.Name, err)
		}
	}
	return nil
}

// connect establishes one session over transport and registers its tools. It is
// split from Connect (which builds a stdio transport per config) so tests can
// drive it with an in-memory transport against a reference server.
func (r *Registry) connect(ctx context.Context, transport mcp.Transport) error {
	session, err := r.client.Connect(ctx, transport, nil)
	if err != nil {
		return err
	}
	type named struct {
		name string
		ref  toolRef
	}
	var found []named
	params := &mcp.ListToolsParams{}
	for {
		res, err := session.ListTools(ctx, params)
		if err != nil {
			_ = session.Close()
			return err
		}
		for _, t := range res.Tools {
			found = append(found, named{t.Name, toolRef{session: session, description: t.Description, inputSchema: t.InputSchema}})
		}
		if res.NextCursor == "" {
			break
		}
		params.Cursor = res.NextCursor
	}

	r.mu.Lock()
	r.sessions = append(r.sessions, session)
	for _, f := range found {
		if _, dup := r.tools[f.name]; !dup {
			r.names = append(r.names, f.name)
		}
		r.tools[f.name] = f.ref // last server wins on a name collision
	}
	r.mu.Unlock()
	return nil
}

// Defs returns the tool definitions to advertise to the model, in discovery
// order. Each tool's InputSchema (received as decoded JSON) is re-marshaled to a
// json.RawMessage and passed through unchanged — the MCP server owns argument
// validation.
func (r *Registry) Defs() []core.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]core.ToolDef, 0, len(r.names))
	for _, name := range r.names {
		ref := r.tools[name]
		var schema json.RawMessage
		if ref.inputSchema != nil {
			if raw, err := json.Marshal(ref.inputSchema); err == nil {
				schema = raw
			}
		}
		defs = append(defs, core.ToolDef{Name: name, Description: ref.description, Schema: schema})
	}
	return defs
}

// Call routes a tool call to the server advertising it and returns the tool's
// text output. A tool that ran and reported an error (result.IsError) is an
// ordinary error the engine feeds back to the model; a transport-level failure
// (dead session / broken RPC) wraps core.ErrToolUnavailable so the engine aborts
// the loop instead (ADR 0011).
func (r *Registry) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	r.mu.RLock()
	ref, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		// A name the model invented — feed it back so the model can correct, don't
		// abort: an ordinary error, not ErrToolUnavailable.
		return "", fmt.Errorf("tools: unknown tool %q", name)
	}
	res, err := ref.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return "", fmt.Errorf("tools: call %q: %w: %w", name, core.ErrToolUnavailable, err)
	}
	text := flattenContent(res.Content)
	if res.IsError {
		return "", fmt.Errorf("tools: %q reported an error: %s", name, text)
	}
	return text, nil
}

// Close shuts down every open session (terminating the stdio subprocesses). It
// reports the first error but always attempts to close all.
func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var firstErr error
	for _, s := range r.sessions {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	r.sessions = nil
	return firstErr
}

// flattenContent concatenates the text blocks of a tool result, one per line.
// Non-text content (images, resources) is ignored — Phase-1 tools return text.
func flattenContent(content []mcp.Content) string {
	var b strings.Builder
	for _, c := range content {
		tc, ok := c.(*mcp.TextContent)
		if !ok {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(tc.Text)
	}
	return b.String()
}

// compile-time assertion that Registry satisfies core.Tools.
var _ core.Tools = (*Registry)(nil)
