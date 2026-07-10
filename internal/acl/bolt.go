package acl

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

var aclBucket = []byte("acl")

// BoltStore persists per-user Records in a bbolt file — the house store (ADR
// 0005), matching the pattern in internal/budget. The store survives restarts so
// consent and tier assignments are durable.
type BoltStore struct {
	db *bbolt.DB
}

// OpenBolt opens (creating if needed) the ACL store at path.
func OpenBolt(path string) (*BoltStore, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("acl: open %s: %w", path, err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(aclBucket)
		return e
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("acl: init bucket: %w", err)
	}
	return &BoltStore{db: db}, nil
}

// Close releases the underlying database.
func (s *BoltStore) Close() error { return s.db.Close() }

// Get returns the user's Record. A first-seen user yields a zero Record (its
// UserID set, ConsentUnknown) — no consent implied — rather than an error.
func (s *BoltStore) Get(ctx context.Context, userID string) (Record, error) {
	rec := Record{UserID: userID}
	found := false
	err := s.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(aclBucket).Get([]byte(userID))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &rec)
	})
	if err != nil {
		return Record{}, fmt.Errorf("acl: get %q: %w", userID, err)
	}
	if !found {
		return Record{UserID: userID}, nil
	}
	return rec, nil
}

// Put writes the user's Record.
func (s *BoltStore) Put(ctx context.Context, r Record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("acl: marshal record: %w", err)
	}
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(aclBucket).Put([]byte(r.UserID), b)
	}); err != nil {
		return fmt.Errorf("acl: put %q: %w", r.UserID, err)
	}
	return nil
}

// Update applies mutate to the user's record within one bbolt write transaction:
// read-modify-write with no window for a concurrent writer to clobber.
func (s *BoltStore) Update(ctx context.Context, userID string, mutate func(*Record) error) error {
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(aclBucket)
		rec := Record{UserID: userID}
		if v := bkt.Get([]byte(userID)); v != nil {
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("acl: unmarshal record: %w", err)
			}
		}
		if err := mutate(&rec); err != nil {
			return err
		}
		data, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("acl: marshal record: %w", err)
		}
		return bkt.Put([]byte(userID), data)
	}); err != nil {
		return fmt.Errorf("acl: update %q: %w", userID, err)
	}
	return nil
}
