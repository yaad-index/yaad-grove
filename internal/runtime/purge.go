package runtime

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/namespaces"
)

// purgeDelay is how long after a window boundary the purge runs, so a clock
// a little behind still finds itself in the new window.
const purgeDelay = time.Minute

// Purger deletes what retention expired from one namespace.
type Purger interface {
	Purge(ctx context.Context) error
}

// RunPurge runs every one of ps at once and then shortly after every window
// boundary, until ctx ends (ADR 0023 §5). A failed purge is logged and tried
// again at the next boundary, and does not stop the others; what it missed is
// still never read, since reads skip records older than retention.
func RunPurge(ctx context.Context, ps []Purger, window time.Duration, now func() time.Time, after func(time.Duration) <-chan time.Time) {
	if now == nil {
		now = time.Now
	}
	if after == nil {
		after = time.After
	}
	for {
		for _, p := range ps {
			if err := p.Purge(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("long-term memory purge failed; retrying at the next window", "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-after(untilPurge(now(), window)):
		}
	}
}

// untilPurge is how long from t until the next purge, purgeDelay after a
// window boundary: the one just passed when t is still within purgeDelay of it,
// else the next.
func untilPurge(t time.Time, window time.Duration) time.Duration {
	return core.WindowStart(t.Add(-purgeDelay), window).Add(window + purgeDelay).Sub(t)
}

// PurgeCut is where a purge at now cuts: retention before the start of the
// current window. With retention a whole number of windows the cut is a window
// boundary, so every session lies wholly on one side of it and is deleted
// whole, never rewritten, whenever the purge runs. A record is deleted at most
// one window after it expires, and is never read once it has.
func PurgeCut(now time.Time, window, retention time.Duration) time.Time {
	return core.WindowStart(now, window).Add(-retention)
}

// Deleter deletes a namespace's records older than a cut; a bonyan memory
// backend is one.
type Deleter interface {
	DeleteBefore(ctx context.Context, t time.Time) error
}

// NamespacePurge purges one namespace's long-term memory at PurgeCut, and
// keeps the record of namespaces current (ADR 0023 §5). A namespace dropped
// from the configuration is still purged, and forgotten once a purge's cut
// has passed the moment it was dropped, since nothing was kept in it after.
type NamespacePurge struct {
	Namespace string
	Backend   Deleter
	Record    *namespaces.Record
	Retention time.Duration
	Window    time.Duration
	// Dropped is when the namespace was first found missing from the
	// configuration; the zero time is a configured one.
	Dropped time.Time
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Purge purges p's namespace and updates the record.
func (p NamespacePurge) Purge(ctx context.Context) error {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	cut := PurgeCut(now(), p.Window, p.Retention)
	if err := p.Backend.DeleteBefore(ctx, cut); err != nil {
		return fmt.Errorf("namespace %q: %w", p.Namespace, err)
	}
	if p.Dropped.IsZero() || cut.Before(p.Dropped) {
		return nil
	}
	if err := p.Record.Forget(p.Namespace); err != nil {
		return fmt.Errorf("namespace %q: %w", p.Namespace, err)
	}
	slog.Info("long-term memory namespace forgotten: dropped from the configuration and fully purged", "namespace", p.Namespace)
	return nil
}
