package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
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

// Orderable is the ordered-recall surface the enumerate tool needs from a Store
// (ADR 0022) — the whole-collection ordering primitive behind sort/limit.
type Orderable interface {
	Ordered(ctx context.Context, field string, dir store.Direction, limit int) ([]store.DocRef, error)
}

// Structured is the full structured-lookup surface the built-in tools need from a
// Store: the complete-set primitive (kb_enumerate) plus the value-vocabulary
// primitive (kb_dimensions) plus ordered recall. A store.Store satisfies it.
type Structured interface {
	Enumerable
	Dimensioner
	Orderable
}

// WithEnumerate augments a base tool set (the MCP registry) with the built-in
// structured-lookup tools — kb_enumerate and kb_dimensions — backed by st over the
// instance's declared dimensions and orderable fields (ADR 0019 / 0020 / 0022).
// With neither declared it returns base unchanged, so a bot without structured
// data exposes no structured tool at all.
//
// Enumerate results are complete and uncapped but formatted as compact "Title
// (path)" refs, not chunk bodies: a low-cardinality dimension can resolve to a
// large set, and refs stay cheap while remaining complete (content retrieval within
// an enumerated doc is a separate model step).
//
// Orderable fields alone are enough to advertise kb_enumerate: "what is the latest
// one" carries no facet, so ordered recall is useful in a deployment that declares
// no dimensions. Gating on dimensions alone would leave such an instance with an
// indexed order and nothing able to read it. kb_dimensions still needs dimensions
// to have anything to list, so it is advertised separately.
func WithEnumerate(base core.Tools, st Structured, dimensions, orderable []string) core.Tools {
	if len(dimensions) == 0 && len(orderable) == 0 {
		return base
	}
	return composite{
		base: base,
		enum: enumerateTool{store: st, dimensions: dimensions, orderable: orderable},
		dims: dimensionsTool{store: st, dimensions: dimensions},
		// kb_dimensions is a vocabulary tool for facets; with no dimensions declared
		// it has nothing to list, so it stays unadvertised even when ordering exists.
		hasDims: len(dimensions) > 0,
	}
}

// composite advertises the base tools plus the structured-lookup tools and routes a
// call to whichever owns the name.
type composite struct {
	base    core.Tools
	enum    enumerateTool
	dims    dimensionsTool
	hasDims bool
}

