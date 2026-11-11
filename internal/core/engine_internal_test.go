package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Fusing ranked lists (#250): a chunk both return outranks one only a list
// returns, the earlier list wins a tie, and each list keeps its own order.
func TestFuseRanked(t *testing.T) {
	a, b, c, d := Chunk{"a.md", "a"}, Chunk{"b.md", "b"}, Chunk{"c.md", "c"}, Chunk{"d.md", "d"}
	assert.Equal(t, []Chunk{b, a, c}, fuseRanked([]Chunk{a, b}, []Chunk{c, b}), "shared chunk first, then the earlier list's own top")
	assert.Equal(t, []Chunk{a, c, b, d}, fuseRanked([]Chunk{a, b}, []Chunk{c, d}), "ties go to the earlier list")
	assert.Equal(t, []Chunk{c, d}, fuseRanked(nil, []Chunk{c, d}), "an empty first list leaves the second as it was")
	assert.Empty(t, fuseRanked(nil, nil))
}
