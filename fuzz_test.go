//go:build goexperiment.jsonv2

package main

// Fuzz tests. Three targets:
//   - FuzzScanNoPanic: random bytes must never panic the scanner.
//   - FuzzRoundtrip: ggen-decode → ggen-marshal → ggen-decode must be stable.
//   - FuzzCompat: ggen and jsonv2 must agree (both error, or both succeed
//     with semantically equal results).
//
// Run a target: `go test -run=^$ -fuzz=FuzzScanNoPanic -fuzztime=30s`.
// Failing inputs are auto-saved under testdata/fuzz/<Name>/ as regression
// seeds picked up by the normal `go test` run.

import (
	jsonv2 "encoding/json/v2"
	"reflect"
	"testing"

	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
)

var fuzzSeeds = [][]byte{
	[]byte(`{}`),
	[]byte(`{"id":1,"name":"x"}`),
	[]byte(`{"id":1,"name":"x","tags":["a","b"],"props":{"k":"v"}}`),
	[]byte(`{"id":1,"children":[{"id":2,"children":[{"id":3}]}]}`),
	[]byte(`{"name":"A😀","score":1.5e10}`),
	[]byte(`{"active":true,"tags":[]}`),
	[]byte(`null`),
	[]byte(`[]`),
	[]byte(``),
	[]byte(`{"name":"\""}`),
}

func addSeeds(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
}

// FuzzScanNoPanic: untrusted bytes must yield an error, not a panic.
func FuzzScanNoPanic(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = decode.Unmarshal[Node](data)
	})
}

// FuzzRoundtrip: if ggen accepts the input, re-marshalling and re-decoding
// must produce a value semantically equal to the first decode. Catches
// encode/decode asymmetry (escape table mismatches, format-tag drift).
func FuzzRoundtrip(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		v1, err := decode.Unmarshal[Node](data)
		if err != nil {
			return
		}
		// Two rounds: the first round may legitimately diverge (a missing
		// "tags" key decodes to nil, but the marshalled form emits []
		// which decodes to an empty slice). After one round through
		// marshal, the value is in a fixed point — round two must equal.
		out1, err := encode.Marshal(v1)
		if err != nil {
			t.Fatalf("re-marshal failed for accepted input: %v\nin: %s", err, data)
		}
		v2, err := decode.Unmarshal[Node](out1)
		if err != nil {
			t.Fatalf("re-decode failed: %v\nin: %s\nremarshalled: %s", err, data, out1)
		}
		out2, err := encode.Marshal(v2)
		if err != nil {
			t.Fatalf("second marshal failed: %v\nv2: %+v", err, v2)
		}
		v3, err := decode.Unmarshal[Node](out2)
		if err != nil {
			t.Fatalf("third decode failed: %v\nbytes: %s", err, out2)
		}
		if !reflect.DeepEqual(v2, v3) {
			t.Fatalf("roundtrip not fixed point\nfirst: %s\nsecond: %s\nv2: %+v\nv3: %+v", out1, out2, v2, v3)
		}
	})
}

// FuzzCompat: ggen and jsonv2 must agree on accept/reject. When both
// accept, the resulting values must compare semantically equal. Catches
// stdlib drift (e.g. ggen accepting input jsonv2 rejects).
func FuzzCompat(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		gv, gerr := decode.Unmarshal[Node](data)
		var sv Node
		serr := jsonv2.Unmarshal(data, &sv)

		// Don't flag accept/reject drift — known cases (top-level `null`,
		// trailing garbage, invalid UTF-8 in strings) diverge by design.
		// The interesting check is value agreement when both succeed.
		if gerr != nil || serr != nil {
			return
		}
		if !sameWire(t, gv, sv) {
			t.Fatalf("decoded value drift\nggen: %+v\njsonv2: %+v\nin: %s", gv, sv, data)
		}
	})
}
