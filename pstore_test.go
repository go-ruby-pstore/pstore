// Copyright (c) the go-ruby-pstore/pstore authors
//
// SPDX-License-Identifier: BSD-3-Clause

package pstore

import (
	"errors"
	"math/big"
	"testing"

	"github.com/go-ruby-marshal/marshal"
)

// memBackend is the deterministic, in-memory Backend the suite drives the engine
// with — no File IO, no flock, no Ruby. It records reads and writes so tests can
// assert exactly when the engine touched the store.
type memBackend struct {
	data    []byte
	loads   int
	stores  int
	loadErr error // injected to exercise the backend-load error path
	stErr   error // injected to exercise the backend-store error path
}

func (m *memBackend) Load() ([]byte, error) {
	m.loads++
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	return m.data, nil
}

func (m *memBackend) Store(data []byte) error {
	m.stores++
	if m.stErr != nil {
		return m.stErr
	}
	m.data = append([]byte(nil), data...)
	return nil
}

func sym(s string) marshal.Symbol { return marshal.Symbol(s) }
func str(s string) *marshal.Str   { return marshal.NewString(s) }
func i64(n int64) marshal.Int     { return marshal.NewInt(n) }

// seed populates a backend with a committed table via one transaction.
func seed(t *testing.T) *memBackend {
	t.Helper()
	b := &memBackend{}
	s := New(b)
	if err := s.Transaction(false, func(tx *Tx) error {
		if err := tx.Set(sym("foo"), i64(0)); err != nil {
			return err
		}
		if err := tx.Set(sym("bar"), i64(1)); err != nil {
			return err
		}
		return tx.Set(str("baz"), &marshal.Array{Elems: []marshal.Value{i64(1), i64(2), i64(3)}})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return b
}

func TestCommitOnNormalExit(t *testing.T) {
	b := seed(t)
	if b.stores != 1 {
		t.Fatalf("expected 1 store after seed, got %d", b.stores)
	}
	// Re-open: the table persisted.
	s := New(b)
	err := s.Transaction(true, func(tx *Tx) error {
		v, ok, err := tx.Get(sym("foo"))
		if err != nil || !ok {
			t.Fatalf("get foo: %v ok=%v", err, ok)
		}
		if iv := v.(marshal.Int); iv.I.Int64() != 0 {
			t.Fatalf("foo = %v, want 0", iv.I)
		}
		roots, err := tx.Roots()
		if err != nil {
			return err
		}
		if len(roots) != 3 {
			t.Fatalf("roots = %v, want 3", roots)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read txn: %v", err)
	}
}

func TestNoWriteWhenUnchanged(t *testing.T) {
	b := seed(t)
	storesBefore := b.stores
	s := New(b)
	// A read-write transaction that reads but does not change the table must not
	// re-write it (MRI's checksum/size guard).
	if err := s.Transaction(false, func(tx *Tx) error {
		_, _, err := tx.Get(sym("foo"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if b.stores != storesBefore {
		t.Fatalf("unchanged commit wrote %d extra times", b.stores-storesBefore)
	}
}

func TestAbortDiscards(t *testing.T) {
	b := seed(t)
	s := New(b)
	storesBefore := b.stores
	err := s.Transaction(false, func(tx *Tx) error {
		if err := tx.Set(sym("zzz"), i64(99)); err != nil {
			return err
		}
		return tx.Abort()
	})
	if err != nil {
		t.Fatalf("abort txn returned %v", err)
	}
	if b.stores != storesBefore {
		t.Fatalf("abort wrote to backend (%d stores)", b.stores-storesBefore)
	}
	// zzz was not persisted.
	_ = s.Transaction(true, func(tx *Tx) error {
		if ok, _ := tx.RootQ(sym("zzz")); ok {
			t.Fatal("aborted key persisted")
		}
		return nil
	})
}

func TestCommitEarlyExit(t *testing.T) {
	b := seed(t)
	s := New(b)
	reached := false
	err := s.Transaction(false, func(tx *Tx) error {
		if err := tx.Set(sym("bat"), i64(3)); err != nil {
			return err
		}
		if cerr := tx.Commit(); cerr != nil {
			return cerr // exits the body; commit proceeds
		}
		reached = true // unreachable in MRI (commit throws)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if reached {
		t.Fatal("body continued past Commit")
	}
	_ = s.Transaction(true, func(tx *Tx) error {
		if ok, _ := tx.RootQ(sym("bat")); !ok {
			t.Fatal("committed key not persisted")
		}
		return nil
	})
}

func TestReadOnlyForbidsWrites(t *testing.T) {
	b := seed(t)
	s := New(b)
	storesBefore := b.stores
	for _, tc := range []struct {
		name string
		fn   func(tx *Tx) error
	}{
		{"set", func(tx *Tx) error { return tx.Set(sym("x"), i64(1)) }},
		{"delete", func(tx *Tx) error { _, err := tx.Delete(sym("foo")); return err }},
	} {
		err := s.Transaction(true, tc.fn)
		var pe *Error
		if !errors.As(err, &pe) || pe.msg != "in read-only transaction" {
			t.Fatalf("%s: want read-only error, got %v", tc.name, err)
		}
	}
	if b.stores != storesBefore {
		t.Fatal("read-only transaction wrote to backend")
	}
}

func TestReadOnlyAllowsReads(t *testing.T) {
	b := seed(t)
	s := New(b)
	if err := s.Transaction(true, func(tx *Tx) error {
		if _, _, err := tx.Get(sym("foo")); err != nil {
			return err
		}
		if _, err := tx.Roots(); err != nil {
			return err
		}
		if ok, err := tx.RootQ(sym("bar")); err != nil || !ok {
			t.Fatalf("rootq: %v ok=%v", err, ok)
		}
		_, err := tx.Fetch(sym("foo"), nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestNestedTransaction(t *testing.T) {
	b := &memBackend{}
	s := New(b)
	err := s.Transaction(false, func(tx *Tx) error {
		return s.Transaction(false, func(*Tx) error { return nil })
	})
	var pe *Error
	if !errors.As(err, &pe) || pe.msg != "nested transaction" {
		t.Fatalf("want nested transaction error, got %v", err)
	}
}

func TestOutsideTransaction(t *testing.T) {
	b := seed(t)
	s := New(b)
	// Smuggle the Tx out of the transaction so its methods run with inTxn=false,
	// reproducing MRI's "not in transaction" for every accessor.
	var leaked *Tx
	_ = s.Transaction(true, func(tx *Tx) error { leaked = tx; return nil })

	wantErr := func(name string, err error) {
		t.Helper()
		var pe *Error
		if !errors.As(err, &pe) || pe.msg != "not in transaction" {
			t.Fatalf("%s: want not-in-transaction, got %v", name, err)
		}
	}
	_, _, err := leaked.Get(sym("foo"))
	wantErr("Get", err)
	_, err = leaked.Fetch(sym("foo"), nil)
	wantErr("Fetch", err)
	err = leaked.Set(sym("foo"), i64(1))
	wantErr("Set", err)
	_, err = leaked.Delete(sym("foo"))
	wantErr("Delete", err)
	_, err = leaked.Roots()
	wantErr("Roots", err)
	_, err = leaked.RootQ(sym("foo"))
	wantErr("RootQ", err)
}

func TestGetMissAndDelete(t *testing.T) {
	b := seed(t)
	s := New(b)
	if err := s.Transaction(false, func(tx *Tx) error {
		if _, ok, err := tx.Get(sym("absent")); err != nil || ok {
			t.Fatalf("get absent: ok=%v err=%v", ok, err)
		}
		// Delete a present key returns its old value.
		old, err := tx.Delete(sym("foo"))
		if err != nil {
			return err
		}
		if old.(marshal.Int).I.Int64() != 0 {
			t.Fatalf("delete foo old = %v, want 0", old)
		}
		// Delete an absent key returns nil.
		old, err = tx.Delete(sym("absent"))
		if err != nil || old != nil {
			t.Fatalf("delete absent: old=%v err=%v", old, err)
		}
		// Overwrite an existing key (the index>=0 Set branch).
		return tx.Set(sym("bar"), i64(42))
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.Transaction(true, func(tx *Tx) error {
		if v, _, _ := tx.Get(sym("bar")); v.(marshal.Int).I.Int64() != 42 {
			t.Fatalf("bar not overwritten: %v", v)
		}
		if ok, _ := tx.RootQ(sym("foo")); ok {
			t.Fatal("foo not deleted")
		}
		return nil
	})
}

func TestFetchDefaultAndMiss(t *testing.T) {
	b := seed(t)
	s := New(b)
	_ = s.Transaction(true, func(tx *Tx) error {
		// Present key.
		v, err := tx.Fetch(sym("foo"), nil)
		if err != nil || v.(marshal.Int).I.Int64() != 0 {
			t.Fatalf("fetch foo: %v %v", v, err)
		}
		// Absent key with a default returns the default.
		def := str("fallback")
		v, err = tx.Fetch(sym("nope"), def)
		if err != nil || v != marshal.Value(def) {
			t.Fatalf("fetch default: %v %v", v, err)
		}
		// Absent key without a default raises PStore::Error.
		_, err = tx.Fetch(sym("nope"), nil)
		var pe *Error
		if !errors.As(err, &pe) || pe.msg != "undefined key 'nope'" {
			t.Fatalf("fetch miss: %v", err)
		}
		return nil
	})
}

func TestEmptyBackendIsEmptyTable(t *testing.T) {
	b := &memBackend{} // never written
	s := New(b)
	if err := s.Transaction(true, func(tx *Tx) error {
		roots, err := tx.Roots()
		if err != nil {
			return err
		}
		if len(roots) != 0 {
			t.Fatalf("fresh store roots = %v", roots)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if b.stores != 0 {
		t.Fatal("read-only on empty store wrote")
	}
}

func TestEmptyTableCommitWrites(t *testing.T) {
	// A read-write transaction on a fresh store that adds a key must write, and
	// the bytes must Marshal-load back to that table.
	b := &memBackend{}
	s := New(b)
	if err := s.Transaction(false, func(tx *Tx) error {
		return tx.Set(sym("k"), i64(7))
	}); err != nil {
		t.Fatal(err)
	}
	if b.stores != 1 {
		t.Fatalf("fresh commit wrote %d times", b.stores)
	}
	v, err := marshal.Load(b.data)
	if err != nil {
		t.Fatal(err)
	}
	h := v.(*marshal.Hash)
	if len(h.Keys) != 1 || h.Keys[0].(marshal.Symbol) != "k" {
		t.Fatalf("persisted table = %v", h)
	}
}

func TestBodyErrorPropagatesNoCommit(t *testing.T) {
	b := seed(t)
	s := New(b)
	storesBefore := b.stores
	sentinel := errors.New("boom")
	err := s.Transaction(false, func(tx *Tx) error {
		if err := tx.Set(sym("x"), i64(1)); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("want sentinel, got %v", err)
	}
	if b.stores != storesBefore {
		t.Fatal("errored transaction committed")
	}
}

func TestCorruptedStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		// Marshal of a non-Hash (an Integer) — valid Marshal, wrong top type.
		{"nonHash", marshal.Dump(i64(5))},
		// Not valid Marshal at all.
		{"garbage", []byte{0x04, 0x08, 0xff, 0xff}},
	} {
		b := &memBackend{data: tc.data}
		s := New(b)
		err := s.Transaction(true, func(*Tx) error { return nil })
		var pe *Error
		if !errors.As(err, &pe) || pe.msg != "PStore file seems to be corrupted." {
			t.Fatalf("%s: want corrupted error, got %v", tc.name, err)
		}
	}
}

func TestBackendLoadError(t *testing.T) {
	boom := errors.New("disk gone")
	b := &memBackend{loadErr: boom}
	s := New(b)
	if err := s.Transaction(true, func(*Tx) error { return nil }); !errors.Is(err, boom) {
		t.Fatalf("want load error, got %v", err)
	}
}

func TestBackendStoreError(t *testing.T) {
	boom := errors.New("disk full")
	b := &memBackend{stErr: boom}
	s := New(b)
	err := s.Transaction(false, func(tx *Tx) error { return tx.Set(sym("k"), i64(1)) })
	if !errors.Is(err, boom) {
		t.Fatalf("want store error, got %v", err)
	}
}

func TestSaveLoadErrorInGuard(t *testing.T) {
	// saveTable re-Loads to compare bytes; if that second Load fails, the error
	// must surface. Use a backend that succeeds on the entry load but fails the
	// compare load.
	b := &flakyBackend{}
	s := New(b)
	err := s.Transaction(false, func(tx *Tx) error { return tx.Set(sym("k"), i64(1)) })
	if err == nil || err.Error() != "vanished" {
		t.Fatalf("want compare-load error, got %v", err)
	}
}

// flakyBackend fails its second Load (the saveTable compare), succeeding the first.
type flakyBackend struct {
	f      bool
	loads  int
	stores int
}

func (f *flakyBackend) Load() ([]byte, error) {
	f.loads++
	if f.loads >= 2 {
		return nil, errors.New("vanished")
	}
	return nil, nil
}
func (f *flakyBackend) Store([]byte) error { f.stores++; return nil }

func TestErrorType(t *testing.T) {
	e := newError("x %d", 1)
	if e.Error() != "x 1" {
		t.Fatalf("Error() = %q", e.Error())
	}
}

func TestValueEqualScalars(t *testing.T) {
	bi := marshal.Int{I: big.NewInt(5)}
	cases := []struct {
		a, b marshal.Value
		want bool
	}{
		{marshal.Nil{}, marshal.Nil{}, true},
		{marshal.Nil{}, marshal.Bool(true), false},
		{marshal.Bool(true), marshal.Bool(true), true},
		{marshal.Bool(true), marshal.Bool(false), false},
		{marshal.Bool(true), marshal.Nil{}, false},
		{sym("a"), sym("a"), true},
		{sym("a"), sym("b"), false},
		{sym("a"), str("a"), false},
		{i64(5), bi, true},
		{i64(5), i64(6), false},
		{i64(5), sym("a"), false},
		{marshal.Float(1.5), marshal.Float(1.5), true},
		{marshal.Float(1.5), marshal.Float(2.5), false},
		{marshal.Float(1.5), i64(1), false},
		{str("a"), str("a"), true},
		{str("a"), str("b"), false},
		{str("a"), sym("a"), false},
	}
	for i, c := range cases {
		if got := valueEqual(c.a, c.b); got != c.want {
			t.Errorf("case %d: valueEqual(%v,%v)=%v want %v", i, c.a, c.b, got, c.want)
		}
	}
}

func TestValueEqualComposite(t *testing.T) {
	arr := func(vs ...marshal.Value) *marshal.Array { return &marshal.Array{Elems: vs} }
	if !valueEqual(arr(i64(1), i64(2)), arr(i64(1), i64(2))) {
		t.Fatal("equal arrays not equal")
	}
	if valueEqual(arr(i64(1)), arr(i64(1), i64(2))) {
		t.Fatal("diff-len arrays equal")
	}
	if valueEqual(arr(i64(1)), arr(i64(2))) {
		t.Fatal("diff-elem arrays equal")
	}
	if valueEqual(arr(i64(1)), sym("x")) {
		t.Fatal("array equals non-array")
	}

	h := func(k, v marshal.Value) *marshal.Hash {
		return &marshal.Hash{Keys: []marshal.Value{k}, Vals: []marshal.Value{v}}
	}
	if !valueEqual(h(sym("a"), i64(1)), h(sym("a"), i64(1))) {
		t.Fatal("equal hashes not equal")
	}
	if valueEqual(h(sym("a"), i64(1)), &marshal.Hash{}) {
		t.Fatal("diff-len hashes equal")
	}
	if valueEqual(h(sym("a"), i64(1)), h(sym("b"), i64(1))) {
		t.Fatal("diff-key hashes equal")
	}
	if valueEqual(h(sym("a"), i64(1)), h(sym("a"), i64(2))) {
		t.Fatal("diff-val hashes equal")
	}
	if valueEqual(h(sym("a"), i64(1)), sym("x")) {
		t.Fatal("hash equals non-hash")
	}

	// Composite keys exercise valueEqual's Array/Hash branches via Set/Get.
	b := &memBackend{}
	s := New(b)
	_ = s.Transaction(false, func(tx *Tx) error {
		_ = tx.Set(arr(i64(1), i64(2)), i64(10))
		v, ok, _ := tx.Get(arr(i64(1), i64(2)))
		if !ok || v.(marshal.Int).I.Int64() != 10 {
			t.Fatal("array key get failed")
		}
		return nil
	})

	// An unsupported value type compares unequal (default branch).
	if valueEqual(unknownValue{}, unknownValue{}) {
		t.Fatal("unknown value type compared equal")
	}
}

type unknownValue struct{}

func (unknownValue) RubyClass() string { return "Unknown" }

func TestInspectKey(t *testing.T) {
	cases := []struct {
		k    marshal.Value
		want string
	}{
		{sym("foo"), "foo"},
		{str("bar"), "bar"},
		{i64(7), "7"},
		{marshal.Int{}, "0"},
		{marshal.Float(1.5), "1.5"},
		{marshal.Bool(true), "true"},
		{marshal.Bool(false), "false"},
		{marshal.Nil{}, ""},
		{unknownValue{}, "{}"},
	}
	for _, c := range cases {
		if got := inspectKey(c.k); got != c.want {
			t.Errorf("inspectKey(%v) = %q, want %q", c.k, got, c.want)
		}
	}
}
