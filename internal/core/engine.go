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
	"strings"
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
	// Silent is true when there is deliberately no outbound message — the runtime
	// sets it for the throttled-unconsented case (acl.DecideSilent, ADR 0007), and
	// a transport skips Send when it is set.
	Silent bool
	// Actions are interactive affordances offered alongside the text: a transport
	// renders them as buttons (Telegram inline keyboard) where it can, and
	// degrades to an enumerated text list where it cannot (CapButtons, ADR 0009).
	// Empty leaves the reply plain text — fully backward-compatible.
	Actions []Action
	// Notice is an ephemeral acknowledgement shown to the actor in place, not as a
	// new message — a Telegram answerCallbackQuery toast, later a Slack/Discord
	// ephemeral reply. The runtime sets it to answer a button click (ADR 0009);
	// it is empty for ordinary message replies.
	Notice string
}

// Action is an interactive affordance offered on a Reply — one button. It is a
// typed operation the actor can invoke with a tap rather than by retyping a
// command (ADR 0009). This is the wire shape: what a transport needs to render
// the button and round-trip the click. The authorizing/executing half — a
// minimum tier and an executor bound to the Verb — arrives with the action
// registry; a button is only ever a UI hint, re-authorized at execution time.
type Action struct {
	// Verb names the operation (the registry maps it to an executor and a
	// minimum tier). The echo action's verb is unprivileged.
	Verb string
	// Params carries the verb's arguments. String-valued so it round-trips
	// cleanly through the callback token store.
	Params map[string]string
	// Label is the button caption shown to the user.
	Label string
}

// Usage is the token accounting for a model call — what the global spend meter
// (ADR 0006) records. An OpenAI-compatible response reports these in its `usage`
// field.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
}

// Completion is a model call's result: the generated text plus the token usage.
// Usage travels with the text so the model-call path can Record actual spend
// against the ceiling (ADR 0006) — the meter needs the real usage, known only
// from the response.
type Completion struct {
	Text  string
	Usage Usage
}

// Model is an OpenAI-compatible chat completion model. The engine depends only
// on this interface; the concrete client (endpoint, key, model name from
// config) lives in internal/model and can be any OpenAI-shaped provider. Complete
// returns the generated text and the call's token usage (ADR 0006).
type Model interface {
	Complete(ctx context.Context, system, user string) (Completion, error)
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

// RefusalToken is the sentinel the scope/system prompt instructs the model to
// emit — alone — when the retrieved context does not support an answer (ADR
// 0008). Answer detects it and returns a refusal instead of the model's text.
const RefusalToken = "%%OUT_OF_SCOPE%%"

// outOfScopeReply is the fixed user-facing text for a refusal (both the empty-
// grounding short-circuit and the model-signalled sentinel), so a refusal never
// leaks prompt internals.
const outOfScopeReply = "That's outside what I can answer from my curated sources."

// Answer grounds q on the retrieved vault context and returns a Reply, or a
// refusal when it can't be grounded (ADR 0008). The flow: retrieve -> if nothing
// grounds it, refuse without a model call -> else assemble the grounded prompt
// (scope + chunks) and complete -> if the model signals out-of-scope, refuse.
//
// The engine depends only on its interfaces (ADR 0001): the spend ceiling is
// applied by a metered Model decorator (ADR 0006/0008), and the consent gate runs
// ahead of Answer at the runtime boundary (ADR 0007) — neither lives here.
//
// Tool calls are a documented seam: the engine carries a Tools registry, but the
// MCP call loop lands with the transport/tools unit; Answer is retrieval-grounded
// for now.
func (e *Engine) Answer(ctx context.Context, q Query) (Reply, error) {
	chunks, err := e.retriever.Retrieve(ctx, q.Text)
	if err != nil {
		return Reply{}, err
	}
	// Empty-grounding short-circuit: nothing to ground on -> refuse without
	// spending a model call.
	if len(chunks) == 0 {
		return Reply{Text: outOfScopeReply, Refused: true}, nil
	}

	completion, err := e.model.Complete(ctx, groundedSystemPrompt(e.scope, chunks), q.Text)
	if err != nil {
		return Reply{}, err
	}
	// Model-signalled refusal: the scope prompt asks the model to emit the
	// sentinel when the context can't answer.
	if strings.Contains(completion.Text, RefusalToken) {
		return Reply{Text: outOfScopeReply, Refused: true}, nil
	}
	return Reply{Text: completion.Text}, nil
}

// groundedSystemPrompt assembles the scope statement, the refusal contract, and
// the retrieved chunks (each tagged with its Source for citation) into the system
// message (ADR 0008). Plain, deterministic assembly — chunks in the order the
// retriever ranked them.
func groundedSystemPrompt(scope string, chunks []Chunk) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(scope))
	b.WriteString("\n\nAnswer using ONLY the CONTEXT below, and cite the [source] tags you rely on. " +
		"If the context does not contain the answer, reply with exactly " + RefusalToken + " and nothing else.\n\n" +
		"CONTEXT:\n")
	for _, c := range chunks {
		b.WriteString("\n[" + c.Source + "]\n")
		b.WriteString(strings.TrimSpace(c.Text))
		b.WriteString("\n")
	}
	return b.String()
}
