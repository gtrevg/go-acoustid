// Copyright (C) 2016  Lukas Lalinsky
// Distributed under the MIT license, see the LICENSE file for details.

package index

import (
	"github.com/acoustid/go-acoustid/util/vfs"
	"github.com/pkg/errors"
	"go4.org/syncutil"
	"io"
	"io/ioutil"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

var debugLog = log.New(ioutil.Discard, "", log.LstdFlags)

var ErrAlreadyClosed = errors.New("already closed")

// Options represents the options that can be set when opening a database.
type Options struct {
	// When enabled, the database will automatically run compactions in the background.
	EnableAutoCompact bool

	// How often to run automatic compactions. Only used if EnableAutoCompact is true.
	AutoCompactInterval time.Duration
}

// DefaultOptions represent the options used if nil options are passed into Open().
var DefaultOptions = &Options{
	AutoCompactInterval: time.Second * 10,
}

type DB struct {
	fs              vfs.FileSystem
	mu              sync.RWMutex
	wlock           io.Closer
	txid            uint32
	manifest        atomic.Value
	closed          bool
	closing         chan struct{}
	numSnapshots    int64
	numTransactions int64
	refs            map[string]int
	orphanedFiles   chan string
	mergeRequests   chan chan error
	mergePolicy     MergePolicy
	bg              syncutil.Group
	opts            *Options
}

func Open(fs vfs.FileSystem, create bool, opts *Options) (*DB, error) {
	if opts == nil {
		opts = DefaultOptions
	}

	var manifest Manifest
	err := manifest.Load(fs, create)
	if err != nil {
		return nil, errors.Wrap(err, "failed to open the manifest")
	}

	for _, segment := range manifest.Segments {
		err = segment.Open(fs)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to open segment %v", segment.ID)
		}
	}

	db := &DB{fs: fs, opts: opts}
	db.init(&manifest)
	return db, nil
}

func (db *DB) init(manifest *Manifest) {
	db.txid = manifest.ID
	db.manifest.Store(manifest)

	db.refs = make(map[string]int)
	db.incFileRefs(manifest)

	db.closing = make(chan struct{})

	if db.opts.EnableAutoCompact {
		db.bg.Go(db.autoCompact)
	}

	db.orphanedFiles = make(chan string, 16)
	db.bg.Go(db.deleteOrphanedFiles)

	db.mergePolicy = NewTieredMergePolicy()
	db.mergeRequests = make(chan chan error)
	db.bg.Go(db.runMerges)

}

func (db *DB) Close() {
	db.mu.Lock()
	defer db.mu.Unlock()

	close(db.closing)
	close(db.mergeRequests)
	close(db.orphanedFiles)
	db.bg.Wait()

	if db.wlock != nil {
		db.wlock.Close()
		db.wlock = nil
		log.Println("released write lock")
	}

	db.closed = true
}

func (db *DB) Compact() error {
	ch := make(chan error)
	db.mergeRequests <- ch
	err := <-ch
	return err
}

func (db *DB) autoCompact() error {
	interval := db.opts.AutoCompactInterval
	log.Printf("scheduling auto-compact to run every %v", interval)
	ticker := time.NewTicker(interval)
	for {
		select {
		case <-ticker.C:
			err := db.Compact()
			if err != nil {
				log.Printf("auto-compact failed: %v", err)
				interval += interval / 2
				log.Printf("increasing auto-compact interval to %v", interval)
				ticker.Stop()
				ticker = time.NewTicker(interval)
			}
		case <-db.closing:
			ticker.Stop()
			return nil
		}
	}
}

func (db *DB) runMerges() error {
	for ch := range db.mergeRequests {
		ch <- db.runOneMerge(0)
	}
	return nil
}

func (db *DB) runOneMerge(maxSize int) error {
	snapshot := db.newSnapshot()
	defer snapshot.Close()

	merge := db.mergePolicy.FindBestMerge(snapshot.manifest, maxSize)
	if merge == nil {
		return nil
	}

	return merge.Run(db)
}

func (db *DB) deleteOrphanedFiles() error {
	for name := range db.orphanedFiles {
		err := db.fs.Remove(name)
		if err != nil {
			log.Printf("[ERROR] failed to delete file %q: %v", name, err)
		} else {
			debugLog.Printf("deleted file %q", name)
		}
	}
	return nil
}

// Note: This must be called under a locked mutex.
func (db *DB) incFileRefs(m *Manifest) {
	for _, segment := range m.Segments {
		for _, name := range segment.fileNames() {
			db.refs[name]++
		}
	}
}

// Note: This must be called under a locked mutex.
func (db *DB) decFileRefs(m *Manifest) {
	for _, segment := range m.Segments {
		for _, name := range segment.fileNames() {
			db.refs[name]--
			if db.refs[name] <= 0 {
				log.Printf("file %q is no longer needed", name)
				db.orphanedFiles <- name
			}
		}
	}
}

func (db *DB) Add(docID uint32, hashes []uint32) error {
	return db.RunInTransaction(func(txn Batch) error { return txn.Add(docID, hashes) })
}

func (db *DB) Delete(docID uint32) error {
	return db.RunInTransaction(func(txn Batch) error { return txn.Delete(docID) })
}

// Import adds a stream of items to the index.
func (db *DB) Import(input ItemReader) error {
	return db.RunInTransaction(func(txn Batch) error { return txn.Import(input) })
}

