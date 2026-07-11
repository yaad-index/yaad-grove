package telegram

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// The model emits CommonMark; toTelegramHTML must render the subset Telegram
// supports and escape everything else. The parse-not-regex cases (snake_case,
// code spans) are the ones a substitution approach gets wrong.
func TestToTelegramHTML(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want string
	}{
		{"plain", "hello world", "hello world"},
		{"bold", "**bold**", "<b>bold</b>"},
		{"italic", "*it*", "<i>it</i>"},
		{"bold inline", "see **this** now", "see <b>this</b> now"},
		{"inline code", "run `go test`", "run <code>go test</code>"},
		{"link", "[docs](https://x.y)", `<a href="https://x.y">docs</a>`},
		{"escape text", "a < b & c > d", "a &lt; b &amp; c &gt; d"},
		{"escape in code", "`a < b`", "<code>a &lt; b</code>"},
		{"heading", "# Title", "<b>Title</b>"},
		// The footgun: intra-word underscores are NOT emphasis (config keys, idents).
		{"snake_case not italic", "set nudge_mode here", "set nudge_mode here"},
		{"double snake", "one_two_three", "one_two_three"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, toTelegramHTML(tc.md))
		})
	}
}

// Block constructs Telegram can't render natively are flattened; exact newline
// shaping is incidental, so assert on the meaningful fragments.
func TestToTelegramHTMLBlocks(t *testing.T) {
	fenced := toTelegramHTML("```\nx := 1\n```")
	assert.Contains(t, fenced, "<pre>x := 1")
	assert.Contains(t, fenced, "</pre>")

	unordered := toTelegramHTML("- a\n- b")
	assert.Contains(t, unordered, "• a")
	assert.Contains(t, unordered, "• b")

	ordered := toTelegramHTML("1. first\n2. second")
	assert.Contains(t, ordered, "1. first")
	assert.Contains(t, ordered, "2. second")

	// A fenced block preserves its contents literally, escaping HTML specials.
	code := toTelegramHTML("```go\nif a < b {}\n```")
	assert.Contains(t, code, "if a &lt; b {}")
}

// Empty / whitespace-only input yields an empty string, so Send skips the HTML
// attempt rather than sending an empty entity.
func TestToTelegramHTMLEmpty(t *testing.T) {
	assert.Empty(t, toTelegramHTML(""))
	assert.Empty(t, toTelegramHTML("   \n  "))
}
