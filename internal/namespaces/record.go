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
	// dropped maps each recorded namespace to when it was first found missing
	// from the configuration; the zero time is a configured namespace.
	dropped map[string]time.Time
}

// file is the record's form on disk.
type file struct {
	Namespaces map[string]entry `json:"namespaces"`
}

type entry struct {
	// Dropped is when the namespace was first found missing from the
	// configuration; nil while it is configured.
	Dropped *time.Time `json:"dropped,omitempty"`
}

// Open reads the record at path; a missing file is an empty record.
func Open(path string) (*Record, error) {
	r := &Record{path: path, dropped: map[string]time.Time{}}
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
	for ns, e := range f.Namespaces {
		var t time.Time
		if e.Dropped != nil {
			t = *e.Dropped
		}
		r.dropped[ns] = t
	}
	return r, nil
}

// Configured records namespaces as configured, so possibly written to, and
// saves the record before returning, so a namespace is on disk before anything
// is kept in it. A namespace configured again is no longer dropped.
func (r *Record) Configured(namespaces ...string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ns := range namespaces {
		r.dropped[ns] = time.Time{}
	}
	return r.save()
}

// Dropped records that ns is missing from the configuration at now, unless it
// already was, and returns when it was first found missing. Nothing can be
// kept in a namespace once it is dropped, so everything it holds is older.
func (r *Record) Dropped(ns string, now time.Time) (time.Time, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t := r.dropped[ns]; !t.IsZero() {
		return t, nil
	}
	r.dropped[ns] = now
	if err := r.save(); err != nil {
		r.dropped[ns] = time.Time{}
		return time.Time{}, err
	}
	return now, nil
}

// Namespaces are the recorded namespaces, sorted.
func (r *Record) Namespaces() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.dropped))
	for ns := range r.dropped {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// Forget drops ns from the record. The caller forgets a dropped namespace only
// after a purge in it deleted everything kept before it was dropped.
func (r *Record) Forget(ns string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.dropped[ns]
	if !ok {
		return nil
	}
	delete(r.dropped, ns)
	if err := r.save(); err != nil {
		r.dropped[ns] = t
		return err
	}
	return nil
}

// save writes the record to a new file beside it, renames it into place and
// syncs the directory, so a crash leaves the old record or the new one, never
// part of either, and the new one survives a crash once save returns.
func (r *Record) save() error {
	f := file{Namespaces: make(map[string]entry, len(r.dropped))}
	for ns, t := range r.dropped {
		var e entry
		if !t.IsZero() {
			e.Dropped = &t
		}
		f.Namespaces[ns] = e
	}
	b, err := json.MarshalIndent(f, "", "  ")
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
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("namespaces: save: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("namespaces: save: %w", err)
	}
	return nil
}
