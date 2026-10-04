package runtime

import (
	"context"
	"errors"
	"log/slog"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/namespaces"
)

// SubjectDeleter deletes every record of a subject in one namespace; a bonyan
// memory store is one.
type SubjectDeleter interface {
	DeleteSubject(ctx context.Context, subject string) error
}

// EraseResult is how erasing a user went in one namespace.
type EraseResult struct {
	Namespace string
	Err       error
}

// Erased reports whether every namespace in results was erased.
func Erased(results []EraseResult) bool {
	for _, r := range results {
		if r.Err != nil {
			return false
		}
	}
	return true
}

// MemoryEraser erases a user's long-term memory; *Eraser is one.
type MemoryEraser interface {
	Erase(ctx context.Context, user string) []EraseResult
}

// Eraser erases a user's long-term memory in every namespace the instance has
// kept memory in, configured or dropped (ADR 0023 §5).
type Eraser struct {
	// Memory is the engine's long-term memory, stopped from keeping the user's
	// turns already under way before anything is erased. Nil is none.
	Memory *core.Memory
	// Record names the namespaces to erase in, read at each erase.
	Record *namespaces.Record
	// Stores holds every recorded namespace's store.
	Stores map[string]SubjectDeleter
}

// errNoStore is a recorded namespace with no store to erase through.
var errNoStore = errors.New("no store is open for this namespace")

// Erase stops the engine keeping any of user's turns already under way, then
// deletes user in each recorded namespace, trying every namespace even when
// another fails, and returns how each went, in namespace order. A failure is
// logged, never hidden: the caller tells the user it is not done.
func (e *Eraser) Erase(ctx context.Context, user string) []EraseResult {
	e.Memory.Withdraw(user)
	var results []EraseResult
	for _, ns := range e.Record.Namespaces() {
		r := EraseResult{Namespace: ns}
		if s := e.Stores[ns]; s == nil {
			r.Err = errNoStore
		} else {
			r.Err = s.DeleteSubject(ctx, user)
		}
		if r.Err != nil {
			slog.Warn("long-term memory: erasing a withdrawn user failed", "user", user, "namespace", ns, "err", r.Err)
		}
		results = append(results, r)
	}
	return results
}
