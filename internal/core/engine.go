// Package core is the transport-agnostic heart of yaad-grove: one Answer
// function that grounds a query on a curated vault plus external tools and
// refuses anything outside that scope.
//
// Nothing in this package knows about any transport (Telegram, Discord, ...),
// any concrete model provider, or any specific tool. Those all arrive as
// interfaces (Model, Retriever, Tools) and are wired in cmd/yaad-grove. This is
// the boundary that makes the engine generic from day one (ADR 0001): a bot is
// just (vault + tools + scope + transport), and only this package defines what
// "answer" means.
package core

import (
	"context"
	"errors"
)

// ErrNotImplemented marks scaffold stubs that have structure but no behavior
// yet. Every such stub returns it so an accidental early call fails loudly
// instead of silently returning a zero value.
var ErrNotImplemented = errors.New("yaad-grove: not implemented (scaffold)")

// Surface is where a query reached the bot from. It matters for access control:
// a group is bounded by its membership, a DM is unbounded and needs an explicit
// admin allowlist (ADR 0001, access model).
type Surface int

const (
	// SurfaceGroup is a message in a community group the bot is enabled in.
	SurfaceGroup Surface = iota
	// SurfaceDM is a one-to-one direct message to the bot.
	SurfaceDM
)

// User is the platform-neutral identity behind a query. The persistent
// per-user state (consent, tier, rate counters, DM approval) lives in the ACL
// store, keyed by ID; this is only the handle the engine passes around.
type User struct {
	// ID is the platform-scoped user id (e.g. Telegram user id). It is the key
	// into the ACL store.
	ID string
	// Display is a best-effort human label for logs and prompts; never trusted
	// for identity.
	Display string
}

// Query is a single inbound request, normalized by a transport adapter into
// this platform-neutral shape before it reaches the engine.
type Query struct {
	User    User
	Surface Surface
	Text    string
}

// Reply is the engine's platform-neutral response. A transport adapter renders
// it onto its platform (text, and later capability-mapped extras like
// reactions, degrading gracefully where unsupported).
type Reply struct {
	Text string
	// Refused is true when the query fell outside scope and was declined rather
	// than answered. The grounding guarantee (ADR 0001) is structural: a tiny
	// tool surface + scoped prompt + refusal leave the model nowhere to
	// freelance from.
	Refused bool
}

// Model is an OpenAI-compatible chat completion model. The engine depends only
// on this interface; the concrete client (endpoint, key, model name from
// config) lives in internal/model and can be any OpenAI-shaped provider.
type Model interface {
	Complete(ctx context.Context, system, user string) (string, error)
}

// Chunk is a retrieved piece of the curated vault, with its source for
// attribution in the answer.
type Chunk struct {
	Source string
	Text   string
}

// Retriever returns vault chunks relevant to a query. Phase 1 is plain
// full-text over a small corpus; an embedding-backed implementation can replace
// it behind this same interface with no engine change (ADR 0001).
type Retriever interface {
	Retrieve(ctx context.Context, query string) ([]Chunk, error)
}

// Tools is the per-instance registry of external tools the engine may call.
// Tools are not built into the engine: it is an MCP client, and each instance's
// config lists which MCP servers to connect. Their tools become this bot's
// tools, scoped per instance (ADR 0001).
type Tools interface {
	// List names the callable tools, for prompt construction and scoping.
	List() []string
	// Call invokes a named tool with JSON-ish arguments and returns its result.
	Call(ctx context.Context, name string, args map[string]any) (string, error)
}

// Engine answers queries grounded on a Retriever's chunks and Tools' results,
// driven by a Model, and refuses out-of-scope input. It is the only place that
// defines answering; everything else adapts into or out of it.
type Engine struct {
	model     Model
	retriever Retriever
	tools     Tools
	// scope is the instance's system prompt / scope statement that bounds the
	// bot and drives refusal. Loaded from config.
	scope string
}

// New wires an engine from its collaborators. All are interfaces so the core
// carries zero transport, provider, or tool dependencies.
func New(model Model, retriever Retriever, tools Tools, scope string) *Engine {
	return &Engine{model: model, retriever: retriever, tools: tools, scope: scope}
}

// Answer grounds q on retrieved context and tool results and returns a Reply,
// or a refusal when out of scope.
//
// Scaffold: structure only. The Phase-1 flow will be
// retrieve -> assemble grounded prompt (scope + chunks + tool results) ->
// model.Complete -> refuse-if-unsupported.
func (e *Engine) Answer(ctx context.Context, q Query) (Reply, error) {
	return Reply{}, ErrNotImplemented
}
