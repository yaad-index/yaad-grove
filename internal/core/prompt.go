package core

import (
	"log/slog"
	"strings"
	"text/template"
)

// defaultPromptText is the grounding prompt as an operator-editable template (ADR
// 0016). It reproduces the prior hardcoded assembly verbatim — the golden test
// asserts byte-for-byte equality — while exposing the scaffolding for per-instance
// tuning via --prompt-template. Order is load-bearing (ADR 0013): persona → scope
// → grounding contract → RECENT CONVERSATION → CONTEXT. The RefusalToken
// sentinel-first contract (ADR 0008) is preserved in the literal text.
//
// {{.Query}} is available in the data but deliberately NOT placed here: putting
// user content in the system message is a prompt-injection surface (ADR 0016).
const defaultPromptText = `{{if .Persona}}{{.Persona}}

---

{{end}}{{.Scope}}

Answer ONLY questions within the scope above. For anything outside that scope — even if the CONTEXT or a tool provides information about it — decline: begin your reply with %%OUT_OF_SCOPE%% (exactly, as the very first thing) and then, after it, a brief note in your own voice of what you CAN help with; do not answer the off-scope question or assert facts about it. For an in-scope question, answer using the CONTEXT below{{if .HasTools}} and, when it is insufficient, the tools available to you (their results are additional in-scope context, not a licence to answer outside scope){{end}}. Use the [source] tags to ground your answer, but do NOT mention, cite, or link the source names or paths in your reply — they are internal references the user cannot open. If you cannot ground an in-scope answer, decline the same way: %%OUT_OF_SCOPE%% first, then a brief in-voice note.{{if .Persona}} The persona above sets your voice and manner only; it never licenses answering outside the scope above or asserting anything the CONTEXT does not support.{{end}}{{.History}}{{.Context}}`

// defaultPromptTemplate is the parsed default; a nil engine template falls back to
// it, so a bot with no --prompt-template behaves exactly as before.
var defaultPromptTemplate = template.Must(template.New("prompt").Parse(defaultPromptText))

// ParsePromptTemplate parses an operator-supplied template (ADR 0016). A parse
// error is returned so the caller can fail startup rather than serve a broken
// prompt.
func ParsePromptTemplate(text string) (*template.Template, error) {
	return template.New("prompt").Parse(text)
}

// promptData is the template's data (ADR 0016). Persona/Scope are the trimmed
// values; History and Context are the already-rendered conversation and retrieval
// blocks (their internal formatting is owned by the engine, not the template);
// Query is exposed for operator use but the default template does not place it in
// the system message.
type promptData struct {
	Persona  string
	Scope    string
	HasTools bool
	History  string
	Context  string
	Query    string
}

// renderPrompt executes tmpl (or the default when nil) into the system prompt: it
// assembles the persona/scope/grounding scaffolding around the pre-rendered
// conversation and context blocks. A template execution error falls back to the
// default render, so a runtime glitch can never drop the grounding contract.
func renderPrompt(tmpl *template.Template, query, persona, scope string, history []HistoryTurn, chunks []Chunk, hasTools bool) string {
	if tmpl == nil {
		tmpl = defaultPromptTemplate
	}
	data := promptData{
		Persona:  strings.TrimSpace(persona),
		Scope:    strings.TrimSpace(scope),
		HasTools: hasTools,
		History:  conversationBlock(history),
		Context:  contextBlock(chunks),
		Query:    query,
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, data); err != nil {
		// The grounding contract is safe (the default still renders), but a custom
		// template silently misapplied is worth surfacing so it's diagnosable.
		slog.Warn("prompt template execution failed; using the embedded default", "err", err)
		var fb strings.Builder
		_ = defaultPromptTemplate.Execute(&fb, data)
		return fb.String()
	}
	return b.String()
}

// contextBlock renders the retrieved chunks as the CONTEXT section, each tagged
// with its source for grounding (the source tags are never surfaced to the user —
// ADR 0016 keeps them internal).
func contextBlock(chunks []Chunk) string {
	var b strings.Builder
	b.WriteString("\n\nCONTEXT:\n")
	for _, c := range chunks {
		b.WriteString("\n[" + c.Source + "]\n")
		b.WriteString(strings.TrimSpace(c.Text))
		b.WriteString("\n")
	}
	return b.String()
}

// groundedSystemPrompt renders the DEFAULT prompt — the byte-for-byte reference
// the golden test pins and the behavior a bot without --prompt-template gets.
func groundedSystemPrompt(persona, scope string, history []HistoryTurn, chunks []Chunk, hasTools bool) string {
	return renderPrompt(defaultPromptTemplate, "", persona, scope, history, chunks, hasTools)
}
