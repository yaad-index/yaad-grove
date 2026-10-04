// Package namespaces keeps the durable record of every long-term memory
// namespace the engine has kept memory in (ADR 0023 §5), so withdrawal and the
// retention purge still reach a namespace after it is dropped from the
// configuration.
package namespaces

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Record is the record, kept in one file. Every method is safe for
// concurrent use.
type Record struct {
	path string

	mu sync.Mutex
	// seen maps each recorded namespace to the last time it was known to be
	// configured, so possibly written to.
	seen map[string]time.Time
}

// file is the record's form on disk.
type file struct {
	Namespaces map[string]time.Time `json:"namespaces"`
}

// Open reads the record at path; a missing file is an empty record.
func Open(path string) (*Record, error) {
	r := &Record{path: path, seen: map[string]time.Time{}}
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return r, nil
	case err != nil:
		return nil, fmt.Errorf("namespaces: read %s: %w", path, err)
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("namespaces: parse %s: %w", path, err)
	}
	for ns, t := range f.Namespaces {
		r.seen[ns] = t
	}
	return r, nil
}

// Seen records that each of namespaces is configured at now, and saves the
// record before returning, so a namespace is on disk before anything is kept
// in it.
func (r *Record) Seen(now time.Time, namespaces ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ns := range namespaces {
		r.seen[ns] = now
	}
	return r.save()
}

// Namespaces are the recorded namespaces, sorted.
func (r *Record) Namespaces() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.seen))
	for ns := range r.seen {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// Forget drops ns from the record when everything it can hold has expired by
// now: it was last configured, so last written to, longer than keep ago. It
// reports whether ns was dropped. The caller forgets a namespace only after a
// purge in it succeeded at now.
func (r *Record) Forget(ns string, now time.Time, keep time.Duration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	last, ok := r.seen[ns]
	if !ok || now.Sub(last) < keep {
		return false, nil
	}
	delete(r.seen, ns)
	if err := r.save(); err != nil {
		r.seen[ns] = last
		return false, err
	}
	return true, nil
}

// save writes the record to a new file beside it and renames it into place,
// so a crash leaves the old record or the new one, never part of either.
func (r *Record) save() error {
	b, err := json.MarshalIndent(file{Namespaces: r.seen}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(r.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(r.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("namespaces: save: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("namespaces: save: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("namespaces: save: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("namespaces: save: %w", err)
	}
	if err := os.Rename(tmp.Name(), r.path); err != nil {
		return fmt.Errorf("namespaces: save: %w", err)
	}
	return nil
}
