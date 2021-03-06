package diffdb

import (
	"bytes"
	"context"
	"github.com/boltdb/bolt"
	"github.com/hashicorp/go-multierror"
	"gopkg.in/vmihailenco/msgpack.v2"
	"os"
	"errors"
	"github.com/mitchellh/hashstructure"
	"encoding/binary"
)

var (
	// ErrConflictingKey indicates that MustNotConflict() was enabled and a conflicting ID was entered into the state database.
	ErrConflictingKey = errors.New("diffdb: multiple objects with the same ID were added in the same change version")
)

// An Object is a Go object passed to a differential database to track changes on.
// The object must be encodable by msgpack.
type Object interface {
	ID() []byte
}



func HashOf(x interface{}) ([]byte, error) {
	i, err := hashstructure.Hash(x, nil)
	if err != nil {
		return nil, err
	}
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, i)
	return b, nil
}

// New creates a new hashing database using the given filename
func New(path string) (*DB, error) {
	db, err := bolt.Open(path, os.FileMode(0600), nil)
	if err != nil {
		return nil, err
	}

	return &DB{
		db: db,
	}, nil
}

var (
	bucketHashes          = []byte("_m")
	bucketPendingHashes   = []byte("_ph")
	bucketPendingHashData = []byte("_pd")
	bucketUserData        = []byte("_ud")
	bucketKeyConflicts    = []byte("_dk")
)

// A DB is a wrapper around a BoltDB to open multiple differential buckets
type DB struct {
	db *bolt.DB
}

// Open opens a named differential or creates one if it does not exist.
func (db *DB) Open(name string) (*Differential, error) {
	q := []byte(name)
	err := db.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(q)
		if err != nil {
			return err
		}

		_, err = b.CreateBucketIfNotExists(bucketHashes)
		if err != nil {
			return err
		}
		_, err = b.CreateBucketIfNotExists(bucketPendingHashes)
		if err != nil {
			return err
		}
		_, err = b.CreateBucketIfNotExists(bucketPendingHashData)
		if err != nil {
			return err
		}
		_, err = b.CreateBucketIfNotExists(bucketUserData)
		if err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		return nil, err
	}

	return &Differential{
		q:  q,
		db: db.db,
	}, nil
}

// Delete deletes the named differential.
func (db *DB) Delete(name string) error {
	q := []byte(name)
	return db.db.Update(func(tx *bolt.Tx) error {
		return tx.DeleteBucket(q)
	})
}

// Close closes the database file.
func (db *DB) Close() error {
	return db.db.Close()
}

// A Differential tracks changes between serialised Go objects.
type Differential struct {
	q    []byte
	db   *bolt.DB
	cols []string

	trackConflicts bool
}

func (diff *Differential) Name() string {
	return string(diff.q)
}

// MustNotConflict sets a flag to track duplicate IDs given to subsequent calls to Add.
// This can be used as a debugging tool to check if additions in the same version
// have conflicting IDs.
// Calling MustNotConflict will delete any existing conflict information.
func (diff *Differential) MustNotConflict() error {
	return diff.db.Update(func(tx *bolt.Tx) error {
		tx.OnCommit(func() {
			diff.trackConflicts = true
		})

		b := tx.Bucket(diff.q)
		cb := b.Bucket(bucketKeyConflicts)
		if cb != nil {
			err := b.DeleteBucket(bucketKeyConflicts)
			if err != nil {
				return err
			}
		}

		_, err := b.CreateBucket(bucketKeyConflicts)
		return err
	})
}

// AddTx adds an object to start tracking by using an existing BoltDB transaction.
func (diff *Differential) AddTx(tx *bolt.Tx, obj Object) (bool, error) {
	b := tx.Bucket(diff.q)

	var (
		bh   = b.Bucket(bucketHashes)
		bph  = b.Bucket(bucketPendingHashes)
		bphd = b.Bucket(bucketPendingHashData)
	)

	id := obj.ID()

	// Check ID conflicts
	if diff.trackConflicts {
		bkc := b.Bucket(bucketKeyConflicts)
		if bkc.Get(id) != nil {
			return false, ErrConflictingKey
		}
	}

	hash, err := HashOf(obj)
	if err != nil {
		return false, err
	}

	var (
		existing = bh.Get(id)
		match    = bytes.Compare(existing, hash) == 0
	)

	// An existing committed hash is identical, no need for changes
	if match {
		return false, nil
	}

	// Check if pending hash already exists
	if pending := bph.Get(id); pending != nil {

		// Contents are identical to existing pending version, no need for changes
		if len(pending) > 0 && bytes.Compare(pending, hash) == 0 {
			return false, nil
		}

		if err := bphd.Delete(pending); err != nil {
			return false, err
		}
	}

	// Ensure this ID is ready to be tracked
	if err := bph.Put(id, hash); err != nil {
		return false, err
	}

	raw, err := msgpack.Marshal(obj)
	if err != nil {
		return false, err
	}
	if err := bphd.Put(hash, raw); err != nil {
		return false, err
	}

	if diff.trackConflicts {
		err := b.Bucket(bucketKeyConflicts).Put(id, nil)
		if err != nil {
			return false, err
		}
	}

	return true, nil
}

