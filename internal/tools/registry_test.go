package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/core"
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
// connected first, per the protocol's initialization ordering). An optional
// ServerConfig supplies the allow/deny filter; omitted means expose everything.
func connectTo(t *testing.T, r *Registry, server *mcp.Server, cfg ...ServerConfig) {
	t.Helper()
	ctx := context.Background()
	c := ServerConfig{}
	if len(cfg) > 0 {
		c = cfg[0]
	}
	clientT, serverT := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverT, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })
	require.NoError(t, r.connect(ctx, clientT, c))
	t.Cleanup(func() { _ = r.Close() })
}

// Against the reference server: the client enumerates the advertised tools and
// routes a call to one, returning its text.
func TestListAndCall(t *testing.T) {
	r := New(nil)
	connectTo(t, r, referenceServer())

	assert.ElementsMatch(t, []string{"echo", "boom"}, defNames(r))
	// Defs carry the description advertised by the server, for the model.
	for _, d := range r.Defs() {
		if d.Name == "echo" {
			assert.Equal(t, "echo the text argument back", d.Description)
		}
	}

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

	assert.ElementsMatch(t, []string{"echo", "boom", "ping"}, defNames(r))
	out, err := r.Call(context.Background(), "ping", nil)
	require.NoError(t, err)
	assert.Equal(t, "pong", out)
}

// An allow-list exposes ONLY its tools: a dropped tool is absent from Defs (not
// advertised) AND rejected by Call as unknown (not routable) — closing the
// invented-name hole (#87).
func TestConnectAllowList(t *testing.T) {
	r := New(nil)
	connectTo(t, r, referenceServer(), ServerConfig{Name: "ref", Allow: []string{"echo"}})

	assert.ElementsMatch(t, []string{"echo"}, defNames(r), "only the allow-listed tool is advertised")

	// The dropped tool is not callable even by exact name.
	_, err := r.Call(context.Background(), "boom", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown tool", "a filtered tool is unroutable, not just unadvertised")

	// The allowed tool still works.
	out, err := r.Call(context.Background(), "echo", map[string]any{"text": "hi"})
	require.NoError(t, err)
	assert.Equal(t, "echo: hi", out)
}

// A deny-list drops its tools and exposes the rest.
func TestConnectDenyList(t *testing.T) {
	r := New(nil)
	connectTo(t, r, referenceServer(), ServerConfig{Name: "ref", Deny: []string{"boom"}})

	assert.ElementsMatch(t, []string{"echo"}, defNames(r), "the denied tool is dropped, the rest exposed")
	_, err := r.Call(context.Background(), "boom", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown tool")
}

// No allow/deny list exposes everything (backwards compatible).
func TestConnectNoListExposesAll(t *testing.T) {
	r := New(nil)
	connectTo(t, r, referenceServer(), ServerConfig{Name: "ref"})
	assert.ElementsMatch(t, []string{"echo", "boom"}, defNames(r))
}

// permits: allow is exclusive and takes precedence; deny is subtractive; neither
// exposes all.
func TestServerConfigPermits(t *testing.T) {
	assert.True(t, ServerConfig{}.permits("anything"), "no list → all permitted")

	allow := ServerConfig{Allow: []string{"a", "b"}}
	assert.True(t, allow.permits("a"))
	assert.False(t, allow.permits("c"), "allow is exclusive")

	deny := ServerConfig{Deny: []string{"x"}}
	assert.False(t, deny.permits("x"))
	assert.True(t, deny.permits("y"), "deny is subtractive")

	// Defensive: if both are somehow set, allow wins (config parsing rejects this,
	// but the predicate must still fail closed to the exclusive list).
	both := ServerConfig{Allow: []string{"a"}, Deny: []string{"a"}}
	assert.True(t, both.permits("a"))
	assert.False(t, both.permits("b"))
}

// A registry with no servers connects to nothing and lists nothing — no panic.
func TestEmptyRegistry(t *testing.T) {
	r := New(nil)
	require.NoError(t, r.Connect(context.Background()))
	assert.Empty(t, r.Defs())
	require.NoError(t, r.Close())
}

// defNames extracts the tool names from the registry's defs.
func defNames(r *Registry) []string {
	var names []string
	for _, d := range r.Defs() {
		names = append(names, d.Name)
	}
	return names
}

type strictArgs struct {
	IDs []int `json:"ids"`
}

// strictServer advertises a tool whose schema requires integer ids, so the SDK
// rejects a null/wrong-type id with a JSON-RPC invalid-params response — the
// exact shape of the deployed get_things failure (#147).
func strictServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "strict", Version: "v1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "get_things", Description: "fetch by integer ids"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ strictArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		})
	return s
}

// A call the server REJECTS via schema validation (JSON-RPC invalid params) is
// something the model can retry with valid args — it must surface as an ordinary
// error that feeds back, NOT ErrToolUnavailable (which aborts the whole turn).
// Regression for #147: a null id where an integer is required dead-ended the turn.
func TestCallInvalidArgsFeedsBackNotUnavailable(t *testing.T) {
	r := New(nil)
	connectTo(t, r, strictServer())

	_, err := r.Call(context.Background(), "get_things", map[string]any{"ids": []any{nil}})
	require.Error(t, err)
	assert.False(t, errors.Is(err, core.ErrToolUnavailable),
		"a rejected call (invalid params) feeds back to the model, it does not abort the turn")
	assert.Contains(t, err.Error(), "get_things")
}
