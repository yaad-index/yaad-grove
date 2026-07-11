package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yaad-index/yaad-grove/internal/tools"
)

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
