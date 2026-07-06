<p align="center"><img src="https://raw.githubusercontent.com/go-ruby-pstore/brand/main/social/go-ruby-pstore-pstore.png" alt="go-ruby-pstore/pstore" width="720"></p>

# pstore — go-ruby-pstore

[![Docs](https://img.shields.io/badge/docs-mkdocs--material-DC2626)](https://go-ruby-pstore.github.io/docs/)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-blue)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26.4%2B-00ADD8)](https://go.dev/dl/)
[![Coverage](https://img.shields.io/badge/coverage-100%25-1a7f37)](#tests--coverage)

**A pure-Go (no cgo) reimplementation of the transaction engine at the heart of
Ruby's [PStore](https://docs.ruby-lang.org/en/master/PStore.html)** — MRI 4.0.5's
transactional, `Marshal`-backed object store (the `pstore-0.2.1` gem). It runs the
load → transaction-body → commit/abort state machine over a Hash "table" and
serialises that table with [go-ruby-marshal](https://github.com/go-ruby-marshal/marshal),
so the on-disk bytes are **byte-compatible with a file written by MRI's PStore** —
**without any Ruby runtime**.

It is the PStore backend for
[go-embedded-ruby](https://github.com/go-embedded-ruby/ruby), but is a
**standalone, reusable** module — a sibling of
[go-ruby-marshal](https://github.com/go-ruby-marshal/marshal),
[go-ruby-yaml](https://github.com/go-ruby-yaml/yaml),
[go-ruby-regexp](https://github.com/go-ruby-regexp/regexp), and
[go-ruby-erb](https://github.com/go-ruby-erb/erb).

> **What it is — and isn't.** The transactional core — load the table, run the
> body, commit on normal exit (skipping the write when nothing changed), abort or
> commit early, forbid writes in a read-only transaction, refuse a nested
> transaction, raise outside one — is fully deterministic and needs **no
> interpreter**, so it lives here as pure Go over the Marshal codec. The *file*
> half — opening the store file (`O_CREAT`), `flock(LOCK_SH/LOCK_EX)`, the
> atomic-rename / rewind-truncate save strategies — is the host's job, injected as
> a two-method `Backend`. rbgo wires the real `os.File` + `syscall.Flock`; tests
> use an in-memory backend, so the whole suite is deterministic and Ruby-free.

## Features

Faithful port of PStore's transaction semantics, validated against the `ruby`
binary on every supported platform:

- **Commit on normal exit** — a read-write transaction whose body returns normally
  Marshal-dumps the table back through the backend, **only if the bytes changed**
  (MRI's checksum/size guard); an unchanged transaction performs no write.
- **`Commit` / `Abort` early exit** — return `t.Commit()` or `t.Abort()` from the
  body to exit it early (MRI's `throw :pstore_abort_transaction`); `Abort` discards
  every change, `Commit` persists the work so far. The body does not continue past
  either.
- **Read-only transactions** — `Set` and `Delete` raise `PStore::Error`
  (`"in read-only transaction"`) and the backend is never written.
- **The error taxonomy** — a single `*pstore.Error` (Ruby's `PStore::Error`) with
  MRI's exact messages: `"not in transaction"`, `"in read-only transaction"`,
  `"nested transaction"`, `"undefined key '…'"`, and
  `"PStore file seems to be corrupted."`.
- **The on-disk Marshal format** — the table is `Marshal.dump`/`load`ed via
  go-ruby-marshal, so a file this engine commits is read back unchanged by MRI's
  `PStore`, and vice-versa (a real `ruby -rpstore` file loads here).

CGO-free, **100% test coverage**, `gofmt` + `go vet` clean, and green across the
six 64-bit Go targets (amd64, arm64, riscv64, loong64, ppc64le, s390x) and three
OSes (Linux, macOS, Windows).

## Install

```sh
go get github.com/go-ruby-pstore/pstore
```

## Usage

```go
package main

import (
	"fmt"

	"github.com/go-ruby-marshal/marshal"
	"github.com/go-ruby-pstore/pstore"
)

func main() {
	// The host supplies the file seam (real os.File + flock in rbgo); here an
	// in-memory backend stands in.
	be := &mem{}
	store := pstore.New(be)

	// A read-write transaction: writes commit on normal return.
	store.Transaction(false, func(t *pstore.Tx) error {
		t.Set(marshal.Symbol("foo"), marshal.NewInt(0))
		t.Set(marshal.NewString("baz"), &marshal.Array{
			Elems: []marshal.Value{marshal.NewInt(1), marshal.NewInt(2)},
		})
		return nil // commit
	})

	// A read-only transaction: reads only; a write here would raise PStore::Error.
	store.Transaction(true, func(t *pstore.Tx) error {
		roots, _ := t.Roots()           // [:foo, "baz"]
		v, ok, _ := t.Get(marshal.Symbol("foo"))
		fmt.Println(roots, v, ok)
		return nil
	})
}

type mem struct{ b []byte }

func (m *mem) Load() ([]byte, error)  { return m.b, nil }
func (m *mem) Store(b []byte) error   { m.b = b; return nil }
```

## API

```go
// New returns a Store over an injected Backend.
func New(b Backend) *Store

// Backend is the file seam the host injects (real os.File + flock in rbgo).
type Backend interface {
	Load() ([]byte, error)   // current store bytes (empty == fresh store)
	Store(data []byte) error // overwrite with the marshalled table
}

// Transaction loads the table, runs body, and — on a committing read-write
// transaction whose table changed — Marshal-dumps it back to the backend.
// body returns t.Commit()/t.Abort() to exit early, any other error to propagate
// (no commit), or nil to finish (a read-write transaction then commits).
func (s *Store) Transaction(readOnly bool, body func(t *Tx) error) error

// In-transaction API (mirrors PStore#[], #[]=, #delete, #fetch, #roots/#keys,
// #root?/#key?, #commit, #abort). Keys and values are go-ruby-marshal Values.
func (t *Tx) Get(key marshal.Value) (marshal.Value, bool, error)            // PStore#[]
func (t *Tx) Set(key, val marshal.Value) error                             // PStore#[]=
func (t *Tx) Delete(key marshal.Value) (marshal.Value, error)              // PStore#delete
func (t *Tx) Fetch(key marshal.Value, def marshal.Value) (marshal.Value, error) // PStore#fetch
func (t *Tx) Roots() ([]marshal.Value, error)                              // PStore#roots
func (t *Tx) RootQ(key marshal.Value) (bool, error)                        // PStore#root?
func (t *Tx) Commit() error                                                // PStore#commit
func (t *Tx) Abort() error                                                 // PStore#abort

// Error is PStore::Error.
type Error struct{ /* … */ }
```

The table's keys and values are
[go-ruby-marshal](https://github.com/go-ruby-marshal/marshal) `Value`s
(`marshal.Symbol`, `*marshal.Str`, `marshal.Int`, `*marshal.Array`,
`*marshal.Hash`, …) — the same typed model the rest of the go-embedded-ruby stack
speaks, so a host binds its own Ruby objects to and from this engine exactly as it
does for Marshal.

## Tests & coverage

The suite pairs deterministic, ruby-free tests over the in-memory backend (which
alone hold coverage at 100%, so the qemu cross-arch and Windows lanes pass the
gate) with a **differential MRI oracle**: it writes a store with the real
`ruby -rpstore`, loads its file bytes here, and round-trips the table both
directions (MRI→Go and Go→MRI), plus replays MRI's own commit / abort / read-only
sequence and asserts the engine produces the identical persisted roots. The oracle
scripts `$stdout.binmode` so Windows text-mode never corrupts the raw Marshal
bytes, and skip themselves where `ruby` is absent.

```sh
COVERPKG=$(go list ./... | paste -sd, -)
go test -race -coverpkg="$COVERPKG" -coverprofile=cover.out ./...
go tool cover -func=cover.out | tail -1   # 100.0%
```

## License

BSD-3-Clause — see [LICENSE](LICENSE). Copyright the go-ruby-pstore/pstore authors.

## WebAssembly

Being pure Go (CGO=0), this library also compiles to **WebAssembly** — both
`GOOS=js GOARCH=wasm` (browser / Node.js) and `GOOS=wasip1 GOARCH=wasm` (WASI).
CI builds both targets on every push, alongside the six 64-bit native/qemu arches.

```sh
GOOS=js     GOARCH=wasm go build ./...   # browser / Node
GOOS=wasip1 GOARCH=wasm go build ./...   # WASI (wasmtime, wasmer, wasmedge, …)
```
