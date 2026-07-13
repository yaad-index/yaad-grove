// Command yaad-grove is the CLI for the yaad-grove engine: a config-driven bot
// that answers a community's questions from a curated vault plus external
// tools, and refuses anything outside them.
//
// The CLI stays thin (house convention): it parses config and wires the
// internal packages, then runs. All behavior lives under internal/. Config
// layers as file < env < flag via a YAML file (searched at
// /etc/yaad-grove/config.yaml and ./config.yaml). Secrets (model API key,
// Telegram token) come from the environment, never inlined in config.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/alecthomas/kong"
	kongyaml "github.com/alecthomas/kong-yaml"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/embed"
	"github.com/yaad-index/yaad-grove/internal/memory"
	"github.com/yaad-index/yaad-grove/internal/model"
	"github.com/yaad-index/yaad-grove/internal/pending"
	"github.com/yaad-index/yaad-grove/internal/quarantine"
	"github.com/yaad-index/yaad-grove/internal/retrieval"
	"github.com/yaad-index/yaad-grove/internal/runtime"
	"github.com/yaad-index/yaad-grove/internal/tools"
	"github.com/yaad-index/yaad-grove/internal/transcript"
	"github.com/yaad-index/yaad-grove/internal/transport"
	"github.com/yaad-index/yaad-grove/internal/transport/telegram"
	"github.com/yaad-index/yaad-grove/langpacks"
)

// version is the build version, overridden at link time via -ldflags.
var version = "dev"

// CLI is the yaad-grove command surface. Every config value resolves through
// file < env < flag.
type CLI struct {
	LogLevel string `name:"log-level" default:"info" enum:"debug,info,warn,error" help:"Log verbosity."`

	Serve   ServeCmd   `cmd:"" help:"Run the bot: connect the transport and answer queries."`
	Version VersionCmd `cmd:"" help:"Print the build version and exit."`
}