// AddChan adds objects sent from a channel until the channel is closed, the object is nil,  or the context is cancelled.
// AddChan may stop processing the stream if an error occurs in which case no more messages will be consumed
// and that error will be returned.
func (diff *Differential) AddChan(ctx context.Context, stream <-chan Object) error {
	tx, err := diff.db.Begin(true)
	if err != nil {
		return err
	}

	defer tx.Rollback()

	var obj Object
	var i int
	for ; ; i ++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case obj = <-stream:
			if obj == nil {
				return tx.Commit()
			}
		}

		updated, err := diff.AddTx(tx, obj)
		if err != nil {
			return err
		}
		if !updated {
			i --
			continue
		}
	}
}

// Add as a new object x to the list of pending changes.
// Changes to x are tracked through its given ID which uniquely identifies x across changes.
// For example, if x was an SQL row then ID would be the primary key of that row.
//
// If Add is called multiple times same ID before applying changes then
// only the latest change will be taken to be applied.
func (diff *Differential) Add(obj Object) (updated bool, err error) {
	err = diff.db.Update(func(tx *bolt.Tx) error {
		var e error
		updated, e = diff.AddTx(tx, obj)
		return e
	})
	return
}

// Changed returns true if the hash of x has changed for its ID.
func (diff *Differential) Changed(id []byte, x interface{}) (changed bool, err error) {
	var hash []byte
	hash, err = HashOf(x)
	if err != nil {
		return
	}

	err = diff.db.View(func(tx *bolt.Tx) error {
		var compare = tx.Bucket(diff.q).Bucket(bucketHashes).Get(id)
		changed = bytes.Compare(compare, hash) != 0
		return nil
	})
	return
}

// CountTracking counts the number of entries in the hash tracking table.
// In other words, this is the amount of all items tracked by the differential db.
func (diff *Differential) CountTracking() (count int) {
	diff.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(diff.q)
		count = b.Bucket(bucketHashes).Stats().KeyN
		return nil
	})

	return
}

// CountChanges returns the number of items in the change pending bucket.
func (diff *Differential) CountChanges() (pending int) {
	diff.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(diff.q)
		pending = b.Bucket(bucketPendingHashes).Stats().KeyN
		return nil
	})

	return
}

// ApplyFunc is a function to be called to apply each pending change
type ApplyFunc func(id []byte, data Decoder) error

// EachN scans through each change until N items have been processed.
// If n is <= 0 then all pending changes will be applied.
func (diff *Differential) EachN(ctx context.Context, f ApplyFunc, n int) error {
	tx, err := diff.db.Begin(true)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	b := tx.Bucket(diff.q)
	var (
		bh   = b.Bucket(bucketHashes)
		bph  = b.Bucket(bucketPendingHashes)
		bphd = b.Bucket(bucketPendingHashData)

		decoder = new(msgpackDecoder)
		cur     = bph.Cursor()
	)

	var updateErr *multierror.Error
	var i int

scan:
	for id, hash := cur.First(); id != nil; id, hash = cur.Next() {
		select {
		case <-ctx.Done():
			updateErr = multierror.Append(updateErr, ctx.Err())
			break scan
		default:
		}

		var data = bphd.Get(hash)
		if data == nil {
			panic("missing hash data")
		}

		decoder.data = data
		if err := f(id, decoder); err != nil {
			updateErr = multierror.Append(updateErr, err)
			continue
		}

		if err := bh.Put(id, hash); err != nil {
			return err
		}
		if err := bph.Delete(id); err != nil {
			return err
		}
		if err := bphd.Delete(hash); err != nil {
			return err
		}
		i ++
		if n > 0 && n == i {
			break scan
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	return updateErr.ErrorOrNil()
}

// Each scans through each change and attempts to apply f() to each item waiting to be changed
func (diff *Differential) Each(ctx context.Context, f ApplyFunc) error {
	return diff.EachN(ctx, f, -1)
}

// ViewUserData wraps a BoltDB view transaction to allow custom user data to be viewed in the differential database.
// This could include information such as run times, last exported differential, etc.
func (diff *Differential) ViewUserData(f func(b *bolt.Bucket) error) error {
	return diff.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(diff.q).Bucket(bucketUserData)
		return f(b)
	})
}

// UpdateUserData wraps a BoltDB update transaction to allow custom user data to viewed or updated
// in the differential database.
func (diff *Differential) UpdateUserData(f func(b *bolt.Bucket) error) error {
	return diff.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(diff.q).Bucket(bucketUserData)
		return f(b)
	})
}
