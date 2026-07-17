package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/store"
)

// enumerateToolName is the built-in structured-lookup tool the model calls for
// complete "which/list" answers (ADR 0019).
const enumerateToolName = "kb_enumerate"

// Enumerable is the structured-lookup surface the enumerate tool needs from a
// Store — the complete-set primitive, not top-k.
type Enumerable interface {
	Enumerate(ctx context.Context, dimension, value string) ([]store.DocRef, error)
}

// Structured is the full structured-lookup surface the built-in tools need from a
// Store: the complete-set primitive (kb_enumerate) plus the value-vocabulary
// primitive (kb_dimensions). A store.Store satisfies it.
type Structured interface {
	Enumerable
	Dimensioner
}

// WithEnumerate augments a base tool set (the MCP registry) with the built-in
// structured-lookup tools — kb_enumerate and kb_dimensions — backed by st over the
// instance's declared dimensions (ADR 0019 / 0020). With no dimensions declared it
// returns base unchanged, so a bot without structured data exposes neither tool.
// Enumerate results are complete and uncapped but formatted as compact "Title
// (path)" refs, not chunk bodies: a low-cardinality dimension can resolve to a
// large set, and refs stay cheap while remaining complete (content retrieval within
// an enumerated doc is a separate model step).
func WithEnumerate(base core.Tools, st Structured, dimensions []string) core.Tools {
	if len(dimensions) == 0 {
		return base
	}
	return composite{
		base: base,
		enum: enumerateTool{store: st, dimensions: dimensions},
		dims: dimensionsTool{store: st, dimensions: dimensions},
	}
}

// composite advertises the base tools plus the structured-lookup tools and routes a
// call to whichever owns the name.
type composite struct {
	base core.Tools
	enum enumerateTool
	dims dimensionsTool
}

func (c composite) Defs() []core.ToolDef {
	structured := []core.ToolDef{c.enum.def(), c.dims.def()}
	if c.base == nil {
		return structured
	}
	return append(c.base.Defs(), structured...)
}

func (c composite) Call(ctx context.Context, name string, args map[string]any) (string, error) {
	switch name {
	case enumerateToolName:
		return c.enum.call(ctx, args)
	case dimensionsToolName:
		return c.dims.call(ctx, args)
	}
	if c.base == nil {
		return "", fmt.Errorf("tools: unknown tool %q", name)
	}
	return c.base.Call(ctx, name, args)
}

// enumerateTool is the kb_enumerate implementation over a Store's Enumerate, scoped
// to the declared dimensions.
type enumerateTool struct {
	store      Enumerable
	dimensions []string
}

// def is the advertised definition: the description names the declared dimensions
// and the schema constrains `dimension` to them, so the model can only ask for
// dimensions that exist.
func (e enumerateTool) def() core.ToolDef {
	schema, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"dimension": map[string]any{
				"type":        "string",
				"enum":        e.dimensions,
				"description": "which declared attribute to filter on",
			},
			"value": map[string]any{
				"type":        "string",
				"description": "the value to match; any known spelling or alias resolves",
			},
		},
		"required": []string{"dimension", "value"},
	})
	if err != nil {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	desc := "List EVERY document whose given dimension equals the given value — the complete set, not a sample or the top matches. " +
		"Declared dimensions: " + strings.Join(e.dimensions, ", ") + ". " +
		"Use this for which/what-covers/list questions; free-text questions use normal grounding instead."
	return core.ToolDef{Name: enumerateToolName, Description: desc, Schema: schema}
}

// call runs the lookup and formats the complete result as compact refs.
func (e enumerateTool) call(ctx context.Context, args map[string]any) (string, error) {
	dimension := scalarArg(args, "dimension")
	value := scalarArg(args, "value")
	if dimension == "" || value == "" {
		return "", fmt.Errorf("kb_enumerate: both dimension and value are required")
	}
	if !contains(e.dimensions, dimension) {
		return "", fmt.Errorf("kb_enumerate: unknown dimension %q (declared: %s)", dimension, strings.Join(e.dimensions, ", "))
	}
	refs, err := e.store.Enumerate(ctx, dimension, value)
	if err != nil {
		return "", err
	}
	return formatRefs(dimension, value, refs), nil
}

// formatRefs renders the complete ref set as one compact line per document, so a
// large complete set stays prompt-cheap.
func formatRefs(dimension, value string, refs []store.DocRef) string {
	if len(refs) == 0 {
		return fmt.Sprintf("No documents found with %s = %q.", dimension, value)
	}
	lines := make([]string, 0, len(refs)+1)
	lines = append(lines, fmt.Sprintf("%d document(s) with %s = %q:", len(refs), dimension, value))
	for _, r := range refs {
		if r.Title != "" {
			lines = append(lines, fmt.Sprintf("- %s (%s)", r.Title, r.Path))
		} else {
			lines = append(lines, "- "+r.Path)
		}
	}
	return strings.Join(lines, "\n")
}

// scalarArg reads a string tool argument, trimmed; a missing or non-string arg is "".
func scalarArg(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
