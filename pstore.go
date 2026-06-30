// Copyright (c) the go-ruby-pstore/pstore authors
//
// SPDX-License-Identifier: BSD-3-Clause

// Package pstore is a pure-Go (no cgo) reimplementation of the transaction
// engine at the heart of Ruby's PStore — MRI 4.0.5's transactional, Marshal-backed
// object store (the pstore-0.2.1 gem).
//
// PStore persists a Hash (the "table") of Ruby objects to a file using Marshal.
// Reads and writes happen inside a transaction; on normal exit a read-write
// transaction commits the table back to the file (unless Abort was called),
// while a read-only transaction never writes. This package implements exactly
// that transactional, Marshal-(de)serialising core — independent of any
// interpreter — over two injected seams:
//
//   - a Backend (Load []byte / Store []byte): the bytes of the store file. The
//     real File IO, O_CREAT, flock(LOCK_SH/LOCK_EX) and the atomic-rename /
//     fast (rewind+truncate) save strategies are the host's job (rbgo wires
//     real os.File + syscall.Flock); tests use an in-memory backend so the suite
//     is fully deterministic and Ruby-free.
//   - the go-ruby-marshal codec, so the on-disk bytes are byte-compatible with a
//     file written by MRI's PStore (Marshal.dump of the table Hash).
//
// The table's keys and values are go-ruby-marshal Values (the same typed value
// model the rest of the go-embedded-ruby stack speaks), so a host binds its own
// Ruby objects to and from this model exactly as it does for Marshal itself.
//
// # What it is — and isn't
//
// The transaction state machine (load → run body → commit/abort, the read-only
// write guard, the not-in-transaction guard, the nested-transaction guard, the
// commit-only-on-change Marshal round-trip) is fully deterministic and needs no
// interpreter, so it lives here. The File open/flock/rename and the block's Ruby
// control flow (commit/abort exit the block via throw/catch in MRI) are the
// host's concern: this library exposes Commit and Abort as sentinel errors a
// host returns (or the body returns) to drive the same early-exit semantics.
package pstore

import (
	"errors"
	"fmt"

	"github.com/go-ruby-marshal/marshal"
)

// Error is the error type raised by every PStore operation that fails its
// transaction-state checks — the analogue of Ruby's PStore::Error. MRI raises it
// for "not in transaction", "in read-only transaction", "nested transaction",
// and "undefined key" (the no-default Fetch miss); the messages match.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func newError(format string, args ...any) *Error {
	return &Error{msg: fmt.Sprintf(format, args...)}
}

// errCommit and errAbort are the sentinel control-flow signals a transaction body
// returns to exit the block early, mirroring MRI's PStore#commit / PStore#abort
// (which throw :pstore_abort_transaction). Commit() and Abort() return them; the
// engine catches them so the body returns instead of unwinding to the caller.
var (
	errCommit = errors.New("pstore: commit")
	errAbort  = errors.New("pstore: abort")
)

// Backend is the injected file seam: the raw bytes of the store. Load returns the
// current contents (empty for a not-yet-written store); Store overwrites them. The
// host implements this over a real os.File (with O_CREAT and flock); tests use an
// in-memory backend. PStore only reads on transaction entry and writes at most
// once, on a committing read-write transaction whose table actually changed.
type Backend interface {
	// Load returns the current store bytes. An empty slice means a fresh store
	// (MRI treats an empty / newly-created file as the empty table {}).
	Load() ([]byte, error)
	// Store overwrites the store bytes with the marshalled table.
	Store(data []byte) error
}

// Store is a PStore over an injected Backend. It holds no open file or lock; the
// host arranges flock around a transaction. A Store is single-transaction-at-a-time:
// it tracks whether a transaction is active to reproduce MRI's not-in-transaction
// and nested-transaction errors.
type Store struct {
	backend Backend

	inTxn  bool // a transaction is currently running
	rdonly bool // the active transaction is read-only
	abort  bool // Abort was signalled in the active transaction

	keys []marshal.Value // table keys, in insertion order (Ruby Hash order)
	vals []marshal.Value // table values, parallel to keys
}

// New returns a Store backed by the given Backend.
func New(b Backend) *Store { return &Store{backend: b} }

// Tx is the handle passed to a transaction body. Its methods are the in-transaction
// PStore API; calling them is only legal while the body runs (the engine enforces
// this). The body returns nil to commit (read-write) or simply finish (read-only),
// or returns the result of t.Commit() / t.Abort() to exit early.
type Tx struct{ s *Store }

// Transaction runs body as a PStore transaction. It loads the table from the
// backend (an empty backend yields the empty table), runs body, and — on a
// committing read-write transaction whose table changed — marshals the table back
// to the backend. body returns t.Commit() or t.Abort() to exit early, any other
// error to propagate it (no commit happens), or nil to finish normally (which
// commits a read-write transaction unless Abort was signalled).
//
// readOnly true forbids writes: Set / Delete inside the body raise PStore::Error
// (Ruby's "in read-only transaction"), and the backend is never written.
//
// Transaction is not reentrant: calling it from within a body raises
// PStore::Error ("nested transaction"), matching MRI's lock.try_lock guard.
func (s *Store) Transaction(readOnly bool, body func(t *Tx) error) error {
	if s.inTxn {
		return newError("nested transaction")
	}
	s.inTxn = true
	s.rdonly = readOnly
	s.abort = false
	defer func() {
		s.inTxn = false
		s.keys = nil
		s.vals = nil
	}()

	if err := s.loadTable(); err != nil {
		return err
	}

	err := body(&Tx{s: s})
	switch {
	case errors.Is(err, errCommit):
		// fall through to the commit decision below (abort stays false).
	case errors.Is(err, errAbort):
		s.abort = true
	case err != nil:
		return err
	}

	if !s.abort && !readOnly {
		return s.saveTable()
	}
	return nil
}

