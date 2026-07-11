package tools

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// referenceServer builds an MCP server using the official SDK — the conformance
// oracle. Testing our Registry against the reference implementation (not a
// self-authored mock) proves real protocol interop, deterministically and with
// no external process.
func referenceServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "reference", Version: "v1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "echo the text argument back"},
		func(_ context.Context, _ *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, any, error) {
			text, _ := in["text"].(string)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo: " + text}}}, nil, nil
		})
	mcp.AddTool(s, &mcp.Tool{Name: "boom", Description: "always reports a tool error"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "kaboom"}}}, nil, nil
		})
	return s
}

// connectTo wires the registry to server over in-memory transports (server side
// connected first, per the protocol's initialization ordering).
func connectTo(t *testing.T, r *Registry, server *mcp.Server) {
	t.Helper()
	ctx := context.Background()
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })
	require.NoError(t, r.connect(ctx, clientT))
	t.Cleanup(func() { _ = r.Close() })
}

// Against the reference server: the client enumerates the advertised tools and
// routes a call to one, returning its text.
func TestListAndCall(t *testing.T) {
	r := New(nil)
	connectTo(t, r, referenceServer())

	assert.ElementsMatch(t, []string{"echo", "boom"}, r.List())

	out, err := r.Call(context.Background(), "echo", map[string]any{"text": "hi"})
	require.NoError(t, err)
	assert.Equal(t, "echo: hi", out)
}

// A tool that reports an error (IsError) surfaces as a Go error carrying its
// message.
func TestCallToolError(t *testing.T) {
	r := New(nil)
	connectTo(t, r, referenceServer())

	_, err := r.Call(context.Background(), "boom", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kaboom")
}

// An unknown tool is a clean error, not a panic or a nil-session deref.
func TestCallUnknownTool(t *testing.T) {
	r := New(nil)
	connectTo(t, r, referenceServer())

	_, err := r.Call(context.Background(), "nonexistent", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown tool")
}

// Tools from multiple servers aggregate into one surface; a call routes to the
// server that advertises the tool.
func TestMultipleServersAggregate(t *testing.T) {
	r := New(nil)
	connectTo(t, r, referenceServer())

	// A second server advertising a distinct tool.
	s2 := mcp.NewServer(&mcp.Implementation{Name: "second", Version: "v1"}, nil)
	mcp.AddTool(s2, &mcp.Tool{Name: "ping", Description: "returns pong"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "pong"}}}, nil, nil
		})
	connectTo(t, r, s2)

	assert.ElementsMatch(t, []string{"echo", "boom", "ping"}, r.List())
	out, err := r.Call(context.Background(), "ping", nil)
	require.NoError(t, err)
	assert.Equal(t, "pong", out)
}

// A registry with no servers connects to nothing and lists nothing — no panic.
func TestEmptyRegistry(t *testing.T) {
	r := New(nil)
	require.NoError(t, r.Connect(context.Background()))
	assert.Empty(t, r.List())
	require.NoError(t, r.Close())
}
