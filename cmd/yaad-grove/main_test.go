package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/retrieval"
	"github.com/yaad-index/yaad-grove/internal/tools"
)

// The gated default matters: without --similarity-threshold, retrieval must
// resolve to the block-early 0.30 floor. A dropped default tag would silently
// flip the shipped default to brain-judges (0.0), so pin it via a real parse.
func TestSimilarityThresholdDefault(t *testing.T) {
	var cli CLI
	parser, err := kong.New(&cli)
	require.NoError(t, err)
	_, err = parser.Parse([]string{"serve"})
	require.NoError(t, err)
	assert.Equal(t, float32(0.30), cli.Serve.SimilarityThreshold, "block-early 0.30 is the shipped default")
}

// The transcript is off by default (ADR 0015): without --transcript-log the field
// is empty, and setting it flows through as the directory path.
func TestTranscriptLogDefaultOff(t *testing.T) {
	var cli CLI
	parser, err := kong.New(&cli)
	require.NoError(t, err)

	_, err = parser.Parse([]string{"serve"})
	require.NoError(t, err)
	assert.Empty(t, cli.Serve.TranscriptLog, "no transcript by default")

	_, err = parser.Parse([]string{"serve", "--transcript-log", "/tmp/grove-transcripts"})
	require.NoError(t, err)
	assert.Equal(t, "/tmp/grove-transcripts", cli.Serve.TranscriptLog)
}

// Retriever selection (ADR 0017): no embedding config → keyword; an incomplete
// embedding pair → startup error.
func TestBuildRetriever(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	r, err := buildRetriever(&ServeCmd{VaultDir: "vault"}, log)
	require.NoError(t, err)
	_, isKeyword := r.(*retrieval.FullText)
	assert.True(t, isKeyword, "no embedding endpoint → keyword retriever (zero-config default)")

	_, err = buildRetriever(&ServeCmd{VaultDir: "vault", EmbeddingBaseURL: "http://x"}, log)
	assert.Error(t, err, "base-url without model is a startup error")

	_, err = buildRetriever(&ServeCmd{VaultDir: "vault", EmbeddingModel: "bge-m3"}, log)
	assert.Error(t, err, "model without base-url is a startup error")
}

// The default persona path (PERSONA.md, working dir) is graceful when absent and
// used when present; an explicitly-configured path is loaded, and a missing
// explicit path is fatal (ADR 0013).
func TestLoadPersonaDefaultAbsentIsGraceful(t *testing.T) {
	t.Chdir(t.TempDir()) // an empty dir — no PERSONA.md
	got, err := loadPersona("")
	require.NoError(t, err)
	assert.Empty(t, got, "an absent default persona file is not an error")
}

func TestLoadPersonaDefaultPresent(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "PERSONA.md"), []byte("be kind"), 0o600))
	t.Chdir(dir)
	got, err := loadPersona("")
	require.NoError(t, err)
	assert.Equal(t, "be kind", got, "a present default PERSONA.md is loaded")
}

func TestLoadPersonaExplicitMissingIsFatal(t *testing.T) {
	_, err := loadPersona(filepath.Join(t.TempDir(), "nope.md"))
	assert.Error(t, err, "an explicitly-set but missing persona file is fatal")
}

func TestLoadPersonaExplicitPresent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "custom.md")
	require.NoError(t, os.WriteFile(p, []byte("custom voice"), 0o600))
	got, err := loadPersona(p)
	require.NoError(t, err)
	assert.Equal(t, "custom voice", got)
}

// The prompt template loads: empty → the embedded default (nil), a valid file
// parses, and a missing or unparseable path is fatal (ADR 0016).
func TestLoadPromptTemplate(t *testing.T) {
	tmpl, err := loadPromptTemplate("")
	require.NoError(t, err)
	assert.Nil(t, tmpl, "empty path uses the embedded default")

	p := filepath.Join(t.TempDir(), "p.tmpl")
	require.NoError(t, os.WriteFile(p, []byte("{{.Scope}}"), 0o600))
	tmpl, err = loadPromptTemplate(p)
	require.NoError(t, err)
	assert.NotNil(t, tmpl)

	_, err = loadPromptTemplate(filepath.Join(t.TempDir(), "nope.tmpl"))
	assert.Error(t, err, "a missing explicit template is fatal")

	bad := filepath.Join(t.TempDir(), "bad.tmpl")
	require.NoError(t, os.WriteFile(bad, []byte("{{.Unclosed"), 0o600))
	_, err = loadPromptTemplate(bad)
	assert.Error(t, err, "an unparseable template is fatal")
}

func TestParseMCPServers(t *testing.T) {
	got, err := parseMCPServers([]string{
		"search=/usr/bin/mcp-search --vault /data",
		"wiki=mcp-wiki",
	})
	require.NoError(t, err)
	assert.Equal(t, []tools.ServerConfig{
		{Name: "search", Command: "/usr/bin/mcp-search", Args: []string{"--vault", "/data"}},
		{Name: "wiki", Command: "mcp-wiki", Args: []string{}},
	}, got)
}

func TestParseMCPServersEmpty(t *testing.T) {
	got, err := parseMCPServers(nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestParseMCPServersInvalid(t *testing.T) {
	for _, spec := range []string{
		"noequals",   // missing name=command separator
		"=command",   // empty name
		"  =command", // blank name
		"name=",      // no command
		"name=   ",   // only whitespace command
	} {
		_, err := parseMCPServers([]string{spec})
		assert.Error(t, err, "spec %q is rejected", spec)
	}
}
