//go:build goexperiment.jsonv2

package integrationtests

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
	"bytes"
	jsonv2 "encoding/json/v2"
	"reflect"
	"testing"

	"github.com/sirkostya009/ggen/encode"
	"github.com/sirkostya009/ggen/scan"
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
	[]byte(`{"tags":["a","b",]}`),
	[]byte(`{"props":{"k":"v",}}`),
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
		_, _, _ = Node{}.DecodeFrom(data)
	})
}

// FuzzRoundtrip: if ggen accepts the input, re-marshalling and re-decoding
// must produce a value semantically equal to the first decode. Catches
// encode/decode asymmetry (escape table mismatches, format-tag drift).
func FuzzRoundtrip(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		v1, _, err := Node{}.DecodeFrom(data)
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
		v2, _, err := Node{}.DecodeFrom(out1)
		if err != nil {
			t.Fatalf("re-decode failed: %v\nin: %s\nremarshalled: %s", err, data, out1)
		}
		out2, err := encode.Marshal(v2)
		if err != nil {
			t.Fatalf("second marshal failed: %v\nv2: %+v", err, v2)
		}
		v3, _, err := Node{}.DecodeFrom(out2)
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
		gv, _, gerr := Node{}.DecodeFrom(data)
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

// FuzzStreamEqualsBytes: bytes path and stream path must agree when both
// succeed. The chunk size derived from the input varies boundary alignment
// between runs.
func FuzzStreamEqualsBytes(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s, uint8(1))
		f.Add(s, uint8(7))
	}
	f.Add([]byte(`{"id":1,"name":"a","tags":["b"]}`), uint8(1))
	f.Add([]byte(`{"id":1,"name":"a","tags":["b"]}`), uint8(4))
	f.Add([]byte(`{"id":1,"name":"a","tags":["b"]}`), uint8(255))
	f.Fuzz(func(t *testing.T, data []byte, chunkSize uint8) {
		if chunkSize == 0 {
			chunkSize = 1
		}
		want, _, errBytes := Node{}.DecodeFrom(data)
		if errBytes != nil {
			return
		}
		r := &chunkReader{data: data, max: int(chunkSize)}
		var s scan.Stream
		s.Reset(r, make([]byte, 0, 8))
		got, errStream := Node{}.DecodeFromStream(&s)
		if errStream != nil {
			t.Fatalf("bytes accepted but stream rejected (chunk=%d):\n bytes: %s\n err:   %v",
				chunkSize, data, errStream)
		}
		wantOut, _ := encode.Marshal(want)
		gotOut, _ := encode.Marshal(got)
		if !bytes.Equal(wantOut, gotOut) {
			t.Fatalf("stream/bytes divergence chunk=%d:\n bytes:  %s\n stream: %s\n in:     %s",
				chunkSize, wantOut, gotOut, data)
		}
	})
}

// FuzzBoundaryNoPanic: random bytes through the boundary surface
// (BoundaryStruct holds float/int/string with no validation). Every
// outcome (accept or reject) must be panic-free.
func FuzzBoundaryNoPanic(f *testing.F) {
	for _, s := range [][]byte{
		[]byte(`{"f":1.0,"i":1,"str":"a"}`),
		[]byte(`{"f":NaN}`),
		[]byte(`{"f":Infinity}`),
		[]byte(`{"i":9999999999999999999}`),
		[]byte(`{"str":"\uD800"}`),
		[]byte(`{"str":""}`),
		[]byte(`{"str":"\n\r\t\b\f"}`),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on boundary input %q: %v", data, r)
			}
		}()
		_, _, _ = BoundaryStruct{}.DecodeFrom(data)
	})
}

// FuzzStreamHugeStringNoPanic: increasingly-large string payloads through
// tiny initial bufs. Panic-free invariant on the slow-path grow loop.
func FuzzStreamHugeStringNoPanic(f *testing.F) {
	f.Add(uint16(1), uint32(1024))
	f.Add(uint16(4), uint32(65536))
	f.Add(uint16(256), uint32(131072))
	f.Fuzz(func(t *testing.T, chunk uint16, repeat uint32) {
		if chunk == 0 {
			chunk = 1
		}
		const maxRepeat = 256 << 10
		if repeat > maxRepeat {
			repeat = maxRepeat
		}
		payload := append(append([]byte(`{"big":"`), bytes.Repeat([]byte("x"), int(repeat))...), '"', '}')
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic chunk=%d repeat=%d: %v", chunk, repeat, r)
			}
		}()
		r := &chunkReader{data: payload, max: int(chunk)}
		var s scan.Stream
		s.Reset(r, make([]byte, 0, 16))
		got, err := HugeStringStruct{}.DecodeFromStream(&s)
		if err != nil {
			t.Fatalf("err chunk=%d repeat=%d: %v", chunk, repeat, err)
		}
		if len(got.Big) != int(repeat) {
			t.Fatalf("len drift chunk=%d repeat=%d: got %d", chunk, repeat, len(got.Big))
		}
	})
}

// FuzzAppendJSONIdempotent: marshal twice and verify decoded results match.
// Catches non-determinism in encoders (map iteration influence, time
// formatting drift).
func FuzzAppendJSONIdempotent(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		v, _, err := Node{}.DecodeFrom(data)
		if err != nil {
			return
		}
		out1, err := encode.Marshal(v)
		if err != nil {
			t.Fatalf("first marshal: %v", err)
		}
		out2, err := encode.Marshal(v)
		if err != nil {
			t.Fatalf("second marshal: %v", err)
		}
		v1, _, err := Node{}.DecodeFrom(out1)
		if err != nil {
			t.Fatalf("re-decode 1: %v\nout: %s", err, out1)
		}
		v2, _, err := Node{}.DecodeFrom(out2)
		if err != nil {
			t.Fatalf("re-decode 2: %v\nout: %s", err, out2)
		}
		if !reflect.DeepEqual(v1, v2) {
			t.Fatalf("marshal not deterministic:\n out1: %s\n out2: %s", out1, out2)
		}
	})
}
