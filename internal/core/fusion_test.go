package core_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/yaad-index/yaad-grove/internal/core"
)

// Fusing ranked lists (#250): a chunk both return outranks one only a list
// returns, the earlier list wins a tie, each list keeps its own order, and k
// caps the result.
func TestFuseRanked(t *testing.T) {
	a, b, c, d := core.Chunk{Source: "a.md", Text: "a"}, core.Chunk{Source: "b.md", Text: "b"}, core.Chunk{Source: "c.md", Text: "c"}, core.Chunk{Source: "d.md", Text: "d"}
	assert.Equal(t, []core.Chunk{b, a, c}, core.FuseRanked([][]core.Chunk{{a, b}, {c, b}}, 0), "shared chunk first, then the earlier list's own top")
	assert.Equal(t, []core.Chunk{a, c, b, d}, core.FuseRanked([][]core.Chunk{{a, b}, {c, d}}, 0), "ties go to the earlier list")
	assert.Equal(t, []core.Chunk{c, d}, core.FuseRanked([][]core.Chunk{nil, {c, d}}, 0), "an empty first list leaves the second as it was")
	assert.Equal(t, []core.Chunk{a, c}, core.FuseRanked([][]core.Chunk{{a, b}, {c, d}}, 2), "k caps the fused list")
	assert.Empty(t, core.FuseRanked([][]core.Chunk{nil, nil}, 0))
}
