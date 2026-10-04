package main

import (
	"log/slog"
	"os"
	"sync/atomic"

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
// prompt template and language pack. Serve and replay both answer through it,
// so a replay answers as the bot does. A non-nil calls counts every call made
// to the model endpoint. The registry is built but not connected.
func (c *ServeCmd) buildAnswering(log *slog.Logger, meter *budget.Meter, calls *atomic.Int64) (*answering, error) {
	// The model is wrapped with the spend meter (ADR 0006/0008): the engine sees a
	// metered core.Model, so the ceiling is enforced on the model-call path while
	// core stays free of budget.
	var base core.Model = model.New(model.Config{
		BaseURL: c.ModelBaseURL,
		APIKey:  os.Getenv("YAADGROVE_MODEL_API_KEY"),
		Model:   c.ModelName,
	})
	if calls != nil {
		base = countCalls(base, calls)
	}
	m := runtime.MeterModel(meter, base)
	// Retrieval (ADR 0001/0017): keyword by default; semantic when an embedding
	// endpoint is configured, with keyword as the query-time fallback. Building the
	// semantic index embeds the whole vault, so a failure here fails startup.
	retriever, kbStore, err := buildRetriever(c, log)
	if err != nil {
		return nil, err
	}

	// The tool registry connects the configured MCP servers; their tools become
	// this instance's tools (ADR 0001). Zero configured leaves a retrieval-only
	// bot. Connected before the transport starts and closed on shutdown.
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
	// The language pack (ADR 0018): its prompt guidance is layered into the system
	// prompt. Loaded before the engine so an unknown/malformed pack fails startup
	// rather than serving without it. The base "en" adds nothing.
	pack, err := langpacks.Load(c.Language, c.LangpacksDir)
	if err != nil {
		return nil, err
	}
	engine := core.New(m, retriever, toolset, c.Scope,
		core.WithPersona(persona), core.WithPromptTemplate(promptTmpl), core.WithLanguage(pack.Prompt),
		core.WithContextTokens(c.ContextSize))
	return &answering{engine: engine, registry: registry, kbStore: kbStore, toolset: toolset, servers: servers, persona: persona, pack: pack}, nil
}
