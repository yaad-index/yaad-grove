package budget

import (
	"encoding/json"
	"fmt"
	"time"

	"go.etcd.io/bbolt"
)

var (
	bucketName = []byte("budget")
	stateKey   = []byte("state")
)

// BoltStore persists the meter State in a bbolt file — the house store (ADR
// 0005), so the budget survives a restart or crash-loop (the failure the ceiling
// most needs to outlast, ADR 0006).
type BoltStore struct {
	db *bbolt.DB
}

// OpenBolt opens (creating if needed) the budget store at path.
func OpenBolt(path string) (*BoltStore, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("budget: open %s: %w", path, err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(bucketName)
		return e
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("budget: init bucket: %w", err)
	}
	return &BoltStore{db: db}, nil
}

// Close releases the underlying database.
func (s *BoltStore) Close() error { return s.db.Close() }

// Load reads the persisted state; found is false on first run.
func (s *BoltStore) Load() (State, bool, error) {
	var st State
	found := false
	err := s.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucketName).Get(stateKey)
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &st)
	})
	if err != nil {
		return State{}, false, fmt.Errorf("budget: load: %w", err)
	}
	return st, found, nil
}

// Save writes the state.
func (s *BoltStore) Save(st State) error {
	b, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("budget: marshal state: %w", err)
	}
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketName).Put(stateKey, b)
	}); err != nil {
		return fmt.Errorf("budget: save: %w", err)
	}
	return nil
}