// ServeCmd runs the answering bot. It wires the engine (model + retriever +
// tools + scope), the access/consent gate, and the transport, then serves.
type ServeCmd struct {
	VaultDir string `name:"vault-dir" default:"./vault" help:"Curated vault root (markdown + frontmatter) the bot grounds on." type:"path"`
	Scope    string `name:"scope" help:"Scope statement / system prompt that bounds the bot and drives refusal."`

	// PersonaFile is the optional operator-authored persona/behavior file (ADR
	// 0013), layered into the system prompt ahead of scope. Left empty, it defaults
	// to PERSONA.md in the working dir: a present default file is used and its
	// absence is graceful (no persona layer); an explicitly-set path that can't be
	// read is a startup error (the operator meant to use one).
	PersonaFile string `name:"persona-file" help:"Operator persona/behavior file (Markdown) layered into the prompt. Defaults to PERSONA.md in the working dir (absent is fine); an explicitly-set unreadable path is fatal."`

	// PromptTemplate is the optional operator grounding-prompt template (ADR 0016).
	// Empty uses the embedded default (byte-for-byte the built-in prompt); a set
	// path that can't be read or parsed is a startup error.
	PromptTemplate string `name:"prompt-template" help:"Operator grounding-prompt template (Go text/template with {{.Persona}} {{.Scope}} {{.Language}} {{.Asker}} {{.ReplyContext}} {{.History}} {{.Context}}). Empty uses the built-in default; an unreadable/unparseable path is fatal."`

	// Language selects the language pack (ADR 0018): its prompt guidance is layered
	// into the system prompt, and follow-up detection stays language-neutral (the
	// recency gate). Default "en" (the base, which adds nothing). LangpacksDir is an
	// optional external dir to add/override packs beyond the embedded built-ins.
	Language     string `name:"language" default:"en" help:"Language pack to load (e.g. 'en', 'fa'); its prompt guidance is layered into the system prompt. Unknown language is fatal."`
	LangpacksDir string `name:"langpacks-dir" help:"Optional external directory of <code>.yaml packs, added to / overriding the embedded built-ins." type:"path"`

	ModelBaseURL string `name:"model-base-url" default:"https://api.openai.com/v1" help:"OpenAI-compatible API base URL."`
	ModelName    string `name:"model-name" default:"gpt-4o-mini" help:"Model id understood by the endpoint."`

	// Semantic retrieval (ADR 0017): setting the embedding base-url + model pair
	// (both together) switches retrieval from keyword to embedding-based, with
	// keyword as the query-time fallback. Empty pair = keyword (zero-config
	// default). Key: YAADGROVE_EMBEDDING_API_KEY, falling back to the model key.
	EmbeddingBaseURL    string  `name:"embedding-base-url" help:"OpenAI-compatible embeddings API base URL. Set with --embedding-model to enable semantic retrieval (e.g. a local Ollama /v1)."`
	EmbeddingModel      string  `name:"embedding-model" help:"Embedding model id (e.g. text-embedding-3-large, or bge-m3 via Ollama). Set together with --embedding-base-url."`
	SimilarityThreshold float32 `name:"similarity-threshold" default:"0.30" help:"Semantic-retrieval cosine floor: a chunk must clear it to be retrieved (below the floor for everything → refusal). 0 disables the floor (every query reaches the model)."`

	// RetrievalMode (#65) picks how lexical + semantic combine: keyword (lexical
	// only), semantic (embeddings with a keyword error-fallback — the prior default),
	// or hybrid (RRF fusion of both, run every query). Empty defaults to hybrid when
	// embeddings are configured, else keyword. semantic/hybrid require embeddings.
	RetrievalMode string `name:"retrieval-mode" help:"How lexical + semantic retrieval combine: 'keyword', 'semantic', or 'hybrid' (RRF fusion). Default: hybrid when embeddings are set, else keyword. semantic/hybrid require --embedding-*."`

	DefaultTier string `name:"default-tier" default:"default" help:"Tier applied to users without an override."`

	// The ACL store persists consent, tier, and rate state (ADR 0002/0003); it
	// survives restarts so consent decisions and tier assignments are durable.
	ACLDB string `name:"acl-db" default:"./acl.db" help:"Path to the persisted access-control store (consent, tiers, rate counters)." type:"path"`

	// MCPServers lists the external tool servers to connect (ADR 0001), each as
	// "name=command arg1 arg2". Repeatable; empty means a retrieval-only bot.
	//
	// sep:"none" disables kong's default comma-splitting of a []string value, so each
	// flag value is one whole "name=..." spec (repeat the flag for more) and a spec's
	// OWN commas survive — critical for the allow/deny tool lists below, whose values
	// are comma-separated tool names. Without it, `--mcp-allow 'svc=a,b,c'` fragments
	// into ["svc=a","b","c"] and the b/c pieces lose their "svc=" prefix (#92).
	MCPServers []string `name:"mcp-server" sep:"none" help:"An MCP tool server to connect, as 'name=command arg1 arg2' (args are space-separated; no spaces within an arg). Repeatable."`

	// MCPAllow / MCPDeny scope which of a connected server's tools are exposed to
	// the model (issue #87), each as "server=tool1,tool2". Allow-list is exclusive
	// (only the listed tools; everything else dropped) and preferred so new tools are
	// closed by default; deny-list is subtractive. A server may use one or the other,
	// not both. Empty means all of that server's tools are exposed (prior behavior).
	// sep:"none" keeps the comma-separated tool list as one value (see MCPServers).
	MCPAllow []string `name:"mcp-allow" sep:"none" help:"Expose ONLY these tools from a server, as 'server=tool1,tool2' (exclusive allow-list; all others dropped). Repeatable."`
	MCPDeny  []string `name:"mcp-deny" sep:"none" help:"Drop these tools from a server, as 'server=tool1,tool2' (subtractive deny-list; the rest are exposed). Repeatable. Mutually exclusive with --mcp-allow for the same server."`

	// The global spend ceiling (ADR 0006): the hard cost backstop the model-call
	// path consults before every call. Conservative default so the bot is
	// cost-capped out of the box; raise it to the real budget. A non-positive
	// ceiling/period is refused (cost-safety cannot be zeroed off).
	SpendCeiling int64         `name:"spend-ceiling" default:"1000000" help:"Global token budget per spend-period — the cost backstop across all users. Conservative default; raise to your budget."`
	SpendPeriod  time.Duration `name:"spend-period" default:"24h" help:"Window the spend-ceiling accumulates over before it resets."`
	BudgetDB     string        `name:"budget-db" default:"./budget.db" help:"Path to the persisted spend-meter database (survives restarts)." type:"path"`

	TelegramGroups []string `name:"telegram-allowed-groups" help:"Group chat ids that count as 'the community' the bot serves (Telegram)."`

	// TelegramAllowedTopics optionally scopes a forum group to specific topics
	// (#98), each spec "chatid=topicid1,topicid2". A group with a spec is answered
	// only in those topics; a group without one is answered in all topics (today's
	// behavior). sep:"none" keeps the comma-separated topic list as one value (a
	// spec's own commas must survive kong parsing — see the MCP flags, #92).
	TelegramAllowedTopics []string `name:"telegram-allowed-topics" sep:"none" help:"Restrict a forum group to specific topics, as 'chatid=topicid1,topicid2' (repeatable). Empty means all topics."`

	// The callback token store backs interactive buttons (ADR 0009). It owns its
	// own garbage collection: the sweeper runs every CallbackSweepInterval, so
	// expired tokens and old tombstones can't accumulate. The interval is a
	// deployment parameter, not a constant.
	CallbackDB            string        `name:"callback-db" default:"./callbacks.db" help:"Path to the persisted callback token store (survives restarts)." type:"path"`
	CallbackTTL           time.Duration `name:"callback-ttl" default:"10m" help:"How long a rendered button stays valid before it expires."`
	CallbackSweepInterval time.Duration `name:"callback-sweep-interval" default:"5m" help:"How often the callback store sweeps expired tokens and old tombstones."`

	// The quarantine log records consented community messages OUTSIDE the answering
	// vault (ADR 0004), so a later curation pass has data. The answering bot never
	// reads it. Empty disables logging.
	QuarantineLog string `name:"quarantine-log" default:"./quarantine.jsonl" help:"Path to the consent-gated community-message log (JSONL, append-only). Empty disables logging." type:"path"`

	// The transcript is the durable, role-tagged conversation record (ADR 0015):
	// human turns AND the bot's own answers, one append-only <chat-id>.jsonl per
	// group chat inside this directory. Separate from the quarantine log, never read
	// by the answering or curation path. Empty disables it (the default).
	TranscriptLog string `name:"transcript-log" help:"Directory for per-chat conversation transcripts (one <chat-id>.jsonl each; human + bot turns, append-only). Empty disables transcripts." type:"path"`

	// Admins are the platform user ids answered in a DM (ADR 0012). Admin status is
	// a DM-surface privilege only: in the group an admin is consent-gated like any
	// member. Empty means no one can DM the bot for answers (the DM stays
	// consent-management only for everyone).
	Admins []string `name:"admin" help:"A platform user id with DM-answering privilege (repeatable). Admin is a DM-only privilege; in the group admins are consent-gated like anyone."`

	// The consent nudge shown to an unconsented user who directs a message at the
	// bot in a group (ADR 0012): a text reply (default) or an emoji reaction, with
	// configurable copy/emoji. Empty text/emoji use sensible defaults.
	NudgeMode  string `name:"nudge-mode" default:"message" enum:"message,reaction" help:"How to nudge an unconsented user who addresses the bot in a group: 'message' (text reply) or 'reaction' (emoji)."`
	NudgeText  string `name:"nudge-text" help:"Message-mode nudge copy (the opt-in instruction). Empty uses a sensible default."`
	NudgeEmoji string `name:"nudge-emoji" help:"Reaction-mode nudge emoji. Empty uses a sensible default (🤝)."`

	// Conversation memory (ADR 0014): a per-conversation buffer of recent turns so
	// the bot can answer follow-ups ("tldr", "what about X"). MemoryTurns is how
	// many turns are RETAINED; MemoryInject is how many actually enter a prompt.
	// MemoryTurns 0 disables the buffer (each message answered in isolation).
	MemoryTurns  int `name:"memory-turns" default:"100" help:"Recent conversation turns retained per chat for follow-ups (0 disables)."`
	MemoryInject int `name:"memory-inject" default:"15" help:"How many retained turns may enter a prompt (the injected slice)."`

	// FollowupWindow gates non-reply follow-ups by a language-neutral recency signal
	// (ADR 0018): a non-reply is treated as a follow-up only if its sender already
	// has a turn in the chat within this window. A reply is always a follow-up. Zero
	// means replies-only.
	FollowupWindow time.Duration `name:"followup-window" default:"30m" help:"How far back to look for a sender's prior turn when deciding if a non-reply is a follow-up. 0 = replies only."`
}

