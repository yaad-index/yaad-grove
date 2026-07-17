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
	filter := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"dimension": map[string]any{"type": "string", "enum": e.dimensions},
			"value":     map[string]any{"type": "string"},
		},
		"required": []string{"dimension", "value"},
	}
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
			"and": map[string]any{
				"type":        "array",
				"items":       filter,
				"description": "optional additional {dimension, value} filters; a document must match ALL of them plus the primary dimension/value (AND). Use for compound facets like \"train games for 2 players\".",
			},
		},
		"required": []string{"dimension", "value"},
	})
	if err != nil {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	desc := "List EVERY document matching the given dimension/value — the complete set, not a sample or the top matches. " +
		"Pass 'and' to require several facets at once (the intersection). " +
		"Declared dimensions: " + strings.Join(e.dimensions, ", ") + ". " +
		"Use this for which/what-covers/list questions; free-text questions use normal grounding instead."
	return core.ToolDef{Name: enumerateToolName, Description: desc, Schema: schema}
}

// predicate is one {dimension, value} facet filter.
type predicate struct{ dimension, value string }

// call resolves the predicate set (the primary dimension/value plus any 'and'
// filters), intersects their complete sets, and formats the result as compact refs.
func (e enumerateTool) call(ctx context.Context, args map[string]any) (string, error) {
	preds, err := parsePredicates(args)
	if err != nil {
		return "", err
	}
	for _, p := range preds {
		if !contains(e.dimensions, p.dimension) {
			return "", fmt.Errorf("kb_enumerate: unknown dimension %q (declared: %s)", p.dimension, strings.Join(e.dimensions, ", "))
		}
	}
	refs, err := e.intersect(ctx, preds)
	if err != nil {
		return "", err
	}
	return formatRefs(preds, refs), nil
}

// parsePredicates reads the required primary {dimension, value} and any optional
// 'and' filters, in order. The primary is first so it drives result ordering.
func parsePredicates(args map[string]any) ([]predicate, error) {
	dim, val := scalarArg(args, "dimension"), scalarArg(args, "value")
	if dim == "" || val == "" {
		return nil, fmt.Errorf("kb_enumerate: both dimension and value are required")
	}
	preds := []predicate{{dim, val}}
	raw, ok := args["and"]
	if !ok || raw == nil {
		return preds, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("kb_enumerate: 'and' must be a list of {dimension, value} filters")
	}
	for _, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("kb_enumerate: each 'and' filter must be an object with dimension and value")
		}
		d, v := scalarArg(m, "dimension"), scalarArg(m, "value")
		if d == "" || v == "" {
			return nil, fmt.Errorf("kb_enumerate: each 'and' filter needs both dimension and value")
		}
		preds = append(preds, predicate{d, v})
	}
	return preds, nil
}

// intersect enumerates each predicate's complete set and AND-joins them by document
// path, preserving the primary predicate's order. Each leg is itself complete, so
// the intersection is exact and deterministic (ADR 0020).
func (e enumerateTool) intersect(ctx context.Context, preds []predicate) ([]store.DocRef, error) {
	result, err := e.store.Enumerate(ctx, preds[0].dimension, preds[0].value)
	if err != nil {
		return nil, err
	}
	for _, p := range preds[1:] {
		if len(result) == 0 {
			break
		}
		refs, err := e.store.Enumerate(ctx, p.dimension, p.value)
		if err != nil {
			return nil, err
		}
		keep := make(map[string]bool, len(refs))
		for _, r := range refs {
			keep[r.Path] = true
		}
		filtered := result[:0]
		for _, r := range result {
			if keep[r.Path] {
				filtered = append(filtered, r)
			}
		}
		result = filtered
	}
	return result, nil
}

// formatRefs renders the complete ref set as one compact line per document, so a
// large complete set stays prompt-cheap. The header names the predicate(s), joined
// with AND for a compound query.
func formatRefs(preds []predicate, refs []store.DocRef) string {
	desc := describePredicates(preds)
	if len(refs) == 0 {
		return fmt.Sprintf("No documents found with %s.", desc)
	}
	lines := make([]string, 0, len(refs)+1)
	lines = append(lines, fmt.Sprintf("%d document(s) with %s:", len(refs), desc))
	for _, r := range refs {
		if r.Title != "" {
			lines = append(lines, fmt.Sprintf("- %s (%s)", r.Title, r.Path))
		} else {
			lines = append(lines, "- "+r.Path)
		}
	}
	return strings.Join(lines, "\n")
}

// describePredicates renders the predicate set as "dim = "val"" clauses joined by
// AND, for the result header and the empty-set message.
func describePredicates(preds []predicate) string {
	parts := make([]string, len(preds))
	for i, p := range preds {
		parts[i] = fmt.Sprintf("%s = %q", p.dimension, p.value)
	}
	return strings.Join(parts, " AND ")
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
