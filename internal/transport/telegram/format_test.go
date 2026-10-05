package telegram

import (
	"testing"

	"github.com/go-telegram/bot/models"
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

// A Markdown link to a non-web destination (a vault file / relative path) is
// de-linked to plain text — no dead <a> in Telegram — while a genuine http(s)
// link stays clickable (#63 follow-up).
func TestToTelegramHTMLDelinksNonWeb(t *testing.T) {
	assert.Equal(t, "see the faq", toTelegramHTML("see [the faq](faq.md)"))
	assert.Equal(t, "steps notes", toTelegramHTML("steps [notes](notes/a.md#Intro)"))
	assert.Equal(t, `see <a href="https://x.y">docs</a>`, toTelegramHTML("see [docs](https://x.y)"))
	assert.Contains(t, toTelegramHTML("[here](http://z)"), `<a href="http://z">here</a>`)
}

// Empty / whitespace-only input yields an empty string, so Send skips the HTML
// attempt rather than sending an empty entity.
func TestToTelegramHTMLEmpty(t *testing.T) {
	assert.Empty(t, toTelegramHTML(""))
	assert.Empty(t, toTelegramHTML("   \n  "))
}

// An HTML block runs to the next blank line, so its text is inside it: the text
// must survive with the tags dropped, escaped like any other text (#199).
func TestToTelegramHTMLKeepsHTMLBlockText(t *testing.T) {
	cases := []struct {
		name string
		md   string
		want string
	}{
		{"one paragraph", "<p>A.</p>", "A."},
		{"two paragraphs", "<p>A.</p>\n<p>B.</p>", "A.\n\nB."},
		{"between markdown", "Intro.\n\n<p>A.</p>\n\nTail.", "Intro.\n\nA.\n\nTail."},
		{"line break", "<p>one<br>two</p>", "one\ntwo"},
		{"list items", "<ul>\n<li>a</li>\n<li>b</li>\n</ul>", "• a\n• b"},
		{"whitespace collapses", "<div>a\n   b\t c</div>", "a b c"},
		{"entities decoded then escaped", "<p>a &amp; b &lt; c</p>", "a &amp; b &lt; c"},
		{"accepted tag stays text", "<div><b>bold</b> and <a href=\"https://x.y\">link</a></div>", "bold and link"},
		{"script and style dropped", "<div>shown <script>hidden()</script><style>p{}</style> too</div>", "shown too"},
		{"pre keeps its line breaks", "<pre>\nfirst\nlast</pre>\n\nAfter.", "first\nlast\n\nAfter."},
		{"collapsing resumes after pre", "<div><pre>a\nb</pre>c\n   d</div>", "a\nb\n\nc d"},
		{"multi-line script dropped", "<script>\nx()\n</script>\n\nAfter.", "After."},
		{"inside a list item", "- item\n\n  <p>inner</p>\n- next", "• item\ninner\n\n• next"},
		{"comment dropped", "<!-- note -->\n\nAfter.", "After."},
		{"empty block", "Before.\n\n<div></div>\n\nAfter.", "Before.\n\nAfter."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, toTelegramHTML(tc.md))
		})
	}
}

// An inline tag carries no text of its own, so dropping it keeps the sentence.
func TestToTelegramHTMLInlineTagDropped(t *testing.T) {
	assert.Equal(t, "Plain inline tag.", toTelegramHTML("Plain <b>inline</b> tag."))
}

// The plain-text fallback carries no markup: code and pre contents lose their
// backticks and are marked with entities instead, so a /command inside one is
// not made a link, and a link keeps its address (#160). Offsets count UTF-16
// code units: the die before the first code span is two.
func TestPlainFromHTML(t *testing.T) {
	text, entities := plainFromHTML(toTelegramHTML("🎲 Use `/start` or **see** [docs](https://x.example/a), <https://y.example> and `a<b`\n\n```\n/help\n```\n"))
	assert.Equal(t, "🎲 Use /start or see docs (https://x.example/a), https://y.example and a<b\n\n/help\n", text)
	assert.Equal(t, []models.MessageEntity{
		{Type: models.MessageEntityTypeCode, Offset: 7, Length: 6},
		{Type: models.MessageEntityTypeCode, Offset: 71, Length: 3},
		{Type: models.MessageEntityTypePre, Offset: 76, Length: 6},
	}, entities)
}

// A code span holding only a backtick shows that one backtick, marked; text with
// no code needs no entities.
func TestPlainFromHTMLBacktickAndNoCode(t *testing.T) {
	text, entities := plainFromHTML(toTelegramHTML("just **text** and `` ` ``"))
	assert.Equal(t, "just text and `", text)
	assert.Equal(t, []models.MessageEntity{{Type: models.MessageEntityTypeCode, Offset: 14, Length: 1}}, entities)
	text, entities = plainFromHTML(toTelegramHTML("just **text**"))
	assert.Equal(t, "just text", text)
	assert.Empty(t, entities)
}