// Run wires and starts the bot. Scaffold: assembles the pieces and reports that
// behavior is not yet implemented.
func (c *ServeCmd) Run(log *slog.Logger) error {
	// The spend ceiling is built first: cost-safety must exist before any
	// model-call path (ADR 0006). It fails closed on a non-positive ceiling/period,
	// and its meter is persisted so a restart cannot reset the budget. The
	// model-call path (a later unit) consults meter.Allow before each call and
	// meter.Record after.
	budgetStore, err := budget.OpenBolt(c.BudgetDB)
	if err != nil {
		return err
	}
	defer func() { _ = budgetStore.Close() }()
	meter, err := budget.New(budget.Config{Ceiling: c.SpendCeiling, Period: c.SpendPeriod}, budgetStore)
	if err != nil {
		return err
	}

	// The model is wrapped with the spend meter (ADR 0006/0008): the engine sees a
	// metered core.Model, so the ceiling is enforced on the model-call path while
	// core stays free of budget.
	m := runtime.MeterModel(meter, model.New(model.Config{
		BaseURL: c.ModelBaseURL,
		APIKey:  os.Getenv("YAADGROVE_MODEL_API_KEY"),
		Model:   c.ModelName,
	}))
	// Retrieval (ADR 0001/0017): keyword by default; semantic when an embedding
	// endpoint is configured, with keyword as the query-time fallback. Building the
	// semantic index embeds the whole vault, so a failure here fails startup.
	retriever, err := buildRetriever(c, log)
	if err != nil {
		return err
	}

	// The tool registry connects the configured MCP servers; their tools become
	// this instance's tools (ADR 0001). Zero configured leaves a retrieval-only
	// bot. Connected before the transport starts and closed on shutdown.
	servers, err := parseMCPServers(c.MCPServers)
	if err != nil {
		return err
	}
	// Scope each server's exposed tools per --mcp-allow / --mcp-deny (issue #87), so
	// a read-only bot never advertises or can call a server's write/identity tools.
	servers, err = applyToolLists(servers, c.MCPAllow, c.MCPDeny)
	if err != nil {
		return err
	}
	registry := tools.New(servers)

	// The optional persona layer (ADR 0013): operator-authored behavior prepended
	// to the system prompt ahead of scope/grounding. Load before the engine so a
	// misconfigured persona fails startup rather than serving without it.
	persona, err := loadPersona(c.PersonaFile)
	if err != nil {
		return err
	}
	// The optional grounding-prompt template (ADR 0016): empty uses the embedded
	// default (byte-for-byte the built-in prompt); a set-but-unreadable/unparseable
	// path fails startup rather than serving a broken prompt.
	promptTmpl, err := loadPromptTemplate(c.PromptTemplate)
	if err != nil {
		return err
	}
	// The language pack (ADR 0018): its prompt guidance is layered into the system
	// prompt. Loaded before the engine so an unknown/malformed pack fails startup
	// rather than serving without it. The base "en" adds nothing.
	pack, err := langpacks.Load(c.Language, c.LangpacksDir)
	if err != nil {
		return err
	}
	engine := core.New(m, retriever, registry, c.Scope,
		core.WithPersona(persona), core.WithPromptTemplate(promptTmpl), core.WithLanguage(pack.Prompt))

	// The gate stacks surface-reach -> rate-limit -> consent -> serve (ADR
	// 0002/0003/0007) over a persisted ACL store.
	aclStore, err := acl.OpenBolt(c.ACLDB)
	if err != nil {
		return err
	}
	defer func() { _ = aclStore.Close() }()
	gate := acl.NewGate(aclStore, acl.Tier(c.DefaultTier))

	// The callback token store backs interactive buttons (ADR 0009). It owns its
	// own sweeper — started here on open, stopped on Close — so the storage bound
	// is live the moment serve opens the store, with nothing for a later loop to
	// remember to schedule. Opened before the transport starts; its deferred Close
	// fires after Run returns, so in-flight callbacks can still resolve during
	// shutdown.
	callbacks, err := pending.OpenBolt(c.CallbackDB, c.CallbackTTL, c.CallbackSweepInterval)
	if err != nil {
		return err
	}
	defer func() { _ = callbacks.Close() }()

	// The quarantine log records consented messages for later curation (ADR 0004),
	// kept outside the answering vault. It is passed to the runtime handler (which
	// logs only on the consent-granted path); empty path disables it.
	var qlog quarantine.Log
	if c.QuarantineLog != "" {
		flog, err := quarantine.OpenFile(c.QuarantineLog)
		if err != nil {
			return err
		}
		defer func() { _ = flog.Close() }()
		qlog = flog
	}

	// The conversation transcript (ADR 0015): a durable, role-tagged record kept in a
	// directory of per-chat files, separate from the quarantine log and never read by
	// the answering or curation path. The directory is created + validated writable
	// now, so a misconfigured path fails loud at startup. Empty disables it.
	var tlog transcript.Log
	if c.TranscriptLog != "" {
		dlog, err := transcript.OpenDir(c.TranscriptLog)
		if err != nil {
			return err
		}
		defer func() { _ = dlog.Close() }()
		tlog = dlog
	}
	// A signal cancels ctx, which drives the whole shutdown: the transport's Run
	// returns and the deferred Closes fire (LIFO: registry -> qlog -> callbacks ->
	// acl -> budget). Known Phase-1 limitation: the Telegram library dispatches
	// handlers asynchronously and does not join them, so a handler in-flight at
	// shutdown can briefly outlive a store Close (errors are logged, not fatal); an
	// in-flight drain is a follow-up.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := registry.Connect(ctx); err != nil {
		return err
	}
	defer func() { _ = registry.Close() }()

	allowedTopics, err := parseTopicAllowList(c.TelegramAllowedTopics)
	if err != nil {
		return err
	}
	var tp transport.Transport = telegram.New(telegram.Config{
		Token:         os.Getenv("YAADGROVE_TELEGRAM_TOKEN"),
		AllowedGroups: c.TelegramGroups,
		AllowedTopics: allowedTopics,
	}, callbacks)

	// The surface-split answering policy (ADR 0012): the admin allowlist (DM
	// answering) and the consent nudge for an unconsented user who addresses the
	// bot in a group. Both are per-instance config. The composition boundary is
	// where transport capability meets config: a reaction-mode nudge needs a
	// transport that can react, so it is downgraded to message-mode here when the
	// transport can't — the handler then only ever emits a nudge the transport can
	// deliver, and the opt-in instruction is never silently dropped.
	nudge := runtime.Nudge{
		Mode:  runtime.NudgeMode(c.NudgeMode),
		Text:  c.NudgeText,
		Emoji: c.NudgeEmoji,
	}
	if nudge.Mode == runtime.NudgeReaction && !tp.Supports(transport.CapReactions) {
		log.Warn("nudge-mode 'reaction' unsupported by transport; using message-mode", "transport", tp.Name())
		nudge.Mode = runtime.NudgeMessage
	}
	// Conversation memory (ADR 0014): an in-memory per-chat buffer of recent turns.
	// MemoryTurns 0 disables it — the bot then answers each message in isolation.
	convoMemory := memory.New(c.MemoryTurns)
	policy := runtime.Policy{
		Admins:         runtime.NewAdminSet(c.Admins),
		Nudge:          nudge,
		Memory:         convoMemory,
		Inject:         c.MemoryInject,
		FollowupWindow: c.FollowupWindow,
		Transcript:     tlog,
	}

	// The action registry maps admin verbs to ACL-tier-gated executors (ADR
	// 0009/0010); the gate is the tier source, the re-authorizer, and the tier
	// writer. The handler routes by surface (ADR 0012): admin DMs answer, other
	// DMs manage consent, group messages pass the consent gate; consented group
	// messages are recorded (ADR 0004) and button clicks resolved.
	actions := runtime.DefaultRegistry(gate)
	handler := runtime.NewHandler(gate, engine, callbacks, actions, gate, qlog, gate, policy)

	quarantineState := c.QuarantineLog
	if quarantineState == "" {
		quarantineState = "disabled"
	}
	transcriptState := c.TranscriptLog
	if transcriptState == "" {
		transcriptState = "disabled"
	}
	// Startup line: what is actually live, so the staged wiring (sweeper, logging,
	// tools) is verifiable at a glance.
	log.Info("yaad-grove serving",
		"transport", tp.Name(),
		"model", c.ModelName,
		"vault_dir", c.VaultDir,
		"default_tier", c.DefaultTier,
		"acl_db", c.ACLDB,
		"callback_db", c.CallbackDB,
		"callback_sweep", c.CallbackSweepInterval.String(),
		"quarantine_log", quarantineState,
		"transcript_log", transcriptState,
		"persona", persona != "",
		"language", pack.Code,
		"memory_turns", c.MemoryTurns,
		"memory_inject", c.MemoryInject,
		"admins", len(policy.Admins),
		"nudge_mode", c.NudgeMode,
		"mcp_servers", len(servers),
		"tools", len(registry.Defs()),
		"spend_remaining_tokens", meter.Remaining(),
	)

	if err := tp.Run(ctx, handler); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	log.Info("yaad-grove stopped")
	return nil
}