// loadTable reads the backend and Marshal-loads the table into keys/vals. An empty
// backend is the empty table (MRI's newly-created-file case). A non-Hash payload is
// a corrupted store, exactly as MRI reports.
func (s *Store) loadTable() error {
	data, err := s.backend.Load()
	if err != nil {
		return err
	}
	if len(data) == 0 {
		s.keys = nil
		s.vals = nil
		return nil
	}
	v, err := marshal.Load(data)
	if err != nil {
		return newError("PStore file seems to be corrupted.")
	}
	h, ok := v.(*marshal.Hash)
	if !ok {
		return newError("PStore file seems to be corrupted.")
	}
	s.keys = append([]marshal.Value(nil), h.Keys...)
	s.vals = append([]marshal.Value(nil), h.Vals...)
	return nil
}

// saveTable Marshal-dumps the table and writes it to the backend only if the bytes
// differ from what is already there — mirroring MRI's checksum/size guard, which
// skips the write when nothing changed.
func (s *Store) saveTable() error {
	newData := marshal.Dump(&marshal.Hash{Keys: s.keys, Vals: s.vals})
	old, err := s.backend.Load()
	if err != nil {
		return err
	}
	if bytesEqual(old, newData) {
		return nil
	}
	return s.backend.Store(newData)
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// index returns the position of key in the table (by Ruby value equality), or -1.
func (s *Store) index(key marshal.Value) int {
	for i, k := range s.keys {
		if valueEqual(k, key) {
			return i
		}
	}
	return -1
}

// --- in-transaction guards (mirror MRI's in_transaction / in_transaction_wr) ---

func (t *Tx) inTransaction() error {
	if !t.s.inTxn {
		return newError("not in transaction")
	}
	return nil
}

func (t *Tx) inTransactionWr() error {
	if err := t.inTransaction(); err != nil {
		return err
	}
	if t.s.rdonly {
		return newError("in read-only transaction")
	}
	return nil
}

// --- in-transaction API (mirrors PStore#[], #[]=, #delete, #fetch, #keys, …) ---

// Get returns the value for key, or (nil, false) if absent — PStore#[] (which
// returns nil for a missing key). Outside a transaction it returns PStore::Error.
func (t *Tx) Get(key marshal.Value) (marshal.Value, bool, error) {
	if err := t.inTransaction(); err != nil {
		return nil, false, err
	}
	if i := t.s.index(key); i >= 0 {
		return t.s.vals[i], true, nil
	}
	return nil, false, nil
}

// Fetch returns the value for key, or def if key is absent. A nil def reproduces
// MRI's fetch(key) with no default: a miss raises PStore::Error ("undefined key").
// To get a nil default value, pass marshal.Nil{}.
func (t *Tx) Fetch(key marshal.Value, def marshal.Value) (marshal.Value, error) {
	if err := t.inTransaction(); err != nil {
		return nil, err
	}
	if i := t.s.index(key); i >= 0 {
		return t.s.vals[i], nil
	}
	if def == nil {
		return nil, newError("undefined key '%s'", inspectKey(key))
	}
	return def, nil
}

// Set creates or replaces the value for key — PStore#[]=. In a read-only
// transaction (or outside any transaction) it returns PStore::Error.
func (t *Tx) Set(key, val marshal.Value) error {
	if err := t.inTransactionWr(); err != nil {
		return err
	}
	if i := t.s.index(key); i >= 0 {
		t.s.vals[i] = val
		return nil
	}
	t.s.keys = append(t.s.keys, key)
	t.s.vals = append(t.s.vals, val)
	return nil
}

// Delete removes key and returns its old value (or nil if absent) — PStore#delete.
// In a read-only transaction (or outside any transaction) it returns PStore::Error.
func (t *Tx) Delete(key marshal.Value) (marshal.Value, error) {
	if err := t.inTransactionWr(); err != nil {
		return nil, err
	}
	i := t.s.index(key)
	if i < 0 {
		return nil, nil
	}
	old := t.s.vals[i]
	t.s.keys = append(t.s.keys[:i], t.s.keys[i+1:]...)
	t.s.vals = append(t.s.vals[:i], t.s.vals[i+1:]...)
	return old, nil
}

// Roots returns the table's keys in insertion order — PStore#roots (alias of
// PStore#keys). Outside a transaction it returns PStore::Error.
func (t *Tx) Roots() ([]marshal.Value, error) {
	if err := t.inTransaction(); err != nil {
		return nil, err
	}
	return append([]marshal.Value(nil), t.s.keys...), nil
}

// RootQ reports whether key exists — PStore#root? (alias of PStore#key?).
// Outside a transaction it returns PStore::Error.
func (t *Tx) RootQ(key marshal.Value) (bool, error) {
	if err := t.inTransaction(); err != nil {
		return false, err
	}
	return t.s.index(key) >= 0, nil
}

// Commit signals an early commit and exits the body — PStore#commit. Return its
// result from the body: the transaction commits (read-write) and the body ends.
func (t *Tx) Commit() error { return errCommit }

// Abort signals an early abort and exits the body — PStore#abort. Return its
// result from the body: no changes are written and the body ends.
func (t *Tx) Abort() error { return errAbort }
