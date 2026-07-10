package pending

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"

	"github.com/yaad-index/yaad-grove/internal/core"
)

var pendingBucket = []byte("pending_actions")

// BoltStore persists pending actions in a bbolt file — the house store (ADR
// 0005), matching internal/budget and internal/acl. Buttons survive a restart,
// so a click after a redeploy still resolves (or reports expired) rather than
// silently doing nothing.
//
// The store owns its own garbage collection: OpenBolt starts a background
// sweeper and Close stops it, so the storage bound is live the moment the store
// exists — no coordination with the serve loop, nothing to forget to wire.
type BoltStore struct {
	db     *bbolt.DB
	ttl    time.Duration
	now    func() time.Time // injectable clock; time.Now in production
	cancel context.CancelFunc
	done   chan struct{}
}

// OpenBolt opens (creating if needed) the pending-action store at path, with a
// per-token lifetime of ttl, and starts a background sweeper that runs every
// sweepInterval. A non-positive sweepInterval disables the sweeper (tests drive
// Sweep directly).
func OpenBolt(path string, ttl, sweepInterval time.Duration) (*BoltStore, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("pending: open %s: %w", path, err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(pendingBucket)
		return e
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pending: init bucket: %w", err)
	}
	s := &BoltStore{db: db, ttl: ttl, now: time.Now, cancel: func() {}, done: make(chan struct{})}
	if sweepInterval > 0 {
		ctx, cancel := context.WithCancel(context.Background())
		s.cancel = cancel
		go func() {
			defer close(s.done)
			RunSweeper(ctx, s, sweepInterval)
		}()
	} else {
		// Disabled: start no ticker and no goroutine. Pre-close done so Close does
		// not block waiting for a sweeper that was never launched.
		close(s.done)
	}
	return s, nil
}

// Close stops the sweeper (joining its goroutine, so no ticker leaks past Close)
// and releases the underlying database.
func (s *BoltStore) Close() error {
	s.cancel()
	<-s.done
	return s.db.Close()
}

// Put stores action under a fresh token expiring ttl from now.
func (s *BoltStore) Put(_ context.Context, action core.Action) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(record{Action: action, ExpiresAt: s.now().Add(s.ttl)})
	if err != nil {
		return "", fmt.Errorf("pending: marshal record: %w", err)
	}
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(pendingBucket).Put([]byte(token), data)
	}); err != nil {
		return "", fmt.Errorf("pending: put token: %w", err)
	}
	return token, nil
}

// Resolve applies the expiry/single-use rules and tombstones a fresh token, all
// within one write transaction so a concurrent double-click can't both resolve.
func (s *BoltStore) Resolve(_ context.Context, token string) (core.Action, Status, error) {
	var action core.Action
	var status Status
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(pendingBucket)
		v := b.Get([]byte(token))
		if v == nil {
			status = Expired
			return nil
		}
		var rec record
		if err := json.Unmarshal(v, &rec); err != nil {
			return fmt.Errorf("pending: unmarshal record: %w", err)
		}
		a, st, updated := resolveRecord(rec, s.now())
		action, status = a, st
		if updated == nil {
			return nil
		}
		data, err := json.Marshal(*updated)
		if err != nil {
			return fmt.Errorf("pending: marshal tombstone: %w", err)
		}
		return b.Put([]byte(token), data)
	})
	if err != nil {
		return core.Action{}, Expired, fmt.Errorf("pending: resolve token: %w", err)
	}
	return action, status, nil
}

// Sweep drops every record past its TTL. Keys are collected first, then deleted,
// since bbolt forbids mutating a bucket mid-cursor.
func (s *BoltStore) Sweep(_ context.Context) (int, error) {
	now := s.now()
	n := 0
	err := s.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(pendingBucket)
		var stale [][]byte
		if err := b.ForEach(func(k, v []byte) error {
			var rec record
			if err := json.Unmarshal(v, &rec); err != nil {
				// A record we can't read is unusable — sweep it too.
				stale = append(stale, append([]byte(nil), k...))
				return nil
			}
			if now.After(rec.ExpiresAt) {
				stale = append(stale, append([]byte(nil), k...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, k := range stale {
			if err := b.Delete(k); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("pending: sweep: %w", err)
	}
	return n, nil
}

var _ Store = (*BoltStore)(nil)