// loadPersona reads the persona file for the engine (ADR 0013). An empty
// configured path means "use the default": PERSONA.md in the working dir, whose
// absence is graceful (no persona layer). A non-empty configured path is an
// explicit operator choice, so a missing or unreadable file there is fatal — the
// operator meant to run with a persona. Either way an unreadable (not just
// missing) default file is also fatal: the file is there but broken.
func loadPersona(configuredPath string) (string, error) {
	path, explicit := configuredPath, configuredPath != ""
	if !explicit {
		path = "PERSONA.md"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !explicit && errors.Is(err, os.ErrNotExist) {
			return "", nil // default file simply not present — run without a persona
		}
		return "", fmt.Errorf("persona file %q: %w", path, err)
	}
	return string(data), nil
}

// loadPromptTemplate loads an operator grounding-prompt template (ADR 0016). An
// empty path uses the embedded default (nil template). A set path that can't be
// read or parsed is fatal — a broken prompt is a deployment error, not a silent
// fallback.
func loadPromptTemplate(path string) (*template.Template, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("prompt template %q: %w", path, err)
	}
	t, err := core.ParsePromptTemplate(string(data))
	if err != nil {
		return nil, fmt.Errorf("prompt template %q: %w", path, err)
	}
	return t, nil
}

// retrievalMaxChunks caps how many chunks either retriever returns per query.
const retrievalMaxChunks = 8

