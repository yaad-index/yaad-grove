package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// dimensionsToolName is the built-in value-vocabulary tool the model calls to learn
// which values a dimension holds before enumerating one (ADR 0020).
const dimensionsToolName = "kb_dimensions"

// maxVocabPerDimension bounds how many values kb_dimensions lists for a single
// dimension, so a high-cardinality attribute (hundreds of designers) can't bloat
// the tool output or the prompt (ADR 0020). Only this discovery listing is bounded;
// kb_enumerate stays complete and uncapped.
const maxVocabPerDimension = 50

// Dimensioner is the value-vocabulary surface the dimensions tool needs from a
// Store: each declared dimension's distinct values by display form.
type Dimensioner interface {
	Dimensions(ctx context.Context) (map[string][]string, error)
}

// dimensionsTool is the kb_dimensions implementation over a Store's Dimensions,
// scoped to the declared dimensions.
type dimensionsTool struct {
	store      Dimensioner
	dimensions []string
}

// def advertises kb_dimensions. The optional `dimension` arg (constrained to the
// declared set) narrows the listing to one dimension; omitted, it lists all.
func (d dimensionsTool) def() core.ToolDef {
	schema, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"dimension": map[string]any{
				"type":        "string",
				"enum":        d.dimensions,
				"description": "optional: list values for just this dimension; omit for all",
			},
		},
	})
	if err != nil {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	desc := "List the declared dimensions and the values each holds, so you can pick a valid value for kb_enumerate. " +
		"Declared dimensions: " + strings.Join(d.dimensions, ", ") + ". " +
		"Call this before kb_enumerate when unsure how a value is spelled. High-cardinality dimensions are listed up to a cap, with the total count."
	return core.ToolDef{Name: dimensionsToolName, Description: desc, Schema: schema}
}

// call fetches the current vocabulary and formats it, optionally narrowed to one
// declared dimension.
func (d dimensionsTool) call(ctx context.Context, args map[string]any) (string, error) {
	only := scalarArg(args, "dimension")
	if only != "" && !contains(d.dimensions, only) {
		return "", fmt.Errorf("kb_dimensions: unknown dimension %q (declared: %s)", only, strings.Join(d.dimensions, ", "))
	}
	vocab, err := d.store.Dimensions(ctx)
	if err != nil {
		return "", err
	}
	return formatVocab(d.dimensions, vocab, only), nil
}

// formatVocab renders the declared dimensions (in declared order, for stable
// output) and their values, capping each dimension's listing and noting the total
// when it truncates.
func formatVocab(declared []string, vocab map[string][]string, only string) string {
	lines := make([]string, 0, len(declared)+1)
	for _, dim := range declared {
		if only != "" && dim != only {
			continue
		}
		vals := vocab[dim]
		if len(vals) == 0 {
			lines = append(lines, fmt.Sprintf("- %s: (no values indexed yet)", dim))
			continue
		}
		if len(vals) > maxVocabPerDimension {
			shown := strings.Join(vals[:maxVocabPerDimension], ", ")
			lines = append(lines, fmt.Sprintf("- %s: %s … (%d total, showing %d)", dim, shown, len(vals), maxVocabPerDimension))
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", dim, strings.Join(vals, ", ")))
	}
	if len(lines) == 0 {
		return "No declared dimensions."
	}
	return "Declared dimensions and their values (use a listed value with kb_enumerate):\n" + strings.Join(lines, "\n")
}
