// Copyright (c) the go-ruby-pstore/pstore authors
//
// SPDX-License-Identifier: BSD-3-Clause

package pstore

import (
	"fmt"

	"github.com/go-ruby-marshal/marshal"
)

// valueEqual reports whether two go-ruby-marshal Values are the same Ruby Hash key
// — Ruby keys an entry by eql? + hash, so two distinct value objects that are eql?
// (a Symbol, a String, an Integer, a Float, nil, true/false of equal content) name
// the same slot. PStore keys are overwhelmingly Symbols or Strings; this covers the
// full scalar key model and falls back to structural equality for the (unusual)
// Array / Hash key.
func valueEqual(a, b marshal.Value) bool {
	switch av := a.(type) {
	case marshal.Nil:
		_, ok := b.(marshal.Nil)
		return ok
	case marshal.Bool:
		bv, ok := b.(marshal.Bool)
		return ok && av == bv
	case marshal.Symbol:
		bv, ok := b.(marshal.Symbol)
		return ok && av == bv
	case marshal.Int:
		bv, ok := b.(marshal.Int)
		return ok && av.I != nil && bv.I != nil && av.I.Cmp(bv.I) == 0
	case marshal.Float:
		bv, ok := b.(marshal.Float)
		return ok && av == bv
	case *marshal.Str:
		bv, ok := b.(*marshal.Str)
		// Ruby String keys are eql? by bytes (and encoding); PStore copies the
		// frozen key, so byte equality is what matters for slot identity.
		return ok && av.Enc == bv.Enc && bytesEqual(av.Bytes, bv.Bytes)
	case *marshal.Array:
		bv, ok := b.(*marshal.Array)
		if !ok || len(av.Elems) != len(bv.Elems) {
			return false
		}
		for i := range av.Elems {
			if !valueEqual(av.Elems[i], bv.Elems[i]) {
				return false
			}
		}
		return true
	case *marshal.Hash:
		bv, ok := b.(*marshal.Hash)
		if !ok || len(av.Keys) != len(bv.Keys) {
			return false
		}
		for i := range av.Keys {
			if !valueEqual(av.Keys[i], bv.Keys[i]) || !valueEqual(av.Vals[i], bv.Vals[i]) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// inspectKey renders a key the way MRI's PStore#fetch interpolates it into the
// "undefined key '%s'" message — Symbol via to_s (no leading colon, as %s does),
// String / scalar via their text. It is only used to build that error string.
func inspectKey(key marshal.Value) string {
	switch kv := key.(type) {
	case marshal.Symbol:
		return string(kv)
	case *marshal.Str:
		return string(kv.Bytes)
	case marshal.Int:
		if kv.I != nil {
			return kv.I.String()
		}
		return "0"
	case marshal.Float:
		return fmt.Sprintf("%v", float64(kv))
	case marshal.Bool:
		if bool(kv) {
			return "true"
		}
		return "false"
	case marshal.Nil:
		return ""
	default:
		return fmt.Sprintf("%v", key)
	}
}
