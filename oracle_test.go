// Copyright (c) the go-ruby-pstore/pstore authors
//
// SPDX-License-Identifier: BSD-3-Clause

package pstore

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/go-ruby-marshal/marshal"
)

// rubyBin locates a usable `ruby` once. The oracle tests skip themselves when it
// is absent (the qemu cross-arch lanes and the Windows lane), so the deterministic
// in-memory suite alone drives the 100% gate there.
func rubyBin(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ruby")
	if err != nil {
		t.Skip("ruby not on PATH; skipping MRI PStore oracle")
	}
	return path
}

// rubyEval runs a Ruby script and returns its stdout. The script $stdout.binmode's
// itself (via the preamble) so Windows text-mode never pollutes the bytes — the
// PStore file is raw Marshal, so a stray CRLF would corrupt the comparison.
func rubyEval(t *testing.T, bin, script string) []byte {
	t.Helper()
	cmd := exec.Command(bin, "-rpstore", "-rtempfile", "-e", "$stdout.binmode\n"+script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ruby error: %v\nscript:\n%s\noutput:\n%s", err, script, out)
	}
	return out
}

// TestOracleDiskFormatMRIToGo writes a store with the real `ruby -rpstore`, then
// loads its file bytes here and checks the table matches — proving the on-disk
// format is byte-compatible with MRI's PStore in the MRI→Go direction.
func TestOracleDiskFormatMRIToGo(t *testing.T) {
	bin := rubyBin(t)
	raw := rubyEval(t, bin, `
Tempfile.create do |f|
  s = PStore.new(f.path)
  s.transaction do
    s[:foo] = 0
    s[:bar] = 1
    s["baz"] = [1, 2, 3]
  end
  $stdout.write(File.binread(f.path))
end`)

	b := &memBackend{data: raw}
	s := New(b)
	if err := s.Transaction(true, func(tx *Tx) error {
		roots, err := tx.Roots()
		if err != nil {
			return err
		}
		if got := keyNames(roots); got != "foo,bar,baz" {
			t.Fatalf("roots = %q, want foo,bar,baz", got)
		}
		v, _, _ := tx.Get(sym("foo"))
		if v.(marshal.Int).I.Int64() != 0 {
			t.Fatalf("foo = %v", v)
		}
		v, _, _ = tx.Get(str("baz"))
		arr := v.(*marshal.Array)
		if len(arr.Elems) != 3 || arr.Elems[2].(marshal.Int).I.Int64() != 3 {
			t.Fatalf("baz = %v", arr)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestOracleDiskFormatGoToMRI writes a store here, then has the real `ruby -rpstore`
// open it and inspect the table — proving the Go→MRI direction (a file this engine
// commits is readable by MRI's PStore unchanged).
func TestOracleDiskFormatGoToMRI(t *testing.T) {
	bin := rubyBin(t)

	b := &memBackend{}
	s := New(b)
	if err := s.Transaction(false, func(tx *Tx) error {
		if err := tx.Set(sym("alpha"), i64(10)); err != nil {
			return err
		}
		if err := tx.Set(str("beta"), str("hello")); err != nil {
			return err
		}
		return tx.Set(sym("nums"), &marshal.Array{Elems: []marshal.Value{i64(1), i64(2), i64(3)}})
	}); err != nil {
		t.Fatal(err)
	}

	// Hand the bytes to MRI by hex (avoids any pipe text-mode issue) and let its
	// PStore read them back inside a read-only transaction.
	hexData := hexEncode(b.data)
	out := rubyEval(t, bin, `
data = ["`+hexData+`"].pack("H*")
Tempfile.create do |f|
  File.binwrite(f.path, data)
  s = PStore.new(f.path)
  s.transaction(true) do
    print s.roots.map(&:to_s).join(","), "|"
    print s[:alpha], "|"
    print s["beta"], "|"
    print s[:nums].inspect
  end
end`)
	got := string(out)
	want := "alpha,beta,nums|10|hello|[1, 2, 3]"
	if got != want {
		t.Fatalf("MRI read back %q, want %q", got, want)
	}
}

// TestOracleCommitAbortReadOnlySemantics mirrors MRI's own commit / abort /
// read-only behaviour against the reference interpreter: each script drives MRI's
// PStore through the scenario and prints the resulting persisted roots, which must
// match what this engine produces for the identical sequence.
func TestOracleCommitAbortReadOnlySemantics(t *testing.T) {
	bin := rubyBin(t)

	// MRI: abort discards, commit persists, read-only write raises PStore::Error.
	out := rubyEval(t, bin, `
Tempfile.create do |f|
  s = PStore.new(f.path)
  s.transaction { s[:a] = 1; s[:b] = 2 }
  # abort discards a change
  s.transaction { s[:c] = 3; s.abort }
  # commit persists an earlier change, body does not continue
  s.transaction { s[:d] = 4; s.commit; s[:never] = 9 }
  # read-only write raises
  begin
    s.transaction(true) { s[:x] = 1 }
  rescue PStore::Error => e
    print e.message, "|"
  end
  s.transaction(true) { print s.roots.map(&:to_s).sort.join(",") }
end`)
	if string(out) != "in read-only transaction|a,b,d" {
		t.Fatalf("MRI semantics = %q", out)
	}

	// This engine, identical sequence over the in-memory backend.
	b := &memBackend{}
	s := New(b)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.Transaction(false, func(tx *Tx) error {
		must(tx.Set(sym("a"), i64(1)))
		return tx.Set(sym("b"), i64(2))
	}))
	must(s.Transaction(false, func(tx *Tx) error {
		must(tx.Set(sym("c"), i64(3)))
		return tx.Abort()
	}))
	must(s.Transaction(false, func(tx *Tx) error {
		must(tx.Set(sym("d"), i64(4)))
		return tx.Commit()
	}))
	roErr := s.Transaction(true, func(tx *Tx) error { return tx.Set(sym("x"), i64(1)) })
	if roErr == nil || roErr.Error() != "in read-only transaction" {
		t.Fatalf("read-only write = %v", roErr)
	}
	must(s.Transaction(true, func(tx *Tx) error {
		roots, _ := tx.Roots()
		if got := keyNames(roots); got != "a,b,d" {
			t.Fatalf("engine roots = %q, want a,b,d", got)
		}
		return nil
	}))
}

func keyNames(ks []marshal.Value) string {
	var b strings.Builder
	for i, k := range ks {
		if i > 0 {
			b.WriteByte(',')
		}
		switch kv := k.(type) {
		case marshal.Symbol:
			b.WriteString(string(kv))
		case *marshal.Str:
			b.Write(kv.Bytes)
		}
	}
	return b.String()
}

func hexEncode(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, hexdigits[c>>4], hexdigits[c&0xf])
	}
	return string(out)
}
