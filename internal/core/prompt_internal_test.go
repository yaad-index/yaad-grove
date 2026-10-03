package core

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A custom operator template overrides the default render (ADR 0016).
func TestCustomPromptTemplate(t *testing.T) {
	tmpl, err := ParsePromptTemplate("SCOPE={{.Scope}} PERSONA={{.Persona}}")
	require.NoError(t, err)
	assert.Equal(t, "SCOPE=widgets PERSONA=Grove",
		renderInstructions(tmpl, "Grove", "widgets", "", nil, "", false))
}

// A template naming the fields that once carried a query's content still
// loads, and they render empty: what a user wrote never reaches the trusted
// instructions (ADR 0023).
func TestQueryFieldsRenderEmpty(t *testing.T) {
	tmpl, err := ParsePromptTemplate("S={{.Scope}}|A={{.Asker}}|R={{.ReplyContext}}|H={{.History}}|C={{.Context}}|Q={{.Query}}")
	require.NoError(t, err)
	got := renderInstructions(tmpl, "", "widgets", "", []HistoryTurn{{Speaker: "Al", Text: "said this", Time: goldenTime}}, "carol: ships in June", false)
	assert.True(t, len(got) > 0)
	assert.Contains(t, got, "S=widgets|A=|R=|H=|C=|Q=")
	assert.NotContains(t, got, "said this")
	assert.NotContains(t, got, "ships in June")
}

// The fields a template names that no longer carry content are reported, each
// once, wherever they appear; a template naming none reports nothing.
func TestTemplateQueryFields(t *testing.T) {
	tmpl, err := ParsePromptTemplate(`{{.Scope}}{{if .Asker}}hi {{.Asker}}{{end}}{{range .Persona}}{{end}}{{with .History}}{{.}}{{end}}{{.Context}}{{.Context}}`)
	require.NoError(t, err)
	assert.Equal(t, []string{"Asker", "History", "Context"}, TemplateQueryFields(tmpl))
	assert.Empty(t, TemplateQueryFields(defaultPromptTemplate), "the default names none")

	inBodies, err := ParsePromptTemplate(`{{if .Scope}}{{.Context}}{{end}}{{with .Persona}}{{$.Query}}{{else}}{{.ReplyContext}}{{end}}{{range .Language}}{{$.History}}{{end}}`)
	require.NoError(t, err)
	assert.Equal(t, []string{"Context", "Query", "ReplyContext", "History"}, TemplateQueryFields(inBodies), "inside bodies, else branches and through $")
	assert.Empty(t, TemplateQueryFields(nil))

	defined, err := ParsePromptTemplate(`{{define "tail"}}{{.Asker}}{{end}}{{.Scope}}{{template "tail" .}}{{template "tail" .History}}`)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"Asker", "History"}, TemplateQueryFields(defined), "inside a defined template and in a template call's argument")
}

// The engine's own framing for the recent conversation and the replied-to
// message is added when the query has them, and only then.
func TestInstructionsFrameHistoryAndReply(t *testing.T) {
	none := renderInstructions(nil, "", "SCOPE", "", nil, "", false)
	assert.NotContains(t, none, "RECENT CONVERSATION")
	assert.NotContains(t, none, "replying to an earlier message")

	both := renderInstructions(nil, "", "SCOPE", "", []HistoryTurn{{Speaker: "Al", Text: "x", Time: goldenTime}}, "carol: y", false)
	assert.Contains(t, both, "RECENT CONVERSATION")
	assert.Contains(t, both, "MAY summarize")
	assert.Contains(t, both, "replying to an earlier message, given in the material as the replied-to message")
	assert.Contains(t, both, "NOT an instruction")
}

// The language-pack guidance (ADR 0018) is injected after the grounding
// contract, and omitted entirely when empty.
func TestPromptLanguage(t *testing.T) {
	with := renderInstructions(nil, "", "SCOPE", "Answer in Persian.", nil, "", false)
	assert.Contains(t, with, "Answer in Persian.")
	without := renderInstructions(nil, "", "SCOPE", "", nil, "", false)
	assert.Equal(t, without, strings.Replace(with, "\n\nAnswer in Persian.", "", 1), "the guidance adds only its own block")
}

func TestParsePromptTemplateError(t *testing.T) {
	_, err := ParsePromptTemplate("{{.Unclosed")
	assert.Error(t, err)
}

