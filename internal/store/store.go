// Package store keeps what go-fs has to remember across a restart and that is
// no part of the configuration: share links that run out, how far a user got
// into a song, and the like. It is one file, given by -persistent-data and by
// default beside the configuration file, held open by bbolt for as long as
// go-fs runs; a second go-fs pointed at the same file is refused rather than
// let in to corrupt it.
//
// Every part keeps its records in a bucket of its own, named after it, so
// that two parts never meet on a key. A record is a value encoded as JSON,
// stored under a string key, and may be given a time it runs out at: from
// then on it is not found, and the sweep that runs every hour removes it.
package store

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// FileExtension is what the default file is named with in place of the
// configuration file's own extension.
const FileExtension = ".db"

// lockTimeout is how long Open waits for a file another process holds before
// it gives up.
const lockTimeout = time.Second

// sweepInterval is how often the records that ran out are removed.
const sweepInterval = time.Hour

// headerSize is the expiry in front of every value: Unix nanoseconds, big
// endian, zero for a record that never runs out.
const headerSize = 8

// DefaultPath is the file beside the configuration file, named like it:
// /etc/go-fs/go-fs.toml keeps its data in /etc/go-fs/go-fs.db.
func DefaultPath(configPath string) string {
	return strings.TrimSuffix(configPath, filepath.Ext(configPath)) + FileExtension
}

// Store is the open file.
type Store struct {
	db   *bolt.DB
	path string
	log  *slog.Logger
	// now is the clock records are checked against.
	now func() time.Time

	stop      chan struct{}
	swept     chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// Open opens the file at path, creating it, and the folder it is in, when
// there is none yet. It fails when another process has the file open.
func Open(path string, logger *slog.Logger) (*Store, error) {
	return open(path, logger, time.Now, sweepInterval)
}

// open is Open with the clock and the interval of the sweep given, which the
// tests set.
func open(path string, logger *slog.Logger, now func() time.Time, interval time.Duration) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: lockTimeout})
	if errors.Is(err, bolt.ErrTimeout) {
		return nil, fmt.Errorf("the persistent data file %s is in use by another go-fs", path)
	}
	if err != nil {
		return nil, fmt.Errorf("cannot open the persistent data file %s: %w", path, err)
	}
	s := &Store{
		db:    db,
		path:  path,
		log:   logger,
		now:   now,
		stop:  make(chan struct{}),
		swept: make(chan struct{}),
	}
	go s.sweepEvery(interval)
	return s, nil
}

// Path is the file the store keeps its data in.
func (s *Store) Path() string { return s.path }

// Close stops the sweep and closes the file. Records written before it
// returns are on disk.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.swept
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

// sweepEvery removes the records that ran out, once right away, for those
// that did while go-fs was not running, and then every interval until Close.
func (s *Store) sweepEvery(interval time.Duration) {
	defer close(s.swept)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if removed, err := s.sweep(); err != nil {
			s.log.Warn("cannot remove the expired records of the persistent data file",
				"path", s.path, "error", err)
		} else if removed > 0 {
			s.log.Debug("removed expired records from the persistent data file",
				"path", s.path, "removed", removed)
		}
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		}
	}
}

// sweep removes every record that ran out, in every bucket, and says how
// many there were.
func (s *Store) sweep() (int, error) {
	now := s.now()
	removed := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.ForEach(func(_ []byte, bucket *bolt.Bucket) error {
			// a cursor that deletes can skip the record after, so the keys
			// are collected first and deleted after
			var expired [][]byte
			err := bucket.ForEach(func(key, value []byte) error {
				if isExpired(value, now) {
					expired = append(expired, bytes.Clone(key))
				}
				return nil
			})
			if err != nil {
				return err
			}
			for _, key := range expired {
				if err := bucket.Delete(key); err != nil {
					return err
				}
			}
			removed += len(expired)
			return nil
		})
	})
	return removed, err
}

// Bucket is the records of one part of go-fs. It is created with its first
// record; until then, reading from it finds nothing.
type Bucket struct {
	store *Store
	name  []byte
}

// Bucket is the bucket of the given name, "shares", "jellyfin.playstate" and
// the like: one per kind of record, named after the part that keeps it.
func (s *Store) Bucket(name string) *Bucket {
	return &Bucket{store: s, name: []byte(name)}
}

// Get decodes the record at key into v and reports whether there was one. A
// record that ran out is not found.
func (b *Bucket) Get(key string, v any) (bool, error) {
	now := b.store.now()
	found := false
	err := b.store.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(b.name)
		if bucket == nil {
			return nil
		}
		value := bucket.Get([]byte(key))
		if value == nil || isExpired(value, now) {
			return nil
		}
		if len(value) < headerSize {
			return b.corrupt(key)
		}
		found = true
		return json.Unmarshal(value[headerSize:], v)
	})
	return found, err
}

// Put stores v at key, for good, replacing what was there.
func (b *Bucket) Put(key string, v any) error {
	return b.put(key, v, time.Time{})
}

// PutUntil stores v at key until expires, replacing what was there. A zero
// expires is for good, as Put.
func (b *Bucket) PutUntil(key string, v any, expires time.Time) error {
	return b.put(key, v, expires)
}

func (b *Bucket) put(key string, v any, expires time.Time) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	value := make([]byte, headerSize+len(payload))
	if !expires.IsZero() {
		binary.BigEndian.PutUint64(value, uint64(expires.UnixNano()))
	}
	copy(value[headerSize:], payload)
	// a batch takes the writes that arrive together into one commit, so
	// that many small ones do not each wait for the disk; it may run the
	// function more than once, which a put survives
	return b.store.db.Batch(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(b.name)
		if err != nil {
			return err
		}
		return bucket.Put([]byte(key), value)
	})
}

// Delete removes the record at key. One that is not there is no error.
func (b *Bucket) Delete(key string) error {
	return b.store.db.Batch(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(b.name)
		if bucket == nil {
			return nil
		}
		return bucket.Delete([]byte(key))
	})
}

// Scan calls fn for every record whose key starts with prefix, in the order
// of the keys, leaving out those that ran out; an empty prefix is every
// record. decode decodes the record into its argument. The records are read
// before the first call, so fn may write to the store; an error it returns
// ends the scan and is returned.
func (b *Bucket) Scan(prefix string, fn func(key string, decode func(v any) error) error) error {
	type record struct {
		key     string
		payload []byte
	}
	now := b.store.now()
	var records []record
	err := b.store.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(b.name)
		if bucket == nil {
			return nil
		}
		start := []byte(prefix)
		cursor := bucket.Cursor()
		for key, value := cursor.Seek(start); key != nil && bytes.HasPrefix(key, start); key, value = cursor.Next() {
			if isExpired(value, now) {
				continue
			}
			if len(value) < headerSize {
				return b.corrupt(string(key))
			}
			records = append(records, record{string(key), bytes.Clone(value[headerSize:])})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, r := range records {
		payload := r.payload
		if err := fn(r.key, func(v any) error { return json.Unmarshal(payload, v) }); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bucket) corrupt(key string) error {
	return fmt.Errorf("the record %q in the bucket %q of %s is too short to be one",
		key, b.name, b.store.path)
}

// isExpired reports whether the record value ran out by now.
func isExpired(value []byte, now time.Time) bool {
	if len(value) < headerSize {
		return false
	}
	expires := binary.BigEndian.Uint64(value)
	return expires != 0 && now.UnixNano() >= int64(expires)
}