func (c composite) Defs() []core.ToolDef {
	structured := []core.ToolDef{c.enum.def()}
	if c.hasDims {
		structured = append(structured, c.dims.def())
	}
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
		if !c.hasDims {
			break // unadvertised with no dimensions; fall through to base/unknown
		}
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
	store      Structured
	dimensions []string
	orderable  []string
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
	props := map[string]any{
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
	}
	// dimension/value stay required only while faceting is the sole way to ask.
	// Once a field is orderable, "what is the latest one" is a legitimate call that
	// carries no facet at all, so requiring them would make the ordering
	// unreachable except in combination (ADR 0022, the cost of extending this tool
	// rather than adding a second one).
	required := []string{"dimension", "value"}
	desc := "List EVERY document matching the given dimension/value — the complete set, not a sample or the top matches. " +
		"Pass 'and' to require several facets at once (the intersection). " +
		"Declared dimensions: " + strings.Join(e.dimensions, ", ") + ". " +
		"Use this for which/what-covers/list questions; free-text questions use normal grounding instead."
	if len(e.orderable) > 0 {
		props["sort"] = map[string]any{
			"type":        "string",
			"enum":        e.orderable,
			"description": "order the results by this declared field instead of leaving them in document order",
		}
		props["direction"] = map[string]any{
			"type":        "string",
			"enum":        []string{string(store.Descending), string(store.Ascending)},
			"description": "desc = newest/highest first (the default), asc = oldest/lowest first",
		}
		props["limit"] = map[string]any{
			"type":        "integer",
			"minimum":     1,
			"description": "return only the first N after sorting; use 1 for \"the latest\" or \"the newest\"",
		}
		required = nil
		if len(e.dimensions) == 0 {
			desc = "List documents from the knowledge base as a complete set, not a sample or the top matches. "
		}
		desc += " Pass 'sort' with an orderable field to answer recency and ordinal questions — " +
			"the latest, the most recent, the newest, the oldest, the first, the last, the highest-numbered, which is the Nth. " +
			"Those are questions about the WHOLE collection: the ordinary context is a sample and can silently omit the document that decides the answer, so it cannot be read off it. " +
			"Sorting combines with the filters above, which orders only the matching documents. " +
			"Orderable fields: " + strings.Join(e.orderable, ", ") + "."
	}
	root := map[string]any{"type": "object", "properties": props}
	// Omitted rather than emitted as null: a "required": null would be a malformed
	// schema, and some validators read it as an empty constraint while others
	// reject the whole definition.
	if len(required) > 0 {
		root["required"] = required
	}
	schema, err := json.Marshal(root)
	if err != nil {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	return core.ToolDef{Name: enumerateToolName, Description: desc, Schema: schema}
}

// predicate is one {dimension, value} facet filter.
type predicate struct{ dimension, value string }

// call resolves the predicate set (the primary dimension/value plus any 'and'
// filters), intersects their complete sets, and formats the result as compact refs.
func (e enumerateTool) call(ctx context.Context, args map[string]any) (string, error) {
	ord, err := e.parseOrder(args)
	if err != nil {
		return "", err
	}
	preds, err := parsePredicates(args, ord.field != "")
	if err != nil {
		return "", err
	}
	for _, p := range preds {
		if !contains(e.dimensions, p.dimension) {
			return "", fmt.Errorf("kb_enumerate: unknown dimension %q (declared: %s)", p.dimension, strings.Join(e.dimensions, ", "))
		}
	}
	if ord.field == "" {
		refs, err := e.intersect(ctx, preds)
		if err != nil {
			return "", err
		}
		return formatRefs(preds, refs), nil
	}
	refs, err := e.ordered(ctx, preds, ord)
	if err != nil {
		return "", err
	}
	return formatOrdered(preds, ord, refs), nil
}

// order is a parsed sort request: which declared field, which way, and how many.
type order struct {
	field string
	dir   store.Direction
	limit int
}

// parseOrder reads the sort/direction/limit arguments. A sort field must be one of
// the declared orderable ones. Direction defaults to descending, so a bare "the
// latest" means newest-first (ADR 0022, decided by the maintainer).
func (e enumerateTool) parseOrder(args map[string]any) (order, error) {
	field := scalarArg(args, "sort")
	if field == "" {
		return order{}, nil
	}
	if !contains(e.orderable, field) {
		if len(e.orderable) == 0 {
			return order{}, fmt.Errorf("kb_enumerate: this knowledge base declares no orderable fields, so results cannot be sorted")
		}
		return order{}, fmt.Errorf("kb_enumerate: unknown sort field %q (orderable: %s)", field, strings.Join(e.orderable, ", "))
	}
	dir := store.Descending
	switch strings.ToLower(scalarArg(args, "direction")) {
	case "", string(store.Descending):
	case string(store.Ascending):
		dir = store.Ascending
	default:
		return order{}, fmt.Errorf("kb_enumerate: direction must be %q or %q", store.Descending, store.Ascending)
	}
	limit, err := intArg(args, "limit")
	if err != nil {
		return order{}, err
	}
	return order{field: field, dir: dir, limit: limit}, nil
}

// ordered answers a sorted request. With no facet it is the ordered set directly,
// and the cap can be pushed into the store. With facets it is the ordered set
// restricted to the documents that match them.
//
// ⚠️ The cap is applied LAST, never passed to Ordered alongside a facet. Ordering
// first, capping to N, then intersecting would return nothing whenever the newest
// N documents happen not to carry the facet — while matching documents exist, just
// further down the order. That is precisely the defect ADR 0022 exists to remove
// (an answer read off a partial view that omitted the deciding document), rebuilt
// one layer below where it was found.
func (e enumerateTool) ordered(ctx context.Context, preds []predicate, ord order) ([]store.DocRef, error) {
	if len(preds) == 0 {
		return e.store.Ordered(ctx, ord.field, ord.dir, ord.limit)
	}
	matching, err := e.intersect(ctx, preds)
	if err != nil {
		return nil, err
	}
	if len(matching) == 0 {
		return nil, nil
	}
	keep := make(map[string]bool, len(matching))
	for _, r := range matching {
		keep[r.Path] = true
	}
	all, err := e.store.Ordered(ctx, ord.field, ord.dir, 0)
	if err != nil {
		return nil, err
	}
	out := make([]store.DocRef, 0, len(matching))
	for _, r := range all {
		if keep[r.Path] {
			out = append(out, r)
		}
	}
	if ord.limit > 0 && len(out) > ord.limit {
		out = out[:ord.limit]
	}
	return out, nil
}

// parsePredicates reads the required primary {dimension, value} and any optional
// 'and' filters, in order. The primary is first so it drives result ordering.
// sorting relaxes the requirement: a pure recency question ("the latest one")
// carries no facet, so an absent dimension/value is legitimate there. A half-given
// pair is still an error either way — that is a malformed filter, not an omitted
// one, and answering it as though no filter were asked would quietly widen the
// question.
func parsePredicates(args map[string]any, sorting bool) ([]predicate, error) {
	dim, val := scalarArg(args, "dimension"), scalarArg(args, "value")
	if dim == "" && val == "" {
		if sorting {
			return nil, nil
		}
		return nil, fmt.Errorf("kb_enumerate: both dimension and value are required")
	}
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

// formatOrdered renders a sorted result, naming the field and direction so the
// model can see WHICH order it got rather than inferring one from the sequence.
//
// The empty case says the field is carried by no matching document, rather than
// reporting nothing found: a document can match every facet and still be absent
// from an ordered answer because it has no value for the sort field, and those two
// are different facts about the knowledge base.
func formatOrdered(preds []predicate, ord order, refs []store.DocRef) string {
	sense := "highest first"
	if ord.dir == store.Ascending {
		sense = "lowest first"
	}
	scope := "documents"
	if len(preds) > 0 {
		scope = "documents with " + describePredicates(preds)
	}
	if len(refs) == 0 {
		return fmt.Sprintf("No %s carry a usable %s value, so they cannot be ordered by it.", scope, ord.field)
	}
	head := fmt.Sprintf("%d %s, by %s (%s)", len(refs), scope, ord.field, sense)
	if ord.limit > 0 {
		head += fmt.Sprintf(", showing the first %d", ord.limit)
	}
	lines := make([]string, 0, len(refs)+1)
	lines = append(lines, head+":")
	for _, r := range refs {
		if r.Title != "" {
			lines = append(lines, fmt.Sprintf("- %s (%s)", r.Title, r.Path))
		} else {
			lines = append(lines, "- "+r.Path)
		}
	}
	return strings.Join(lines, "\n")
}

// intArg reads a positive integer tool argument. JSON numbers arrive as float64
// through a map[string]any, and some models send them as strings, so both are
// accepted; a non-integer or non-positive value is an error rather than a silent 0,
// which would read as "uncapped" and return the whole collection.
func intArg(args map[string]any, key string) (int, error) {
	raw, ok := args[key]
	if !ok || raw == nil {
		return 0, nil
	}
	var n float64
	switch t := raw.(type) {
	case float64:
		n = t
	case int:
		n = float64(t)
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, nil
		}
		parsed, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, fmt.Errorf("kb_enumerate: %s must be a positive whole number", key)
		}
		n = parsed
	default:
		return 0, fmt.Errorf("kb_enumerate: %s must be a positive whole number", key)
	}
	if n != math.Trunc(n) || n < 1 {
		return 0, fmt.Errorf("kb_enumerate: %s must be a positive whole number", key)
	}
	return int(n), nil
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
