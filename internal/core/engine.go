// Package core is the transport-agnostic heart of yaad-grove: one Answer
// function that grounds a query on a curated vault plus external tools and
// refuses anything outside that scope.
//
// Nothing in this package knows about any transport (Telegram, Discord, ...),
// any concrete model provider, or any specific tool. Those all arrive as
// interfaces (a bonyan chat model, Retriever, Tools) and are wired in
// cmd/yaad-grove. This is the boundary that makes the engine generic from day
// one (ADR 0001): a bot is just (vault + tools + scope + transport), and only
// this package defines what "answer" means.
package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"github.com/yaad-index/bonyan/agent"
	"github.com/yaad-index/bonyan/assemble"
	"github.com/yaad-index/bonyan/budget"
	"github.com/yaad-index/bonyan/content"
	bmodel "github.com/yaad-index/bonyan/model"
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
	// ReplyContext is the message this query replies to, injected verbatim as
	// context (ADR 0014): when a user replies to some earlier message and asks about
	// it, the platform delivers that message inline, so the bot can see it even
	// though it was never buffered. Empty when the query isn't a reply. Like History
	// it is context, not a fact source — grounding still governs factual claims.
	ReplyContext string
	// Chat is the chat the query came from: with User.ID it names the query's
	// long-term memory session (ADR 0023 §2).
	Chat string
	// Remember lets the run use long-term memory: recall what is known about
	// the user and keep this turn and its answer. The runtime sets it only for a
	// turn the consent gate admitted (ADR 0023 §5); an engine with no memory
	// ignores it.
	Remember bool
	// Withdrawals is the asker's withdrawal count (Memory.Withdrawals), read
	// before the consent gate admitted the turn. The turn is kept only if the
	// count is unchanged when it is answered, so a withdrawal at any point after
	// the gate read consent keeps it out (ADR 0023 §5).
	Withdrawals uint64
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
	// Call invokes a named tool with arguments and returns its text result, or
	// the error the call failed with.
	Call(ctx context.Context, name string, args map[string]any) (string, error)
}

