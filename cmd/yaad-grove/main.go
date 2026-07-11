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
	"time"

	"github.com/alecthomas/kong"
	kongyaml "github.com/alecthomas/kong-yaml"

	"github.com/yaad-index/yaad-grove/internal/acl"
	"github.com/yaad-index/yaad-grove/internal/budget"
	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/model"
	"github.com/yaad-index/yaad-grove/internal/pending"
	"github.com/yaad-index/yaad-grove/internal/quarantine"
	"github.com/yaad-index/yaad-grove/internal/retrieval"
	"github.com/yaad-index/yaad-grove/internal/runtime"
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

	// The global spend ceiling (ADR 0006): the hard cost backstop the model-call
	// path consults before every call. Conservative default so the bot is
	// cost-capped out of the box; raise it to the real budget. A non-positive
	// ceiling/period is refused (cost-safety cannot be zeroed off).
	SpendCeiling int64         `name:"spend-ceiling" default:"1000000" help:"Global token budget per spend-period — the cost backstop across all users. Conservative default; raise to your budget."`
	SpendPeriod  time.Duration `name:"spend-period" default:"24h" help:"Window the spend-ceiling accumulates over before it resets."`
	BudgetDB     string        `name:"budget-db" default:"./budget.db" help:"Path to the persisted spend-meter database (survives restarts)." type:"path"`

	TelegramGroups []string `name:"telegram-allowed-groups" help:"Group chat ids that count as 'the community' the bot serves (Telegram)."`

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
	retriever := retrieval.New(c.VaultDir, 8)
	registry := tools.New(nil) // MCP servers come from config in Phase 1
	engine := core.New(m, retriever, registry, c.Scope)

	// The gate stacks surface-reach -> rate-limit -> consent -> serve; its
	// Store (bbolt) is wired in Phase 1.
	_ = acl.NewGate(nil, acl.Tier(c.DefaultTier))

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
	_ = qlog // wired into the runtime handler with the full serve loop

	var tp transport.Transport = telegram.New(telegram.Config{
		Token:         os.Getenv("YAADGROVE_TELEGRAM_TOKEN"),
		AllowedGroups: c.TelegramGroups,
	}, callbacks)

	log.Info("yaad-grove serve (scaffold)",
		"transport", tp.Name(),
		"model", c.ModelName,
		"vault_dir", c.VaultDir,
		"default_tier", c.DefaultTier,
		"spend_ceiling_tokens", c.SpendCeiling,
		"spend_period", c.SpendPeriod.String(),
		"spend_remaining_tokens", meter.Remaining(),
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
