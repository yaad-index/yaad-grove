package retrieval

import (
	"context"
	"log/slog"

	"github.com/yaad-index/yaad-grove/internal/core"
)

// Fallback wraps a primary retriever with a secondary: on a query-time ERROR from
// the primary it logs and retries with the secondary (ADR 0017). It is how a
// semantic retriever whose embedding endpoint blips mid-session degrades to
// keyword rather than failing the whole query. An empty (non-error) primary
// result is a valid "nothing relevant" — it is NOT a failure and does not fall
// back, so the grounding block still fires when the threshold is genuinely unmet.
type Fallback struct {
	Primary   core.Retriever
	Secondary core.Retriever
}

// Retrieve tries Primary; on its error, logs and returns Secondary's result.
func (f Fallback) Retrieve(ctx context.Context, query string) ([]core.Chunk, error) {
	chunks, err := f.Primary.Retrieve(ctx, query)
	if err != nil {
		slog.Warn("retrieval: primary retriever failed; falling back to keyword", "err", err)
		return f.Secondary.Retrieve(ctx, query)
	}
	return chunks, nil
}

// compile-time assertion that Fallback satisfies core.Retriever.
var _ core.Retriever = Fallback{}
