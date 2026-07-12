package model

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A preamble followed by a native tool call: the call is extracted (name + args)
// and the preamble is kept, with no sentinel surviving in the text (#88).
func TestParseNativeToolCallBasic(t *testing.T) {
	content := "let me check that for you\n\n" +
		"function" + nativeToolSep + "search\n" +
		`{"q":"acme-game"}` + "\n" +
		nativeToolEnd

	text, calls := parseNativeToolCalls(content)

	require.Len(t, calls, 1)
	assert.Equal(t, "search", calls[0].Name)
	assert.Equal(t, "acme-game", calls[0].Arguments["q"])
	assert.NotEmpty(t, calls[0].ID, "a synthetic id is assigned for correlation")

	assert.Equal(t, "let me check that for you", text, "preamble kept, fragment removed")
	assertNoSentinels(t, text)
}

// Multiple native calls in one message are all extracted, with distinct ids.
func TestParseNativeToolCallMultiple(t *testing.T) {
	content := "function" + nativeToolSep + "search\n" + `{"q":"a"}` + "\n" + nativeToolEnd +
		"\nfunction" + nativeToolSep + "get_things\n" + `{"id":"7"}` + "\n" + nativeToolEnd

	text, calls := parseNativeToolCalls(content)
	require.Len(t, calls, 2)
	assert.Equal(t, "search", calls[0].Name)
	assert.Equal(t, "get_things", calls[1].Name)
	assert.NotEqual(t, calls[0].ID, calls[1].ID, "ids are distinct")
	assertNoSentinels(t, text)
}

// A call with no args parses to an empty argument map, not a failure.
func TestParseNativeToolCallNoArgs(t *testing.T) {
	content := "function" + nativeToolSep + "get_hotness\n" + nativeToolEnd
	_, calls := parseNativeToolCalls(content)
	require.Len(t, calls, 1)
	assert.Equal(t, "get_hotness", calls[0].Name)
	assert.Empty(t, calls[0].Arguments)
}

// Content with no separator is returned unchanged and yields no calls.
func TestParseNativeToolCallPassthrough(t *testing.T) {
	text, calls := parseNativeToolCalls("just a normal answer about the rules")
	assert.Equal(t, "just a normal answer about the rules", text)
	assert.Empty(t, calls)
}

// An unterminated fragment (no end sentinel) leaks nothing and yields no call —
// the floor drops everything from the separator on.
func TestParseNativeToolCallUnterminated(t *testing.T) {
	content := "here you go\n\nfunction" + nativeToolSep + "search\n" + `{"q":"x"}`
	text, calls := parseNativeToolCalls(content)
	assert.Empty(t, calls, "an unterminated fragment produces no call")
	assert.Equal(t, "here you go", text)
	assertNoSentinels(t, text)
}

// A fragment whose args aren't valid JSON is dropped (no call), and no sentinel
// leaks — a malformed call degrades to silence, never a bad dispatch.
func TestParseNativeToolCallBadArgs(t *testing.T) {
	content := "function" + nativeToolSep + "search\n" + "not json" + "\n" + nativeToolEnd
	text, calls := parseNativeToolCalls(content)
	assert.Empty(t, calls, "unparseable args drop the call")
	assertNoSentinels(t, text)
}

// The floor: any residual sentinel is scrubbed even outside a well-formed
// fragment.
func TestStripToolSentinels(t *testing.T) {
	assert.Equal(t, "hello world", stripToolSentinels("hello "+nativeToolSep+"world"+nativeToolEnd))
}

// A lone end-sentinel with NO opening separator must still be scrubbed — the
// no-separator early path routes around the fragment parser, so it has to apply
// the floor itself or the sentinel leaks (the gap this test pins).
func TestParseNativeToolCallLoneEndSentinel(t *testing.T) {
	text, calls := parseNativeToolCalls("here is your answer " + nativeToolEnd)
	assert.Empty(t, calls, "no opening separator → no call")
	assert.Equal(t, "here is your answer", text, "the stray end-sentinel is scrubbed, not leaked")
	assertNoSentinels(t, text)
}

func assertNoSentinels(t *testing.T, text string) {
	t.Helper()
	assert.False(t, strings.Contains(text, nativeToolSep), "no <|tool_sep|> leaks")
	assert.False(t, strings.Contains(text, nativeToolEnd), "no <|tool_call_end|> leaks")
	assert.False(t, strings.Contains(text, nativeCallPrefix+nativeToolSep), "no function<|… leaks")
}