// Engine answers queries grounded on a Retriever's chunks and Tools' results,
// driven by a chat model, and refuses out-of-scope input. It is the only place
// that defines answering; everything else adapts into or out of it.
type Engine struct {
	// chat is the model the agent run answers with, under the name modelName.
	chat      bmodel.Chat
	modelName string
	retriever Retriever
	// tools are the run's tools; nil is none.
	tools agent.Tools
	// maxOutputTokens caps each model reply (bonyan requires a cap).
	maxOutputTokens int
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
	// language is the selected language pack's prompt guidance (ADR 0018): opaque
	// per-instance text injected into the system prompt (e.g. "answer in Persian").
	// Empty = the base language, which adds nothing — the engine knows no specific
	// language, it just injects whatever the pack provides.
	language string
	// contextTokens is the hard cap on the retrieved material, in approximate tokens
	// (ADR 0021 Part B). Zero means no cap — the knob's requiredness is enforced at
	// startup by the CLI, so an in-process engine built without it behaves as before.
	contextTokens int
	// memory is the long-term memory (ADR 0023); nil is none.
	memory *Memory
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

// WithLanguage sets the selected language pack's prompt guidance (ADR 0018). Empty
// (the base language) is a no-op, so a bot on the default language renders exactly
// as before.
func WithLanguage(prompt string) Option {
	return func(e *Engine) { e.language = prompt }
}

// WithContextTokens sets the hard cap on the retrieved material, in approximate
// tokens (ADR 0021 Part B). Zero or negative is a no-op (no cap): the requiredness
// is enforced at startup by the CLI, not here, so existing callers and tests that
// build an engine without a cap keep working.
func WithContextTokens(n int) Option {
	return func(e *Engine) { e.contextTokens = n }
}

// WithMaxOutputTokens caps each model reply, in tokens. Zero or negative keeps
// DefaultMaxOutputTokens.
func WithMaxOutputTokens(n int) Option {
	return func(e *Engine) {
		if n > 0 {
			e.maxOutputTokens = n
		}
	}
}

// DefaultMaxOutputTokens caps a model reply when no cap is configured. bonyan
// requires one; this one is wide enough not to cut a normal answer.
const DefaultMaxOutputTokens = 4096

// New wires an engine from its collaborators: the chat model, under the name
// modelName, that a bonyan agent run answers with (ADR 0023), the retriever and
// the run's tools, nil for none. The core depends on interfaces and bonyan, not
// on a transport or a provider.
func New(chat bmodel.Chat, modelName string, retriever Retriever, tools agent.Tools, scope string, opts ...Option) *Engine {
	e := &Engine{chat: chat, modelName: modelName, retriever: retriever, tools: tools, scope: scope, maxOutputTokens: DefaultMaxOutputTokens}
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
// metered chat-model decorator (ADR 0006/0008) and the consent gate runs ahead of
// Answer at the runtime boundary (ADR 0007) — neither lives here.
func (e *Engine) Answer(ctx context.Context, q Query) (Reply, error) {
	chunks, err := e.retriever.Retrieve(ctx, q.Text)
	if err != nil {
		return Reply{}, err
	}
	// Hard context-size guard (ADR 0021 Part B): chunks arrive score-sorted, so cap
	// the retrieved material by dropping whole chunks from the lowest-scored tail
	// until the rendered block fits the configured token budget — never mid-chunk.
	// The guard runs before the grounding trace and the empty-retrieval short-circuit
	// so both reflect the chunks that actually ground the answer.
	kept := capContext(chunks, e.contextTokens)
	switch {
	case len(chunks) > 0 && len(kept) == 0:
		// The cap emptied a non-empty retrieval: even the top-scored chunk alone
		// exceeds it. Warn so this is distinguishable from a genuinely empty
		// retrieval — otherwise a too-small cap looks identical to "no relevant
		// chunks" and is undiagnosable. The query falls through to the refusal path.
		slog.Warn("context-size guard dropped every retrieved chunk: the top-scored chunk alone exceeds the cap — it is likely set too small for this vault's chunk sizes",
			"cap_tokens", e.contextTokens, "retrieved", len(chunks))
	case len(kept) < len(chunks):
		slog.Info("context-size guard trimmed low-scored chunks",
			"cap_tokens", e.contextTokens, "kept", len(kept), "dropped", len(chunks)-len(kept))
	}
	chunks = kept
	// Server-side grounding trace (ADR 0008): the source tags never reach the user
	// (they are internal, un-openable paths), so the sources that grounded an
	// answer are recorded here instead — model-independent, straight from the
	// retrieved chunks.
	if len(chunks) > 0 {
		slog.Info("grounding sources", "count", len(chunks), "sources", chunkSources(chunks))
	}
	var tools []bmodel.ToolDef
	if e.tools != nil {
		tools = e.tools.Definitions()
	}
	// Nothing to ground on, no tool that could, AND no conversation to meta-operate
	// on: refuse without a model call. History AND a reply-context are exceptions — a
	// meta follow-up like "tldr", or a reply asking about an inlined earlier message,
	// has no vault chunks yet must still reach the model (ADR 0014), so an empty
	// retrieval alone must not short-circuit them. The grounding contract in the
	// prompt still refuses a genuine off-domain question.
	if len(chunks) == 0 && len(tools) == 0 && len(q.History) == 0 && q.ReplyContext == "" {
		return Reply{Text: outOfScopeReply, Refused: true}, nil
	}

	// One bonyan agent run (ADR 0023): the operator's instructions are the
	// trusted part; the vault chunks, the replied-to message and the recent
	// conversation reach the model as untrusted material and history, each in
	// a marked place, and the asker's name only as a label on the input.
	a := agent.Agent{
		Name:            "grove",
		Models:          []agent.Model{{Name: e.modelName, Chat: e.chat}},
		Prices:          budget.PriceTable{e.modelName: {}},
		MaxOutputTokens: e.maxOutputTokens,
		Limits: agent.Limits{
			MaxSteps: maxToolIterations,
			Deadline: answerDeadline,
			// The spend ceiling is the engine's own meter around the model (ADR
			// 0006), so the run's budget must never trip first. bonyan requires
			// both bounds positive; the model's price is zero, so bonyan's meter
			// costs every call at 0 and the cost bound of 1 is never reached,
			// and runTokens sits far above any real answer.
			Budget: agent.Budget{MaxTokens: runTokens, MaxCostMicros: 1},
		},
		// The engine's context guard (ADR 0021) bounds what is sent; bonyan's
		// own budgets are set wide so they never trim it further.
		Context:       assemble.Budgets{Instructions: wide, Memory: wide, Material: wide, History: wide, Tools: wide},
		LoopThreshold: -1, // the step cap bounds a repeating model, as before
		Instructions:  content.Instruction(renderInstructions(e.prompt, e.persona, e.scope, e.language, q.History, q.ReplyContext, len(tools) > 0)),
		Material:      material(chunks, q.ReplyContext),
		History:       historyMessages(q.History),
	}
	if e.tools != nil {
		a.Tools = e.tools
	}
	memTurn := e.memory.use(&a, q)
	input := askerLabel(q.User.Display) + q.Text
	out, rep, err := agent.Run(ctx, a, content.From(content.Provenance{Kind: content.KindUser}, input))
	if err != nil {
		return Reply{}, err
	}
	if answer, ok := out.Answer(); ok {
		// Final answer. The scope prompt has the model lead an out-of-domain (or
		// ungroundable) reply with the sentinel, then a brief in-persona note of
		// what it can help with (ADR 0013): parseRefusal strips the marker and
		// surfaces that note as the persona-shaped decline.
		text, refused := parseRefusal(answer)
		if !refused {
			memTurn.keep(ctx, input, text)
		}
		return Reply{Text: text, Refused: refused}, nil
	}
	switch out.Reason() {
	case agent.ReasonStepLimit:
		// The loop hit its cap without a final answer — refuse rather than hang.
		return Reply{Text: outOfScopeReply, Refused: true}, nil
	case agent.ReasonDeadline:
		if ctx.Err() != nil {
			return Reply{}, ctx.Err()
		}
	}
	if rep.Err != nil {
		return Reply{}, rep.Err
	}
	return Reply{}, fmt.Errorf("core: the answer run ended %s", out)
}

// The run's bounds that are not the engine's own settings.
const (
	// answerDeadline bounds one answer, every model and tool call included.
	answerDeadline = 3 * time.Minute
	// runTokens is the per-run token bound bonyan requires; the spend ceiling
	// is the engine's meter, so this one is set far above any real answer.
	runTokens = 10_000_000
	// wide is a context budget, in bytes, that the engine's own guard keeps
	// every request under.
	wide = 1 << 22
)

// askerLabel prefixes the input with who asks, when known: a label on the
// untrusted input, never in the instructions (#99, ADR 0023). Whitespace runs
// collapse so a name cannot start a line of its own, and its square brackets
// become parentheses so it cannot close the label early and open another.
func askerLabel(display string) string {
	name := labelBrackets.Replace(strings.Join(strings.Fields(display), " "))
	if name == "" {
		return ""
	}
	return "[" + name + "] "
}

// labelBrackets keeps a name inside its label.
var labelBrackets = strings.NewReplacer("[", "(", "]", ")")

// material is the run's untrusted material: each retrieved chunk under its
// grounding id, then the message the query replies to.
func material(chunks []Chunk, replyContext string) []content.Untrusted {
	out := make([]content.Untrusted, 0, len(chunks)+1)
	for _, c := range chunks {
		out = append(out, content.From(content.Provenance{Kind: content.KindFetched, ID: docID(c)}, strings.TrimSpace(c.Text)))
	}
	if r := strings.TrimSpace(replyContext); r != "" {
		out = append(out, content.From(content.Provenance{Kind: content.KindUser, ID: replyID}, r))
	}
	return out
}

// replyID labels the replied-to message among the material.
const replyID = "replied-to message"

// historyMessages are the recent conversation's turns as the run's history,
// oldest first: a person's turn is a user message carrying its time, speaker
// and reply-to in its text, the bot's own is an assistant message. Every turn
// is untrusted.
func historyMessages(history []HistoryTurn) []bmodel.Message {
	if len(history) == 0 {
		return nil
	}
	label := make(map[string]string, len(history)) // message id -> speaker, for reply-to threading
	for _, t := range history {
		if t.MessageID != "" {
			label[t.MessageID] = speakerLabel(t)
		}
	}
	out := make([]bmodel.Message, 0, len(history))
	for _, t := range history {
		var b strings.Builder
		b.WriteString("[" + t.Time.Format("15:04") + "] " + speakerLabel(t))
		if t.ReplyTo != "" {
			target := label[t.ReplyTo]
			if target == "" {
				target = "a message not shown"
			}
			b.WriteString(" (reply to " + target + ")")
		}
		b.WriteString(": " + strings.TrimSpace(t.Text))
		if t.Bot {
			out = append(out, bmodel.Message{Role: bmodel.RoleAssistant, Parts: []content.Text{content.From(content.Provenance{Kind: content.KindModel, ID: t.MessageID}, b.String())}})
			continue
		}
		out = append(out, bmodel.Message{Role: bmodel.RoleUser, Parts: []content.Text{content.From(content.Provenance{Kind: content.KindUser, ID: t.MessageID}, b.String())}})
	}
	return out
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
