//go:build !ladybug

package store

import (
	"errors"

	"github.com/yaad-index/yaad-grove/internal/embed"
)

// ErrLadybugUnavailable is returned when the ladybug backend is selected in a build
// that did not compile it in. The default build is pure-Go (CGO_ENABLED=0, static
// distroless, ADR 0019); the ladybug backend compiles only under `-tags ladybug`,
// so `--retrieval-store ladybug` on a default build fails loudly here.
var ErrLadybugUnavailable = errors.New("store: this build was compiled without ladybug support (rebuild with -tags ladybug)")

// NewLadybug is the no-cgo placeholder for the default build — it always errors.
// The real implementation lives in ladybug.go under the `ladybug` build tag.
func NewLadybug(string, embed.Embedder, float32) (Store, error) {
	return nil, ErrLadybugUnavailable
}
