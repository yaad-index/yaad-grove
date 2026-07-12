package model

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// Native tool-call sentinels. Some models (notably deepseek) sometimes emit a
// tool call in their NATIVE syntax inside the assistant `content` channel instead
// of the OpenAI-structured `tool_calls` field (#88). Left unparsed, the raw
// sentinels leak into the user's reply and the tool never runs. The shape is:
//
//	function<|tool_sep|><name>
//	<json args>
//	<|tool_call_end|>
const (
	nativeToolSep = "<|tool_sep|>"
	nativeToolEnd = "<|tool_call_end|>"
	// nativeCallPrefix is the token the model writes right before the separator; it
	// is dropped along with the fragment so it doesn't linger in the cleaned text.
	nativeCallPrefix = "function"
)

// parseNativeToolCalls extracts native-format tool calls from assistant content
// (#88). It returns the content with every native fragment removed — so no
// sentinel reaches the user — plus the extracted calls. A fragment that is
// malformed or unterminated is dropped from the text (the floor) and yields no
// call, so a broken emission degrades to silence rather than leaking syntax or
// running a bad call. Content with no separator is returned unchanged.
func parseNativeToolCalls(content string) (string, []core.ToolCall) {
	if !strings.Contains(content, nativeToolSep) {
		// No opening separator, so there is no call to extract. Ordinary text is
		// returned verbatim, but a stray lone end-sentinel (no matching sep) must still
		// be scrubbed so it can't slip past the floor.
		if !strings.Contains(content, nativeToolEnd) {
			return content, nil
		}
		return strings.TrimSpace(stripToolSentinels(content)), nil
	}
	var calls []core.ToolCall
	var clean strings.Builder
	rest := content
	for {
		i := strings.Index(rest, nativeToolSep)
		if i < 0 {
			clean.WriteString(rest)
			break
		}
		// Keep the text before the fragment, dropping a trailing "function" token that
		// immediately precedes the separator (it is part of the call syntax).
		pre := strings.TrimRight(rest[:i], " \t")
		pre = strings.TrimSuffix(pre, nativeCallPrefix)
		clean.WriteString(pre)

		afterSep := rest[i+len(nativeToolSep):]
		end := strings.Index(afterSep, nativeToolEnd)
		if end < 0 {
			// Unterminated: no end sentinel. Drop everything from the separator on so a
			// half-written call never leaks, and stop.
			break
		}
		if tc, ok := parseNativeCallBody(afterSep[:end], len(calls)); ok {
			calls = append(calls, tc)
		}
		rest = afterSep[end+len(nativeToolEnd):]
	}
	return strings.TrimSpace(stripToolSentinels(clean.String())), calls
}

// parseNativeCallBody parses a "<name>\n<json args>" fragment into a tool call.
// idx seeds a synthetic id — the native format carries none, but the tool loop
// needs one to correlate the result on the next round. A missing name or
// unparseable args makes it not-ok, so the caller drops the fragment instead of
// dispatching a malformed call.
func parseNativeCallBody(body string, idx int) (core.ToolCall, bool) {
	name, argsStr, _ := strings.Cut(strings.TrimSpace(body), "\n")
	name = strings.TrimSpace(name)
	if name == "" {
		return core.ToolCall{}, false
	}
	args := map[string]any{}
	if s := strings.TrimSpace(argsStr); s != "" {
		if err := json.Unmarshal([]byte(s), &args); err != nil {
			return core.ToolCall{}, false
		}
	}
	return core.ToolCall{ID: fmt.Sprintf("call_native_%d", idx), Name: name, Arguments: args}, true
}

// stripToolSentinels removes any residual native tool-call sentinels so they can
// never reach a user-facing message (#88 floor) — the last line of defence for a
// fragment shape parseNativeToolCalls didn't fully consume.
func stripToolSentinels(s string) string {
	s = strings.ReplaceAll(s, nativeToolSep, "")
	s = strings.ReplaceAll(s, nativeToolEnd, "")
	return s
}
