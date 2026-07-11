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
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"text/template"
	"time"
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
	// History is the recent-conversation context to inject (ADR 0014): prior
	// turns the runtime selected for this query, already ordered chronologically.
	// Empty means no history (a standalone question or a disabled buffer). It is
	// context, never a source of facts — grounding still governs factual claims.
	History []HistoryTurn
}

// HistoryTurn is one prior conversation turn injected into the answer prompt as
// context (ADR 0014). It is the core-level view of a memory-buffer turn — enough
// to render a threaded, timestamped, speaker-attributed line — with no dependency
// on the memory subsystem; the runtime converts buffer turns into these.
type HistoryTurn struct {
	// Speaker is the human display label; empty (with Bot) renders as the assistant.
	Speaker string
	// Bot marks the bot's own prior answer.
	Bot bool
	// Text is the turn's content.
	Text string
	// Time orders and timestamps the turn in the injected block.
	Time time.Time
	// MessageID is this turn's id — a target another turn's ReplyTo may point at.
	MessageID string
	// ReplyTo is the MessageID this turn replies to, or empty. A ReplyTo whose
	// target is not among the injected turns renders as "a message not shown".
	ReplyTo string
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
	// Reaction is an emoji the transport attaches to the message that triggered
	// this reply, rather than sending a new message — a Telegram setMessageReaction
	// (CapReactions, ADR 0012). The runtime sets it for a reaction-mode consent
	// nudge; empty leaves the reply a normal message. A transport without reactions
	// never sees it: the runtime downgrades reaction-mode to text at wiring time.
	Reaction string
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

// Completion is a model call's result. It is either a final answer (Text) or a
// request to run one or more tools (ToolCalls) — never both meaningfully; the
// engine loops, running the tools and calling again, until the model returns
// text (ADR 0011). Usage travels with it so the model-call path can Record actual
// spend against the ceiling (ADR 0006).
type Completion struct {
	Text      string
	ToolCalls []ToolCall
	Usage     Usage
}

// Role is a conversation turn's author in the model exchange.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one turn in the model conversation. Assistant turns may carry
// ToolCalls (the model's tool requests); tool turns carry a ToolCallID naming the
// request they answer — the two must correlate (ADR 0011).
type Message struct {
	Role       Role
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
}

// ToolDef is a callable tool advertised to the model: its name, a description,
// and the JSON Schema for its arguments. The schema is passed through to the
// model as-is; the MCP server validates arguments on its end (no client-side
// schema handling — ADR 0011).
type ToolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// ToolCall is the model's request to invoke a tool with arguments.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

// Model is an OpenAI-compatible chat model. The engine depends only on this
// interface; the concrete client lives in internal/model. Complete runs one
// round of the conversation with the available tools and returns either a final
// text answer or the tools the model wants to call, plus the call's usage.
type Model interface {
	Complete(ctx context.Context, messages []Message, tools []ToolDef) (Completion, error)
}

// ErrToolUnavailable marks a tool *call* that failed at the transport level (a
// dead MCP session, a broken RPC) rather than a tool that ran and reported an
// error. The engine aborts the loop on it — it is infrastructure the model can't
// reason its way around — whereas a tool-reported failure is fed back as content
// so the model can adapt (ADR 0011).
var ErrToolUnavailable = errors.New("core: tool call unavailable")

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
	// Defs returns the callable tool definitions to advertise to the model.
	Defs() []ToolDef
	// Call invokes a named tool with arguments and returns its text result. A
	// transport-level failure (dead session) wraps ErrToolUnavailable; a
	// tool-reported failure is an ordinary error.
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
	// persona is the optional operator-authored behavioral layer (ADR 0013): it
	// shapes voice, social handling, and refusal wording, and is prepended to the
	// system prompt before scope. Empty = no persona layer (current behavior). It
	// never relaxes scope or grounding — the grounding instruction that follows it
	// overrides any persona guidance that would.
	persona string
	// prompt is the optional operator-supplied grounding template (ADR 0016); nil
	// uses the embedded default, which reproduces the prior prompt byte-for-byte.
	prompt *template.Template
}

// Option configures an Engine at construction. Options keep New's required
// collaborators positional while letting optional layers (like persona) be added
// without breaking existing callers.
type Option func(*Engine)

// WithPersona sets the operator-authored persona layer (ADR 0013). Empty is a
// no-op, so a deployment without a persona file behaves exactly as before.
func WithPersona(persona string) Option {
	return func(e *Engine) { e.persona = persona }
}