// Truncate deletes all docs from the index.
func (db *DB) Truncate() error {
	snapshot := db.newSnapshot()
	defer snapshot.Close()

	return db.commit(func(base *Manifest) (*Manifest, error) {
		manifest := base.Clone()
		for _, segment := range snapshot.manifest.Segments {
			manifest.RemoveSegment(segment)
		}
		return manifest, nil
	})
}

func (db *DB) commit(prepareCommit func(base *Manifest) (*Manifest, error)) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		return ErrAlreadyClosed
	}

	base := db.manifest.Load().(*Manifest)

	manifest, err := prepareCommit(base)
	if err != nil {
		return errors.Wrap(err, "commit preparation failed")
	}

	manifest.ID = atomic.AddUint32(&db.txid, 1)

	for _, segment := range manifest.Segments {
		err := segment.SaveUpdate(db.fs, manifest.ID)
		if err != nil {
			return errors.Wrap(err, "segment update failed")
		}
	}

	err = manifest.Save(db.fs)
	if err != nil {
		return errors.Wrap(err, "save failed")
	}

	db.incFileRefs(manifest)
	db.decFileRefs(base)

	db.manifest.Store(manifest)

	log.Printf("committed transaction %d (docs=%v, items=%v, segments=%v, checksum=%d)",
		manifest.ID, manifest.NumDocs-manifest.NumDeletedDocs, manifest.NumItems, len(manifest.Segments), manifest.Checksum)

	return nil
}

func (db *DB) Search(query []uint32) (map[uint32]int, error) {
	snapshot := db.newSnapshot()
	defer snapshot.Close()
	return snapshot.Search(query)
}

// Snapshot creates a consistent read-only view of the DB.
func (db *DB) Snapshot() Searcher {
	return db.newSnapshot()
}

func (db *DB) closeTransaction(tx *Transaction) error {
	numTransactions := atomic.AddInt64(&db.numTransactions, -1)
	if numTransactions == 0 {
		debugLog.Printf("closed transaction %p", tx)
	} else {
		debugLog.Printf("closed transaction %p, %v transactions still open", tx, numTransactions)
	}
	return nil
}

// Transaction starts a new write transaction. You need to explicitly call Commit for the changes to be applied.
func (db *DB) Transaction() (Batch, error) {
	snapshot := db.newSnapshot()

	db.mu.Lock()
	defer db.mu.Unlock()

	if db.closed {
		snapshot.Close()
		return nil, ErrAlreadyClosed
	}

	if db.wlock == nil {
		lock, err := db.fs.Lock("write.lock")
		if err != nil {
			snapshot.Close()
			return nil, errors.Wrap(err, "unable to acquire write lock")
		}
		log.Println("acquired write lock")
		db.wlock = lock
	}

	tx := &Transaction{snapshot: snapshot, db: db, closeFn: db.closeTransaction}
	tx.init()

	atomic.AddInt64(&db.numTransactions, 1)

	debugLog.Printf("created transaction %p (base=%v)", tx, tx.snapshot.manifest.ID)

	return tx, nil
}

// RunInTransaction executes the given function in a transaction. If the function does not return an error,
// the transaction will be automatically committed.
func (db *DB) RunInTransaction(fn func(txn Batch) error) error {
	txn, err := db.Transaction()
	if err != nil {
		return err
	}
	defer txn.Close()

	err = fn(txn)
	if err != nil {
		return err
	}

	return txn.Commit()
}

func (db *DB) closeSnapshot(snapshot *Snapshot) error {
	db.mu.RLock()
	defer db.mu.RUnlock()

	db.decFileRefs(snapshot.manifest)

	numSnapshots := atomic.AddInt64(&db.numSnapshots, -1)
	if numSnapshots == 0 {
		debugLog.Printf("closed snapshot %p (%v)", snapshot, snapshot.manifest.ID)
	} else {
		debugLog.Printf("closed snapshot %p (%v), %v snapshots still open", snapshot, snapshot.manifest.ID, numSnapshots)
	}

	return nil
}

func (db *DB) newSnapshot() *Snapshot {
	db.mu.RLock()
	defer db.mu.RUnlock()

	snapshot := &Snapshot{
		manifest: db.manifest.Load().(*Manifest),
		closeFn:  db.closeSnapshot,
	}

	db.incFileRefs(snapshot.manifest)
	atomic.AddInt64(&db.numSnapshots, 1)

	debugLog.Printf("created snapshot %p (id=%v)", snapshot, snapshot.manifest.ID)

	return snapshot
}

func (db *DB) createSegment(input ItemReader) (*Segment, error) {
	return CreateSegment(db.fs, atomic.AddUint32(&db.txid, 1), input)
}

func (db *DB) Reader() ItemReader {
	var readers []ItemReader
	manifest := db.manifest.Load().(*Manifest)
	for _, segment := range manifest.Segments {
		readers = append(readers, segment.Reader())
	}
	return MergeItemReaders(readers...)
}

func (db *DB) NumSegments() int {
	manifest := db.manifest.Load().(*Manifest)
	return len(manifest.Segments)
}

func (db *DB) NumDocs() int {
	manifest := db.manifest.Load().(*Manifest)
	return manifest.NumDocs
}

func (db *DB) NumDeletedDocs() int {
	manifest := db.manifest.Load().(*Manifest)
	return manifest.NumDeletedDocs
}

// Contains returns true if the DB contains the given docID.
func (db *DB) Contains(docID uint32) bool {
	manifest := db.manifest.Load().(*Manifest)
	for _, segment := range manifest.Segments {
		if segment.Contains(docID) {
			return true
		}
	}
	return false
}
