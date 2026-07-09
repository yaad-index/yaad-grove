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
	"fmt"
	"log/slog"
	"os"

	"github.com/alecthomas/kong"
	kongyaml "github.com/alecthomas/kong-yaml"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/model"
	"github.com/yaad-index/yaad-grove/internal/retrieval"
	"github.com/yaad-index/yaad-grove/internal/tools"
	"github.com/yaad-index/yaad-grove/internal/transport"
	"github.com/yaad-index/yaad-grove/internal/transport/telegram"
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

	ModelBaseURL string `name:"model-base-url" default:"https://api.openai.com/v1" help:"OpenAI-compatible API base URL."`
	ModelName    string `name:"model-name" default:"gpt-4o-mini" help:"Model id understood by the endpoint."`

	DefaultTier string `name:"default-tier" default:"default" help:"Tier applied to users without an override."`
}

// Run wires and starts the bot. Scaffold: assembles the pieces and reports that
// behavior is not yet implemented.
func (c *ServeCmd) Run(log *slog.Logger) error {
	m := model.New(model.Config{
		BaseURL: c.ModelBaseURL,
		APIKey:  os.Getenv("YAADGROVE_MODEL_API_KEY"),
		Model:   c.ModelName,
	})
	retriever := retrieval.New(c.VaultDir, 8)
	registry := tools.New(nil) // MCP servers come from config in Phase 1
	engine := core.New(m, retriever, registry, c.Scope)

	// The gate stacks surface-reach -> rate-limit -> consent -> serve; its
	// Store (bbolt) is wired in Phase 1.
	_ = acl.NewGate(nil, acl.Tier(c.DefaultTier))

	var tp transport.Transport = telegram.New(telegram.Config{
		Token: os.Getenv("YAADGROVE_TELEGRAM_TOKEN"),
	})

	log.Info("yaad-grove serve (scaffold)",
		"transport", tp.Name(),
		"model", c.ModelName,
		"vault_dir", c.VaultDir,
		"default_tier", c.DefaultTier,
	)
	_ = engine
	return fmt.Errorf("serve: %w", core.ErrNotImplemented)
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