// Retrieval modes (#65): how the lexical and semantic retrievers combine.
const (
	retrievalModeKeyword  = "keyword"  // lexical only
	retrievalModeSemantic = "semantic" // embeddings, with keyword as an error-fallback (prior default)
	retrievalModeHybrid   = "hybrid"   // RRF fusion of both, always
)

// buildRetriever selects the retriever (ADR 0001/0017, issue #65). The keyword
// (lexical) retriever is always available. When the embedding base-url + model
// pair is set it also builds the semantic retriever — which embeds the whole vault
// now, so a failure here is a startup error. An incomplete pair (one without the
// other) is a startup error, like the chat-model pair. The embedding key is
// YAADGROVE_EMBEDDING_API_KEY, falling back to the model key.
//
// --retrieval-mode picks how they combine: keyword (lexical only), semantic
// (embeddings with a keyword error-fallback — the prior default behavior), or
// hybrid (RRF fusion of both, always). The default is hybrid when embeddings are
// configured, else keyword. semantic and hybrid both require embeddings.
func buildRetriever(c *ServeCmd, log *slog.Logger) (core.Retriever, error) {
	keyword := retrieval.New(c.VaultDir, retrievalMaxChunks)
	base := strings.TrimSpace(c.EmbeddingBaseURL)
	emodel := strings.TrimSpace(c.EmbeddingModel)
	embeddingsSet := base != "" || emodel != ""
	if embeddingsSet && (base == "" || emodel == "") {
		return nil, errors.New("serve: --embedding-base-url and --embedding-model must be set together")
	}

	// Resolve the mode: an unset mode defaults to hybrid when embeddings are on (the
	// new best default), else keyword (the zero-config default).
	mode := strings.TrimSpace(c.RetrievalMode)
	if mode == "" {
		if embeddingsSet {
			mode = retrievalModeHybrid
		} else {
			mode = retrievalModeKeyword
		}
	}
	if mode == retrievalModeKeyword {
		return keyword, nil
	}
	if mode != retrievalModeSemantic && mode != retrievalModeHybrid {
		return nil, fmt.Errorf("serve: unknown --retrieval-mode %q (want keyword, semantic, or hybrid)", mode)
	}
	if !embeddingsSet {
		return nil, fmt.Errorf("serve: --retrieval-mode %q requires --embedding-base-url and --embedding-model", mode)
	}

	key := os.Getenv("YAADGROVE_EMBEDDING_API_KEY")
	if key == "" {
		key = os.Getenv("YAADGROVE_MODEL_API_KEY")
	}
	embedder := embed.New(embed.Config{BaseURL: base, APIKey: key, Model: emodel})
	semantic, err := retrieval.NewSemantic(context.Background(), c.VaultDir, embedder, retrievalMaxChunks, c.SimilarityThreshold)
	if err != nil {
		return nil, fmt.Errorf("serve: build semantic index: %w", err)
	}
	// Prominent so the index size — and thus the boot embedding cost — is visible
	// (ADR 0017 crash-loop caveat).
	log.Info("semantic retrieval enabled",
		"retrieval_mode", mode,
		"embedding_model", emodel,
		"chunks_indexed", semantic.Len(),
		"similarity_threshold", c.SimilarityThreshold,
	)
	if mode == retrievalModeHybrid {
		// Fuse lexical + semantic every query, so proper-noun / "which episode" queries
		// come in via the lexical leg even when the semantic leg is empty (#65).
		return retrieval.NewHybrid(retrievalMaxChunks, keyword, semantic), nil
	}
	// semantic: the prior behavior — semantic with keyword as the error-fallback.
	return retrieval.Fallback{Primary: semantic, Secondary: keyword}, nil
}

