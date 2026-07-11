package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/tools"
)

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
