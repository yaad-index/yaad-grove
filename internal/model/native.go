package model

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// Native tool-call sentinels. Some models (notably deepseek) sometimes emit a
// tool call in their NATIVE syntax inside the assistant `content` channel instead
// of the OpenAI-structured `tool_calls` field (#88). Left unparsed, the tool
// never runs and the raw sentinels leak into the user's reply. The canonical
// shape is:
//
//	<|tool_calls_begin|><|tool_call_begin|>function<|tool_sep|><name>
//	```json
//	<json args>
//	```<|tool_call_end|><|tool_calls_end|>
//
// but real emissions vary: the begin sentinels and the ```json fence are often
// absent, args land on the same line as the name, garbage tokens get glued in
// front of `function`, and the block is sometimes closed by the plural
// <|tool_calls_end|> only. The parser tolerates all of these so the call still
// executes; anything it can't turn into a call is scrubbed rather than leaked.
const (
	nativeToolSep    = "<|tool_sep|>"
	nativeToolEnd    = "<|tool_call_end|>"
	nativeToolsEnd   = "<|tool_calls_end|>"
	nativeCallBegin  = "<|tool_call_begin|>"
	nativeCallsBegin = "<|tool_calls_begin|>"
	// nativeCallPrefix is the token the model writes right before the separator; it
	// is dropped along with the fragment so it doesn't linger in the cleaned text.
	nativeCallPrefix = "function"
)

// nativeSentinels is every sentinel that must never survive into user-facing
// text. Order matters only for stripping (all are removed), not correctness.
var nativeSentinels = []string{
	nativeCallsBegin, nativeCallBegin, nativeToolSep, nativeToolEnd, nativeToolsEnd,
}

// parseNativeToolCalls extracts native-format tool calls from assistant content
// (#88). It returns the content with every native fragment removed — so no
// sentinel reaches the user — plus the extracted calls. A fragment that is
// malformed or unterminated is dropped from the text (the floor) and yields no
// call, so a broken emission degrades to silence rather than leaking syntax or
// running a bad call. Content with no separator is returned unchanged (but a
// stray end-sentinel with no opening separator is still scrubbed).
func parseNativeToolCalls(content string) (string, []core.ToolCall) {
	if !strings.Contains(content, nativeToolSep) {
		if !containsSentinel(content) {
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
		// Keep the text before the fragment, dropping the call-open tokens
		// (`function` and any begin sentinels) that immediately precede the separator.
		clean.WriteString(trimCallOpen(rest[:i]))

		afterSep := rest[i+len(nativeToolSep):]
		end, endLen := firstTerminator(afterSep)
		if end < 0 {
			// Unterminated: no end sentinel. Drop everything from the separator on so a
			// half-written call never leaks, and stop.
			break
		}
		if tc, ok := parseNativeCallBody(afterSep[:end], len(calls)); ok {
			calls = append(calls, tc)
		}
		rest = afterSep[end+endLen:]
	}
	return strings.TrimSpace(stripToolSentinels(clean.String())), calls
}

// trimCallOpen strips the call-open tokens that precede the separator — a
// trailing `function` plus any begin sentinels — leaving only genuine preamble
// text. A non-space garbage token glued directly to `function` (e.g. a stray
// decode artifact) is left as-is: it isn't a sentinel, and once the call
// executes the preamble text is dropped by the tool loop anyway.
func trimCallOpen(pre string) string {
	pre = strings.TrimRight(pre, " \t\r\n")
	pre = strings.TrimSuffix(pre, nativeCallPrefix)
	pre = strings.TrimSuffix(pre, nativeCallBegin)
	pre = strings.TrimSuffix(pre, nativeCallsBegin)
	return pre
}

// firstTerminator finds the earliest per-call end sentinel in s, accepting either
// the singular <|tool_call_end|> or the plural <|tool_calls_end|> (some emissions
// close a lone call with the plural only). It returns the index and the matched
// sentinel's length, or -1 when neither is present.
func firstTerminator(s string) (int, int) {
	i1 := strings.Index(s, nativeToolEnd)
	i2 := strings.Index(s, nativeToolsEnd)
	switch {
	case i1 < 0 && i2 < 0:
		return -1, 0
	case i1 < 0:
		return i2, len(nativeToolsEnd)
	case i2 < 0:
		return i1, len(nativeToolEnd)
	case i1 <= i2:
		return i1, len(nativeToolEnd)
	default:
		return i2, len(nativeToolsEnd)
	}
}

// parseNativeCallBody parses a "<name>\n<json args>" fragment into a tool call.
// idx seeds a synthetic id — the native format carries none, but the tool loop
// needs one to correlate the result on the next round. A missing name or
// malformed args makes it not-ok, so the caller drops the fragment instead of
// dispatching a bad call.
func parseNativeCallBody(body string, idx int) (core.ToolCall, bool) {
	name, rest := splitNativeName(strings.TrimSpace(body))
	name = sanitizeToolName(name)
	if name == "" {
		return core.ToolCall{}, false
	}
	args, ok := parseNativeArgs(rest)
	if !ok {
		return core.ToolCall{}, false
	}
	return core.ToolCall{ID: fmt.Sprintf("call_native_%d", idx), Name: name, Arguments: args}, true
}

// splitNativeName separates the tool name from the args region. The name runs to
// the first newline or the first '{' (some emissions put the JSON on the same
// line as the name). The remainder is the args region, which may be wrapped in a
// ```json fence or trailed by stray text the caller tolerates.
func splitNativeName(body string) (name, rest string) {
	cut := len(body)
	if nl := strings.IndexByte(body, '\n'); nl >= 0 && nl < cut {
		cut = nl
	}
	if b := strings.IndexByte(body, '{'); b >= 0 && b < cut {
		return body[:b], body[b:]
	}
	if cut == len(body) {
		return body, ""
	}
	return body[:cut], body[cut+1:]
}

// sanitizeToolName trims whitespace and any surrounding code-fence backticks from
// the extracted name.
func sanitizeToolName(s string) string {
	return strings.TrimSpace(strings.Trim(strings.TrimSpace(s), "`"))
}

// parseNativeArgs turns the args region into a map. An empty region is a no-arg
// call (ok, empty map). Otherwise the first JSON object is decoded with a
// streaming decoder so a wrapping ```json fence, a residual sentinel, or trailing
// text after the closing brace is tolerated. A non-empty region with no decodable
// object is malformed (not-ok), so the caller drops the call rather than running
// it with silently-empty args.
func parseNativeArgs(rest string) (map[string]any, bool) {
	trimmed := strings.TrimSpace(strings.Trim(strings.TrimSpace(stripToolSentinels(rest)), "`"))
	if trimmed == "" {
		return map[string]any{}, true
	}
	start := strings.IndexByte(trimmed, '{')
	if start < 0 {
		return nil, false
	}
	args := map[string]any{}
	if err := json.NewDecoder(strings.NewReader(trimmed[start:])).Decode(&args); err != nil {
		return nil, false
	}
	return args, true
}

// stripToolSentinels removes any residual native tool-call sentinel so none can
// reach a user-facing message (#88 floor) — the last line of defence for a
// fragment shape parseNativeToolCalls didn't fully consume.
func stripToolSentinels(s string) string {
	for _, t := range nativeSentinels {
		s = strings.ReplaceAll(s, t, "")
	}
	return s
}

// containsSentinel reports whether s holds any native sentinel — used on the
// no-separator path to decide whether the floor scrub is needed.
func containsSentinel(s string) bool {
	for _, t := range nativeSentinels {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}