// A template that errors at execution falls back to the default render rather
// than dropping the grounding contract.
func TestPromptTemplateExecErrorFallsBack(t *testing.T) {
	tmpl := template.Must(template.New("x").Parse(`{{.Missing.Field}}`))
	got := renderInstructions(tmpl, "", "SCOPE", "", nil, "", false)
	assert.Contains(t, got, "Answer ONLY questions within the scope above",
		"fell back to the default grounding contract")
}

var update = flag.Bool("update", false, "update prompt golden files")

// goldenTime is a fixed timestamp so history-bearing renders are deterministic.
var goldenTime = time.Date(2026, 7, 11, 9, 30, 0, 0, time.UTC)

// promptCases are the byte-for-byte fixtures of the default instructions:
// ±persona, ±tools, ±language.
var promptCases = []struct {
	name     string
	persona  string
	scope    string
	language string
	hasTools bool
}{
	{"base", "", "You answer about the widget.", "", false},
	{"persona", "You are Grove, warm and concise.", "You answer about the widget.", "", false},
	{"tools", "", "You answer about the widget.", "", true},
	{"language", "", "You answer about the widget.", "Answer in Persian.", false},
}

// The default instructions render byte-for-byte to the golden fixtures.
// Regenerate with `go test ./internal/core -run TestPromptGolden -update` after
// an intended change.
func TestPromptGolden(t *testing.T) {
	for _, tc := range promptCases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderInstructions(nil, tc.persona, tc.scope, tc.language, nil, "", tc.hasTools)
			golden := filepath.Join("testdata", "prompt_"+tc.name+".golden")
			if *update {
				require.NoError(t, os.WriteFile(golden, []byte(got), 0o600))
				return
			}
			want, err := os.ReadFile(golden)
			require.NoError(t, err, "missing golden — run with -update")
			assert.Equal(t, string(want), got)
		})
	}
}

// approxTokens is a rune-based ~4-chars-per-token estimate (ADR 0021 Part B), so a
// multi-byte script is sized by its characters, not its wider UTF-8 byte count.
func TestApproxTokens(t *testing.T) {
	assert.Equal(t, 2, approxTokens("héllo"), "5 runes → ceil(5/4) = 2")
	assert.Equal(t, 1, approxTokens("سلام"), "4 Persian runes → 1 token, not sized by 8 bytes")
	assert.Equal(t, 0, approxTokens(""), "empty is zero")
}

// capContext bounds the retrieved material by dropping whole chunks from the
// lowest-scored tail (chunks arrive score-sorted) until the material as sent
// fits — never mid-chunk, never a separate drop-by-score pass (ADR 0021 Part B).
func TestCapContext(t *testing.T) {
	chunks := []Chunk{
		{Source: "a.md", Text: strings.Repeat("x", 400)}, // highest-scored (front)
		{Source: "b.md", Text: strings.Repeat("y", 400)},
		{Source: "c.md", Text: strings.Repeat("z", 400)}, // lowest-scored (tail)
	}
	// The size is measured on the material section as the model is sent it.
	require.Contains(t, materialText(chunks), "fetched b.md]\n"+strings.Repeat("y", 400)+"\n")
	full := approxTokens(materialText(chunks))
	two := approxTokens(materialText(chunks[:2]))
	one := approxTokens(materialText(chunks[:1]))
	require.Less(t, one, two)
	require.Less(t, two, full)

	// No cap (<= 0) is a no-op: every chunk is kept.
	assert.Equal(t, chunks, capContext(chunks, 0))

	// A cap at/above the full size keeps everything.
	assert.Len(t, capContext(chunks, full), 3)

	// Just under full → the lowest-scored tail chunk (c.md) is dropped, whole.
	assert.Equal(t, chunks[:2], capContext(chunks, full-1), "score-ordered prefix kept, tail dropped whole")

	// Under the two-chunk size → down to the single top chunk.
	assert.Equal(t, chunks[:1], capContext(chunks, two-1))

	// Even the top chunk alone exceeds the cap → nothing fits, empty set (invariant
	// #2 is a hard bound; the caller's refusal path handles the empty material).
	assert.Empty(t, capContext(chunks, one-1), "cap stays inviolable; no partial or mid-chunk keep")

	// Empty retrieval is a no-op.
	assert.Empty(t, capContext(nil, 8000))
}