// parseTopicAllowList parses "chatid=topicid1,topicid2" specs into a group→topics
// map for forum topic-scoping (#98). Both the chat id and each topic id must be
// integers; a malformed spec is a startup error rather than a silently-ignored
// scope. Repeating a chat id merges its topics.
func parseTopicAllowList(specs []string) (map[int64][]int, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make(map[int64][]int, len(specs))
	for _, spec := range specs {
		idStr, topicsStr, ok := strings.Cut(spec, "=")
		idStr = strings.TrimSpace(idStr)
		if !ok || idStr == "" {
			return nil, fmt.Errorf("serve: invalid --telegram-allowed-topics %q (want 'chatid=topicid1,topicid2')", spec)
		}
		chatID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("serve: --telegram-allowed-topics %q has a non-numeric chat id %q", spec, idStr)
		}
		var topics []int
		for _, t := range strings.Split(topicsStr, ",") {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			tid, err := strconv.Atoi(t)
			if err != nil {
				return nil, fmt.Errorf("serve: --telegram-allowed-topics %q has a non-numeric topic id %q", spec, t)
			}
			topics = append(topics, tid)
		}
		if len(topics) == 0 {
			return nil, fmt.Errorf("serve: --telegram-allowed-topics %q lists no topics", spec)
		}
		out[chatID] = append(out[chatID], topics...)
	}
	return out, nil
}

