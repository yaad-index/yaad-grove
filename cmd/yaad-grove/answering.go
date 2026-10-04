package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/yaad-index/bonyan/model/chatcompat"
	"github.com/yaad-index/bonyan/secret"

	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/model"
	"github.com/yaad-index/yaad-grove/internal/runtime"
	"github.com/yaad-index/yaad-grove/internal/store"
	"github.com/yaad-index/yaad-grove/internal/tools"
	"github.com/yaad-index/yaad-grove/langpacks"
)

// answering is the engine built from the configuration, with the parts serve
// also needs: the tool registry to connect and close, the store to reindex, and
// what the startup line reports.
type answering struct {
	engine   *core.Engine
	registry *tools.Registry
	kbStore  store.Store
	toolset  core.Tools
	servers  []tools.ServerConfig
	persona  string
	pack     *langpacks.Pack
}

// buildAnswering builds the engine from c: model, retrieval, tools, persona,
// prompt template and language pack, with mem as its long-term memory (nil for
// none). Serve and replay both answer through it, so a replay answers as the
// bot does. The registry is built but not connected.
func (c *ServeCmd) buildAnswering(log *slog.Logger, meter *budget.Meter, secrets *secret.Resolver, mem *core.Memory) (*answering, error) {
	// The model is bonyan's OpenAI-compatible client (ADR 0023), with the native
	// tool-call fallback (#88), wrapped with the spend meter (ADR 0006/0008) so the
	// ceiling is enforced on the model-call path while core stays free of budget.
	// The key is read from the environment on every call; none is sent when the
	// variable is unset.
	chatOpts := chatcompat.Options{BaseURL: c.ModelBaseURL, Model: c.ModelName, HTTPClient: &http.Client{Timeout: 60 * time.Second}}
	if os.Getenv(modelKeyEnv) != "" {
		chatOpts.Secrets, chatOpts.KeyName = secrets.Scope(modelKeyEnv), modelKeyEnv
	}
	chat, err := chatcompat.New(chatOpts)
	if err != nil {
		return nil, err
	}
	m := runtime.MeterChat(meter, model.NativeToolCalls(chat))
	// Retrieval (ADR 0001/0017): keyword by default; semantic when an embedding
	// endpoint is configured, with keyword as the query-time fallback. Building the
	// semantic index embeds the whole vault, so a failure here fails startup.
	retriever, kbStore, err := buildRetriever(c, log)
	if err != nil {
		return nil, err
	}

	// The tool registry connects the configured MCP servers; their tools become
	// this instance's tools (ADR 0001). Zero configured leaves a retrieval-only
	// bot. The caller connects it and closes it.
	servers, err := parseMCPServers(c.MCPServers)
	if err != nil {
		return nil, err
	}
	// Scope each server's exposed tools per --mcp-allow / --mcp-deny (issue #87), so
	// a read-only bot never advertises or can call a server's write/identity tools.
	servers, err = applyToolLists(servers, c.MCPAllow, c.MCPDeny)
	if err != nil {
		return nil, err
	}
	registry := tools.New(servers, version)
	// The instance's tool set is the MCP registry plus, when structured dimensions
	// are declared, the built-in kb_enumerate structured-lookup tool over the store
	// (ADR 0019/0022). With neither dimensions nor orderable fields, WithEnumerate
	// returns the registry unchanged.
	toolset := tools.WithEnumerate(registry, kbStore, c.StoreDimensions, c.StoreOrderable)

	// The optional persona layer (ADR 0013): operator-authored behavior prepended
	// to the system prompt ahead of scope/grounding. Load before the engine so a
	// misconfigured persona fails startup rather than serving without it.
	persona, err := loadPersona(c.PersonaFile)
	if err != nil {
		return nil, err
	}
	// The optional grounding-prompt template (ADR 0016): empty uses the embedded
	// default (byte-for-byte the built-in prompt); a set-but-unreadable/unparseable
	// path fails startup rather than serving a broken prompt.
	promptTmpl, err := loadPromptTemplate(c.PromptTemplate)
	if err != nil {
		return nil, err
	}
	if stale := core.TemplateQueryFields(promptTmpl); len(stale) > 0 {
		// ADR 0023: these fields render empty, since a query's content no longer
		// sits in the trusted instructions. A template written for them loses it.
		log.Warn("the prompt template names fields that are always empty; the content they carried now reaches the model outside the instructions — update the template", "path", c.PromptTemplate, "fields", stale)
	}
	// The language pack (ADR 0018): its prompt guidance is layered into the system
	// prompt. Loaded before the engine so an unknown/malformed pack fails startup
	// rather than serving without it. The base "en" adds nothing.
	pack, err := langpacks.Load(c.Language, c.LangpacksDir)
	if err != nil {
		return nil, err
	}
	engine := core.New(m, c.ModelName, retriever, tools.ForAgent(toolset, registry), c.Scope,
		core.WithPersona(persona), core.WithPromptTemplate(promptTmpl), core.WithLanguage(pack.Prompt),
		core.WithContextTokens(c.ContextSize), core.WithMaxOutputTokens(c.MaxOutputTokens),
		core.WithMemory(mem))
	return &answering{engine: engine, registry: registry, kbStore: kbStore, toolset: toolset, servers: servers, persona: persona, pack: pack}, nil
}
