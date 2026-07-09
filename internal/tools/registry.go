// Package tools is the engine's tool surface. Tools are external, reached over
// MCP: yaad-grove is an MCP client/host, and each instance's config lists the
// MCP servers to connect. Their advertised tools become that bot's callable
// tools, scoped per instance (ADR 0001). Nothing tool-specific is baked into
// the engine.
package tools

import (
	"context"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// ServerConfig points at one MCP server to connect for an instance.
type ServerConfig struct {
	Name    string   // logical name for logs/scoping
	Command string   // how to launch/reach the server (stdio command or URL)
	Args    []string // launch args, if a spawned stdio server
}

// Registry is an MCP client host that connects the configured servers and
// exposes their tools through core.Tools. It keeps the tool surface tiny and
// explicit — the structural half of the grounding guarantee.
type Registry struct {
	servers []ServerConfig
}

// New returns a Registry for the given MCP servers. Connection is deferred to
// Connect so wiring stays cheap.
func New(servers []ServerConfig) *Registry {
	return &Registry{servers: servers}
}

// Connect dials the configured MCP servers and enumerates their tools.
//
// Scaffold: no MCP handshake yet.
func (r *Registry) Connect(ctx context.Context) error {
	return core.ErrNotImplemented
}

// List names the callable tools across all connected servers.
//
// Scaffold: empty until Connect is implemented.
func (r *Registry) List() []string {
	return nil
}

// Call routes a tool call to the server that advertises it.
//
// Scaffold: no routing yet.
func (r *Registry) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	return "", core.ErrNotImplemented
}

// compile-time assertion that Registry satisfies core.Tools.
var _ core.Tools = (*Registry)(nil)