// WithPromptTemplate sets the operator-supplied grounding template (ADR 0016). A
// nil template is a no-op — the engine renders the embedded default, byte-for-byte
// identical to the prior prompt.
func WithPromptTemplate(t *template.Template) Option {
	return func(e *Engine) { e.prompt = t }
}

// New wires an engine from its collaborators. All are interfaces so the core
// carries zero transport, provider, or tool dependencies.
func New(model Model, retriever Retriever, tools Tools, scope string, opts ...Option) *Engine {
	e := &Engine{model: model, retriever: retriever, tools: tools, scope: scope}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// RefusalToken is the sentinel the scope/system prompt instructs the model to
// emit — alone — when the retrieved context does not support an answer (ADR
// 0008). Answer detects it and returns a refusal instead of the model's text.
const RefusalToken = "%%OUT_OF_SCOPE%%"

// outOfScopeReply is the fallback refusal text: the empty-grounding short-circuit
// (no model call to shape it) and a model that emitted only the bare sentinel both
// use it, so a refusal never leaks prompt internals.
const outOfScopeReply = "That's outside what I can answer from my curated sources."

// parseRefusal interprets a final model reply against the out-of-scope sentinel
// (ADR 0008/0013). The model is instructed to LEAD an off-domain or ungroundable
// reply with the token, then a brief in-persona note of what it can help with —
// so the token is expected first. After trimming leading whitespace, a prefix
// match is a refusal: the marker is stripped and the persona note surfaced (or the
// fallback line if the model wrote none), with Refused set.
//
// Detection is prefix-only on purpose: a token buried mid-reply is an instruction
// violation, not a refusal — it is logged (observable, never silently wrong) and
// the reply is treated as a normal answer, with any stray marker stripped so no
// raw token reaches the user.
func parseRefusal(text string) (reply string, refused bool) {
	lead := strings.TrimLeft(text, " \t\r\n")
	if strings.HasPrefix(lead, RefusalToken) {
		note := strings.TrimSpace(strings.TrimPrefix(lead, RefusalToken))
		if note == "" {
			note = outOfScopeReply
		}
		return note, true
	}
	if strings.Contains(text, RefusalToken) {
		slog.Warn("refusal sentinel not at reply start; treating as a normal answer", "reply", text)
		text = strings.TrimSpace(strings.ReplaceAll(text, RefusalToken, ""))
	}
	return text, false
}

// maxToolIterations caps the tool-call loop: the model may call tools up to this
// many rounds before Answer gives up and refuses. It bounds a stuck or looping
// model; the spend ceiling (ADR 0006) is the cost backstop across the rounds.
const maxToolIterations = 5

// Answer grounds q on the retrieved vault context — extended, when the query is
// in-domain, by tools the model may call — and returns a Reply, or a refusal when
// it is out of scope or cannot be grounded (ADR 0008/0011).
//
// The flow: retrieve -> assemble the grounded prompt (scope + chunks) -> loop:
// complete with the tool set; if the model requests tools, run them and feed the
// results back as scoped context, then complete again; when the model returns
// text, refuse on the sentinel else answer. The loop is capped.
//
// The tool <-> grounding boundary (ADR 0011): tool results enter as scoped,
// attributed context, never as authority. The scope prompt keys refusal on the
// instance's DOMAIN, not on whether some context happens to cover the query — so
// a tool can ground an in-domain answer the vault lacks, but can never widen what
// is in scope. The refusal sentinel fires the same as ever.
//
// The engine depends only on its interfaces (ADR 0001): the spend ceiling is a
// metered Model decorator (ADR 0006/0008) and the consent gate runs ahead of
// Answer at the runtime boundary (ADR 0007) — neither lives here.
func (e *Engine) Answer(ctx context.Context, q Query) (Reply, error) {
	chunks, err := e.retriever.Retrieve(ctx, q.Text)
	if err != nil {
		return Reply{}, err
	}
	// Server-side grounding trace (ADR 0008): the source tags never reach the user
	// (they are internal, un-openable paths), so the sources that grounded an
	// answer are recorded here instead — model-independent, straight from the
	// retrieved chunks.
	if len(chunks) > 0 {
		slog.Info("grounding sources", "count", len(chunks), "sources", chunkSources(chunks))
	}
	tools := e.tools.Defs()
	// Nothing to ground on, no tool that could, AND no conversation to meta-operate
	// on: refuse without a model call. History is the exception — a meta follow-up
	// like "tldr" has no vault chunks yet must still reach the model to summarize
	// the recent conversation (ADR 0014), so an empty retrieval alone must not
	// short-circuit it. The grounding contract in the prompt still refuses a
	// genuine off-domain question.
	if len(chunks) == 0 && len(tools) == 0 && len(q.History) == 0 {
		return Reply{Text: outOfScopeReply, Refused: true}, nil
	}

	messages := []Message{
		{Role: RoleSystem, Content: renderPrompt(e.prompt, q.Text, e.persona, e.scope, q.History, chunks, len(tools) > 0)},
		{Role: RoleUser, Content: q.Text},
	}

	for i := 0; i < maxToolIterations; i++ {
		completion, err := e.model.Complete(ctx, messages, tools)
		if err != nil {
			return Reply{}, err
		}
		if len(completion.ToolCalls) == 0 {
			// Final answer. The scope prompt has the model lead an out-of-domain (or
			// ungroundable) reply with the sentinel, then a brief in-persona note of
			// what it can help with (ADR 0013): parseRefusal strips the marker and
			// surfaces that note as the persona-shaped decline.
			text, refused := parseRefusal(completion.Text)
			return Reply{Text: text, Refused: refused}, nil
		}

		// The model wants tools: record its request, run each, and append the
		// results as scoped tool context for the next round.
		messages = append(messages, Message{Role: RoleAssistant, ToolCalls: completion.ToolCalls})
		for _, tc := range completion.ToolCalls {
			result, err := e.tools.Call(ctx, tc.Name, tc.Arguments)
			if err != nil {
				if errors.Is(err, ErrToolUnavailable) {
					// A transport failure is not something the model can reason around.
					return Reply{}, err
				}
				// A tool that ran and failed feeds its failure back so the model adapts.
				result = "tool error: " + err.Error()
			}
			messages = append(messages, Message{Role: RoleTool, ToolCallID: tc.ID, Content: result})
		}
	}

	// The loop hit its cap without a final answer — refuse rather than hang.
	return Reply{Text: outOfScopeReply, Refused: true}, nil
}

// conversationBlock renders the injected recent-conversation turns (ADR 0014) as
// a labelled, threaded, chronological block. It is framed as partial context,
// never a fact source: only consented participants appear, so a gap — or a reply
// to "a message not shown" — means not-shown / not-consented, not silence. Each
// line is timestamped and speaker-attributed; a reply-to whose target is in the
// injected set names that speaker, else renders "a message not shown". Empty
// history renders nothing (a standalone question or a disabled buffer).
func conversationBlock(history []HistoryTurn) string {
	if len(history) == 0 {
		return ""
	}
	label := make(map[string]string, len(history)) // message id -> speaker, for reply-to threading
	for _, t := range history {
		if t.MessageID != "" {
			label[t.MessageID] = speakerLabel(t)
		}
	}
	var b strings.Builder
	b.WriteString("\n\nRECENT CONVERSATION — the recent turns of this chat, for continuity. You MAY summarize, continue, or refer to them when the user makes a meta or follow-up request (\"tldr\", \"more\", \"what did you mean\"): that is in-scope and needs no [source] citation. But they are conversation context, NOT external facts — never assert their contents as factual claims about the world; factual answers still come only from the CONTEXT below. A partial record: only consented participants appear, so a gap or a reply to \"a message not shown\" means not shown / not consented, not that no one spoke.\n")
	for _, t := range history {
		b.WriteString("\n[")
		b.WriteString(t.Time.Format("15:04"))
		b.WriteString("] ")
		b.WriteString(speakerLabel(t))
		if t.ReplyTo != "" {
			target := label[t.ReplyTo]
			if target == "" {
				target = "a message not shown"
			}
			b.WriteString(" (reply to " + target + ")")
		}
		b.WriteString(": ")
		b.WriteString(strings.TrimSpace(t.Text))
	}
	return b.String()
}

// speakerLabel renders a turn's author: the human display label, or the assistant
// for the bot's own turns.
func speakerLabel(t HistoryTurn) string {
	if t.Bot || t.Speaker == "" {
		return "assistant"
	}
	return t.Speaker
}

// chunkSources lists the source tags of the retrieved chunks, for the server-side
// grounding trace (ADR 0008). Never user-facing.
func chunkSources(chunks []Chunk) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.Source
	}
	return out
}
