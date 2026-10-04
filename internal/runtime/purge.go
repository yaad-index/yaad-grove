package runtime

import (
	"context"
	"log/slog"
	"time"

	"github.com/yaad-index/yaad-grove/internal/core"
	"github.com/yaad-index/yaad-grove/internal/namespaces"
)

// purgeDelay is how long after a window boundary the purge runs, so a clock
// a little behind still cuts in the new window.
const purgeDelay = time.Minute

// Purger deletes what retention expired; *memory.Store satisfies it.
type Purger interface {
	Purge(ctx context.Context) error
}

// RunPurge purges every one of ps, each a namespace's long-term memory, shortly
// after every window boundary until ctx ends (ADR 0023 §5). With retention a whole number of windows, the cut
// (now minus retention) then falls on a boundary too, so a session lies wholly
// on one side of it and is deleted whole rather than rewritten. A failed purge
// is logged and tried again at the next boundary, and does not stop the others;
// what it missed is still never read, since reads skip records older than
// retention.
func RunPurge(ctx context.Context, ps []Purger, window time.Duration, now func() time.Time, after func(time.Duration) <-chan time.Time) {
	if now == nil {
		now = time.Now
	}
	if after == nil {
		after = time.After
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-after(untilPurge(now(), window)):
		}
		for i, p := range ps {
			if err := p.Purge(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				slog.Warn("long-term memory purge failed; retrying at the next window", "store", i, "err", err)
			}
		}
	}
}

// untilPurge is how long from t until the next purge, purgeDelay after a
// window boundary: the one just passed when t is still within purgeDelay of it,
// else the next.
func untilPurge(t time.Time, window time.Duration) time.Duration {
	return core.WindowStart(t.Add(-purgeDelay), window).Add(window + purgeDelay).Sub(t)
}

// NamespacePurge purges one namespace's long-term memory and keeps the record
// of namespaces current (ADR 0023 §5). A configured namespace is recorded as
// seen at every purge, whether the purge succeeds or not, since it may be
// written to until the next. One dropped from the configuration is still
// purged, and forgotten once a purge in it succeeds after everything it can
// hold has expired.
type NamespacePurge struct {
	Namespace  string
	Store      Purger
	Record     *namespaces.Record
	Configured bool
	// Keep is how long after a namespace was last configured it can still hold
	// a record: the retention period and one window.
	Keep time.Duration
	// Now is the clock; nil is time.Now.
	Now func() time.Time
}

// Purge purges p's namespace and updates the record.
func (p NamespacePurge) Purge(ctx context.Context) error {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	if p.Configured {
		if err := p.Record.Seen(now(), p.Namespace); err != nil {
			return err
		}
	}
	if err := p.Store.Purge(ctx); err != nil {
		return err
	}
	if p.Configured {
		return nil
	}
	forgot, err := p.Record.Forget(p.Namespace, now(), p.Keep)
	if forgot {
		slog.Info("long-term memory namespace forgotten: dropped from the configuration and fully expired", "namespace", p.Namespace)
	}
	return err
}
