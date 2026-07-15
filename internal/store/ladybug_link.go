//go:build ladybug

package store

// The ladybug backend links the embedded LadybugDB C library (ADR 0019). The
// shared library + headers are downloaded into <repo>/lib-ladybug by the
// go:generate directive in ladybug.go (kept out of the tree, .gitignored). Built
// with `-tags "ladybug system_ladybug"`: the `ladybug` tag selects this backend,
// and `system_ladybug` puts the go-ladybug binding in system-library mode (it adds
// `-llbug`); this file supplies the include/lib paths + an rpath so the runtime
// finds liblbug.so. cgo is isolated to this tag — the default build stays pure-Go,
// CGO_ENABLED=0, static/distroless.

// #cgo CFLAGS: -I${SRCDIR}/../../lib-ladybug
// #cgo LDFLAGS: -L${SRCDIR}/../../lib-ladybug -lstdc++ -lm -Wl,-rpath,${SRCDIR}/../../lib-ladybug
import "C"