// parseMCPServers parses "name=command arg1 arg2" specs into server configs.
func parseMCPServers(specs []string) ([]tools.ServerConfig, error) {
	out := make([]tools.ServerConfig, 0, len(specs))
	for _, spec := range specs {
		name, cmd, ok := strings.Cut(spec, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("serve: invalid --mcp-server %q (want 'name=command args')", spec)
		}
		fields := strings.Fields(cmd)
		if len(fields) == 0 {
			return nil, fmt.Errorf("serve: --mcp-server %q has no command", spec)
		}
		out = append(out, tools.ServerConfig{Name: name, Command: fields[0], Args: fields[1:]})
	}
	return out, nil
}

// applyToolLists merges --mcp-allow / --mcp-deny specs onto the parsed servers by
// name (issue #87). Each spec is "server=tool1,tool2". It fails loud on a spec
// naming an unconfigured server (a typo would otherwise silently do nothing) and
// on a server given both an allow and a deny list (ambiguous). The actual
// tool-name filtering lives in the registry; this only wires config to servers.
func applyToolLists(servers []tools.ServerConfig, allowSpecs, denySpecs []string) ([]tools.ServerConfig, error) {
	idx := make(map[string]int, len(servers))
	for i, s := range servers {
		idx[s.Name] = i
	}
	assign := func(specs []string, kind string, set func(*tools.ServerConfig, []string)) error {
		for _, spec := range specs {
			name, csv, ok := strings.Cut(spec, "=")
			name = strings.TrimSpace(name)
			if !ok || name == "" {
				return fmt.Errorf("serve: invalid --mcp-%s %q (want 'server=tool1,tool2')", kind, spec)
			}
			i, known := idx[name]
			if !known {
				return fmt.Errorf("serve: --mcp-%s %q names unknown server %q (no matching --mcp-server)", kind, spec, name)
			}
			list := splitTools(csv)
			if len(list) == 0 {
				return fmt.Errorf("serve: --mcp-%s %q lists no tools", kind, spec)
			}
			set(&servers[i], list)
		}
		return nil
	}
	if err := assign(allowSpecs, "allow", func(s *tools.ServerConfig, l []string) { s.Allow = l }); err != nil {
		return nil, err
	}
	if err := assign(denySpecs, "deny", func(s *tools.ServerConfig, l []string) { s.Deny = l }); err != nil {
		return nil, err
	}
	for _, s := range servers {
		if len(s.Allow) > 0 && len(s.Deny) > 0 {
			return nil, fmt.Errorf("serve: server %q has both --mcp-allow and --mcp-deny; use one (allow-list is exclusive)", s.Name)
		}
	}
	return servers, nil
}

// splitTools parses a "tool1,tool2" list, trimming blanks so a stray comma can't
// inject an empty tool name.
func splitTools(csv string) []string {
	var out []string
	for _, t := range strings.Split(csv, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// VersionCmd prints the build version.
type VersionCmd struct{}

// Run prints the version to stdout.
func (VersionCmd) Run() error {
	fmt.Println(version)
	return nil
}

func main() {
	var cli CLI
	parser := kong.Must(&cli,
		kong.Name("yaad-grove"),
		kong.Description("A config-driven community knowledge-base bot: grounded answers, bounded scope."),
		kong.Configuration(kongyaml.Loader, "/etc/yaad-grove/config.yaml", "config.yaml"),
		kong.UsageOnError(),
	)

	ctx, err := parser.Parse(os.Args[1:])
	parser.FatalIfErrorf(err)

	log := newLogger(cli.LogLevel)

	err = ctx.Run(log)
	ctx.FatalIfErrorf(err)
}

// newLogger builds the house slog logger at the given level, writing text to
// stderr.
func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}
