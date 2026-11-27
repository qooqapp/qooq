// Package bbolt implements core.Store on top of go.etcd.io/bbolt (pure-Go KV,
// single file, no cgo). Buckets: components, groups, events, reservations. Events are stored
// in a per-component nested bucket, append-only with a per-component cap.
package bbolt

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/qooqapp/qooq/internal/core"
)

var (
	bucketComponents = []byte("components")
	bucketGroups     = []byte("groups")
	bucketEvents     = []byte("events")
	bucketReserved   = []byte("reservations")
)

// maxEventsPerComponent caps the append-only per-component event log.
const maxEventsPerComponent = 100

// Store is the bbolt-backed core.Store.
type Store struct {
	db *bolt.DB
}

var _ core.Store = (*Store)(nil)

// Open opens (creating if needed) the bbolt file and ensures top-level buckets.
// The parent directory is created if missing so `-data /some/new/dir/q.db`
// works without the operator pre-creating the path (bolt.Open only creates the
// file itself, not its parent).
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir %s: %w", dir, err)
		}
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 3 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bbolt %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketComponents, bucketGroups, bucketEvents, bucketReserved} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("init buckets: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Backup writes a consistent snapshot of the whole database to w. bbolt's
// Tx.WriteTo runs inside a read transaction, so it is safe to call on a live
// daemon: it captures a point-in-time view without blocking concurrent reads,
// and the result is a valid bbolt file that can be opened directly or restored
// by copying it back over the data path while the daemon is stopped.
func (s *Store) Backup(w io.Writer, sizeHint func(int64)) error {
	return s.db.View(func(tx *bolt.Tx) error {
		if sizeHint != nil {
			sizeHint(tx.Size())
		}
		_, err := tx.WriteTo(w)
		return err
	})
}

func (s *Store) SaveComponent(c *core.Component) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketComponents).Put([]byte(c.ID), data)
	})
}

func (s *Store) GetComponent(id string) (*core.Component, error) {
	var c core.Component
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketComponents).Get([]byte(id))
		if v == nil {
			return core.ErrNotFound
		}
		return json.Unmarshal(v, &c)
	})
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) ListComponents() ([]*core.Component, error) {
	var out []*core.Component
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketComponents).ForEach(func(_, v []byte) error {
			var c core.Component
			if err := json.Unmarshal(v, &c); err != nil {
				return err
			}
			out = append(out, &c)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) DeleteComponent(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketComponents).Delete([]byte(id)); err != nil {
			return err
		}
		root := tx.Bucket(bucketEvents)
		if root.Bucket([]byte(id)) != nil {
			return root.DeleteBucket([]byte(id))
		}
		return nil
	})
}

func (s *Store) SaveGroup(g *core.Group) error {
	data, err := json.Marshal(g)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketGroups).Put([]byte(g.ID), data)
	})
}

func (s *Store) GetGroup(id string) (*core.Group, error) {
	var g core.Group
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket(bucketGroups).Get([]byte(id))
		if v == nil {
			return core.ErrNotFound
		}
		return json.Unmarshal(v, &g)
	})
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func (s *Store) ListGroups() ([]*core.Group, error) {
	var out []*core.Group
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketGroups).ForEach(func(_, v []byte) error {
			var g core.Group
			if err := json.Unmarshal(v, &g); err != nil {
				return err
			}
			out = append(out, &g)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) DeleteGroup(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketGroups).Delete([]byte(id))
	})
}

func reservationKey(namespace, name string) []byte { return []byte(namespace + "/" + name) }

func (s *Store) SaveReservation(r *core.Reservation) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketReserved).Put(reservationKey(r.Spec.Namespace, r.Spec.Name), data)
	})
}

func (s *Store) ListReservations() ([]*core.Reservation, error) {
	var out []*core.Reservation
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketReserved).ForEach(func(_, v []byte) error {
			var r core.Reservation
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			out = append(out, &r)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) DeleteReservation(namespace, name string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketReserved).Delete(reservationKey(namespace, name))
	})
}

func (s *Store) AppendEvent(componentID string, e core.Event) error {
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.Bucket(bucketEvents).CreateBucketIfNotExists([]byte(componentID))
		if err != nil {
			return err
		}
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		key := make([]byte, 8)
		binary.BigEndian.PutUint64(key, seq)
		if err := b.Put(key, data); err != nil {
			return err
		}
		// Truncate: drop keys older than the newest maxEventsPerComponent.
		// The +1 is what makes the cap exact — sequences are 1-based, so
		// keeping everything >= seq-max leaves max+1 entries.
		if seq > maxEventsPerComponent {
			lowKey := make([]byte, 8)
			binary.BigEndian.PutUint64(lowKey, seq-maxEventsPerComponent+1)
			var stale [][]byte
			c := b.Cursor()
			for k, _ := c.First(); k != nil && bytes.Compare(k, lowKey) < 0; k, _ = c.Next() {
				kk := make([]byte, len(k))
				copy(kk, k)
				stale = append(stale, kk)
			}
			for _, k := range stale {
				if err := b.Delete(k); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Store) ListEvents(componentID string) ([]core.Event, error) {
	var out []core.Event
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketEvents).Bucket([]byte(componentID))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var e core.Event
			if err := json.Unmarshal(v, &e); err != nil {
				return err
			}
			out = append(out, e)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
