package telegram

import (
	"html"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

// mdParser parses CommonMark once; it is stateless and reused across goroutines.
// We use only the parser (the AST) — the rendering is our own walk, because
// Telegram accepts just a small tag subset, not general HTML.
var mdParser = goldmark.New().Parser()

// toTelegramHTML converts the model's Markdown to the subset of HTML Telegram
// renders — <b> <i> <code> <pre> <a> <blockquote> — flattening constructs
// Telegram can't show (headings → a bold line, lists → prefixed lines) and
// escaping all text. The model emits CommonMark, but Telegram needs entities:
// sending the raw Markdown shows literal `**` / “ ` “ / `[](…)` (the #53 bug).
// Parsing rather than regex is what gets code spans and word-internal
// underscores (snake_case, kebab-case config keys) right — they are common in
// the model's answers and are exactly where a regex substitution goes wrong.
func toTelegramHTML(md string) string {
	r := &htmlRenderer{src: []byte(md)}
	doc := mdParser.Parse(text.NewReader(r.src))
	_ = ast.Walk(doc, r.render)
	return strings.TrimSpace(r.b.String())
}

type htmlRenderer struct {
	b     strings.Builder
	src   []byte
	lists []*listState // the enclosing ordered/unordered lists, for item markers
}

type listState struct {
	ordered bool
	item    int
}

func (r *htmlRenderer) inList() bool { return len(r.lists) > 0 }

// render is the ast.Walk visitor: it opens a tag on entry and closes it on exit,
// or writes a self-contained fragment and skips the node's children. Every text
// value is HTML-escaped, so nothing the model wrote can inject a tag.
func (r *htmlRenderer) render(n ast.Node, entering bool) (ast.WalkStatus, error) {
	switch n := n.(type) {
	case *ast.Text:
		if entering {
			r.b.WriteString(html.EscapeString(string(n.Segment.Value(r.src))))
			if n.HardLineBreak() || n.SoftLineBreak() {
				r.b.WriteByte('\n')
			}
		}
	case *ast.String:
		if entering {
			r.b.WriteString(html.EscapeString(string(n.Value)))
		}
	case *ast.AutoLink:
		if entering {
			u := html.EscapeString(string(n.URL(r.src)))
			r.b.WriteString(`<a href="`)
			r.b.WriteString(u)
			r.b.WriteString(`">`)
			r.b.WriteString(u)
			r.b.WriteString("</a>")
		}
		return ast.WalkSkipChildren, nil
	case *ast.CodeSpan:
		if entering {
			r.b.WriteString("<code>")
			r.writeChildText(n) // literal contents, no nested formatting
			r.b.WriteString("</code>")
		}
		return ast.WalkSkipChildren, nil
	case *ast.Emphasis:
		tag := "i"
		if n.Level >= 2 {
			tag = "b"
		}
		if entering {
			r.b.WriteByte('<')
			r.b.WriteString(tag)
			r.b.WriteByte('>')
		} else {
			r.b.WriteString("</")
			r.b.WriteString(tag)
			r.b.WriteByte('>')
		}
	case *ast.Link:
		// De-link a destination that isn't a real web URL (#63 follow-up): the model
		// is told to ground on the vault's [source] tags but not surface them, yet it
		// occasionally emits a Markdown link to a vault file — a dead link in Telegram
		// (the vault isn't web-addressable). Keep genuine http(s) links clickable;
		// render everything else as plain text (the link text still shows).
		if isWebURL(string(n.Destination)) {
			if entering {
				r.b.WriteString(`<a href="`)
				r.b.WriteString(html.EscapeString(string(n.Destination)))
				r.b.WriteString(`">`)
			} else {
				r.b.WriteString("</a>")
			}
		}
	case *ast.Image:
		// Telegram can't inline an image via HTML; render the alt text as a link to a
		// real http(s) source, or plain text for a non-web (dead) destination.
		if isWebURL(string(n.Destination)) {
			if entering {
				r.b.WriteString(`<a href="`)
				r.b.WriteString(html.EscapeString(string(n.Destination)))
				r.b.WriteString(`">`)
			} else {
				r.b.WriteString("</a>")
			}
		}
	case *ast.Heading:
		// Telegram has no headings — render the line bold.
		if entering {
			r.b.WriteString("<b>")
		} else {
			r.b.WriteString("</b>\n\n")
		}
	case *ast.Paragraph:
		if !entering {
			if r.inList() {
				r.b.WriteByte('\n')
			} else {
				r.b.WriteString("\n\n")
			}
		}
	case *ast.TextBlock:
		if !entering {
			r.b.WriteByte('\n')
		}
	case *ast.FencedCodeBlock:
		if entering {
			r.b.WriteString("<pre>")
			r.writeLines(n)
			r.b.WriteString("</pre>\n\n")
		}
		return ast.WalkSkipChildren, nil
	case *ast.CodeBlock:
		if entering {
			r.b.WriteString("<pre>")
			r.writeLines(n)
			r.b.WriteString("</pre>\n\n")
		}
		return ast.WalkSkipChildren, nil
	case *ast.Blockquote:
		if entering {
			r.b.WriteString("<blockquote>")
		} else {
			r.b.WriteString("</blockquote>\n")
		}
	case *ast.List:
		if entering {
			start := n.Start
			if start == 0 {
				start = 1
			}
			r.lists = append(r.lists, &listState{ordered: n.IsOrdered(), item: start})
		} else {
			r.lists = r.lists[:len(r.lists)-1]
			if !r.inList() {
				r.b.WriteByte('\n')
			}
		}
	case *ast.ListItem:
		if entering {
			ls := r.lists[len(r.lists)-1]
			if ls.ordered {
				r.b.WriteString(strconv.Itoa(ls.item))
				r.b.WriteString(". ")
				ls.item++
			} else {
				r.b.WriteString("• ")
			}
		} else {
			r.b.WriteByte('\n')
		}
	case *ast.ThematicBreak:
		if entering {
			r.b.WriteString("—\n")
		}
	case *ast.RawHTML, *ast.HTMLBlock:
		// Drop model-emitted raw HTML rather than forward tags Telegram would
		// reject; the text around it still renders.
		return ast.WalkSkipChildren, nil
	}
	return ast.WalkContinue, nil
}

// writeChildText writes a node's immediate text children, escaped and unadorned —
// for a code span, whose contents must render literally.
func (r *htmlRenderer) writeChildText(n ast.Node) {
	for c := n.FirstChild(); c != nil; c = c.NextSibling() {
		if t, ok := c.(*ast.Text); ok {
			r.b.WriteString(html.EscapeString(string(t.Segment.Value(r.src))))
		}
	}
}

// writeLines writes a code block's raw source lines, escaped.
func (r *htmlRenderer) writeLines(n ast.Node) {
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		r.b.WriteString(html.EscapeString(string(seg.Value(r.src))))
	}
}

// isWebURL reports whether dest is an absolute http(s) URL — the only link
// destinations Telegram can actually open. Bare filenames and relative paths
// (dead vault refs) are not, so the renderer plain-texts them.
func isWebURL(dest string) bool {
	d := strings.ToLower(strings.TrimSpace(dest))
	return strings.HasPrefix(d, "http://") || strings.HasPrefix(d, "https://")
}
