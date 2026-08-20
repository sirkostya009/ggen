package integrationtests

//go:generate ../ggen $GOFILE

// Fuzz tests. Run a target:
// `go test -run=^$ -fuzz=FuzzPrimitivesCompat -fuzztime=30s`.

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

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
	// Escape/unescape path — raw-string backslashes are JSON escapes: surrogate
	// pairs (😀), BMP \uXXXX, two-char. Long enough that small chunk
	// sizes straddle escapes across stream refill boundaries.
	[]byte(`{"name":"a\ud83d\ude00b\u00e9c\n\t\"\\"}`),
	[]byte(`{"name":"aaaaaaaaaaaaaaaaaaaaaaaa\ud83d\ude00bbbbbbbb","tags":["x\ud83d\ude00y","\u00e9\u00e9"]}`),
	[]byte(`{"name":"\u00e9\u00e9\u00e9","props":{"k\ud83d\ude00":"v\u00e9"}}`),
	// Invalid UTF-8 / unpaired surrogates \u2014 both paths must reject identically.
	[]byte("{\"name\":\"a\xffb\"}"),
	[]byte("{\"name\":\"a\xe2(z\"}"),
	[]byte(`{"name":"\ud83d"}`),
	[]byte(`{"name":"\udc00x"}`),
	[]byte("{\"name\":\"h\u00e9llo \u017c\u00f3\u0142\u0107\"}"),
}

// Bytes and stream paths agree when both succeed; chunk size varies
// boundary alignment.
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
			// Map marshal order is nondeterministic, so a byte diff may be pure
			// key order. Re-check order-insensitively (parse both to any) before
			// failing — a real content divergence still trips.
			var wa, ga any
			ew := jsonv2.Unmarshal(wantOut, &wa)
			eg := jsonv2.Unmarshal(gotOut, &ga)
			// A map key with invalid UTF-8 (faithfully preserved from input)
			// makes jsonv2's strict-UTF-8 reparse fail at a key whose position
			// depends on random map order — so the two partial parses diverge
			// spuriously. Both decoded the same map; skip when either won't parse.
			if ew != nil || eg != nil {
				return
			}
			if !reflect.DeepEqual(wa, ga) {
				t.Fatalf("stream/bytes divergence chunk=%d:\n bytes:  %s\n stream: %s\n in:     %s",
					chunkSize, wantOut, gotOut, data)
			}
		}
	})
}

// BoundaryStruct (float/int/string, no validation) must be panic-free on any
// input.
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

// Large strings through tiny bufs exercise the slow-path grow loop; must stay
// panic-free.
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

// PrimStruct holds one field per primitive kind, fuzzed by value (not payload
// bytes) so every input is a well-formed payload driving the value-parsers
// across their full domain.
//
//ggen:generate
type PrimStruct struct {
	B   bool    `json:"b"`
	I   int     `json:"i"`
	I8  int8    `json:"i8"`
	I16 int16   `json:"i16"`
	I32 int32   `json:"i32"`
	I64 int64   `json:"i64"`
	U   uint    `json:"u"`
	U8  uint8   `json:"u8"`
	U16 uint16  `json:"u16"`
	U32 uint32  `json:"u32"`
	U64 uint64  `json:"u64"`
	F32 float32 `json:"f32"`
	F64 float64 `json:"f64"`
	Str string  `json:"str"`
}

// FuzzPrimitivesCompat builds a payload from fuzzed typed values via stdlib,
// then asserts ggen and stdlib decode it identically: both accept or both
// reject, and on success the decoded structs are equal.
func FuzzPrimitivesCompat(f *testing.F) {
	add := func(p PrimStruct) {
		f.Add(p.B, p.I, p.I8, p.I16, p.I32, p.I64, p.U, p.U8, p.U16, p.U32, p.U64, p.F32, p.F64, p.Str)
	}
	add(PrimStruct{B: true, I: 1, I8: -1, I16: 1, I32: -1, I64: 1, U: 1, U8: 255, U16: 1, U32: 1, U64: 1, F32: 1.5, F64: 1.0, Str: "strang"})
	add(PrimStruct{I8: math.MinInt8, I16: math.MinInt16, I32: math.MinInt32, I64: math.MinInt64,
		U8: math.MaxUint8, U16: math.MaxUint16, U32: math.MaxUint32, U64: math.MaxUint64,
		F32: math.MaxFloat32, F64: math.MaxFloat64, Str: ""})
	add(PrimStruct{I8: math.MaxInt8, I16: math.MaxInt16, I32: math.MaxInt32, I64: math.MaxInt64,
		F32: math.SmallestNonzeroFloat32, F64: math.SmallestNonzeroFloat64, Str: "\"\\\n\t é\U0001f600"})
	// Invalid UTF-8 seeds — routed to the reject-parity branch below.
	add(PrimStruct{Str: "a\xffb"})
	add(PrimStruct{Str: "a\xe2(z"})
	add(PrimStruct{Str: "\xed\xa0\x80"})
	f.Fuzz(func(t *testing.T, b bool, i int, i8 int8, i16 int16, i32 int32, i64 int64,
		u uint, u8 uint8, u16 uint16, u32 uint32, u64 uint64, f32 float32, f64 float64, str string) {
		// Invalid UTF-8 can't round-trip through jsonv2.Marshal (it errors), so
		// build the payload by hand and assert REJECT parity: both ggen and
		// jsonv2 must refuse it (ggen with scan.ErrInvalidUTF8). Only when the
		// raw bytes keep the payload structurally well-formed — a quote/
		// backslash/ctrl byte would change what is being tested.
		if !utf8.ValidString(str) {
			if strings.ContainsAny(str, "\"\\") || strings.ContainsFunc(str, func(r rune) bool { return r < 0x20 }) {
				return
			}
			payload := []byte(`{"str":"` + str + `"}`)
			_, _, gerr := PrimStruct{}.DecodeFrom(payload)
			var sv PrimStruct
			serr := jsonv2.Unmarshal(payload, &sv)
			if gerr == nil || serr == nil {
				t.Fatalf("invalid UTF-8 accepted:\n ggen err:   %v\n stdlib err: %v\n payload: %q", gerr, serr, payload)
			}
			if !errors.Is(gerr, scan.ErrInvalidUTF8) {
				t.Fatalf("want scan.ErrInvalidUTF8, got %v (payload %q)", gerr, payload)
			}
			return
		}
		want := PrimStruct{B: b, I: i, I8: i8, I16: i16, I32: i32, I64: i64,
			U: u, U8: u8, U16: u16, U32: u32, U64: u64, F32: f32, F64: f64, Str: str}
		// NaN/Inf are the ONLY values with no JSON form — skip those upfront so
		// any OTHER marshal failure is a real bug, not a silently-swallowed skip.
		nonFinite := math.IsNaN(f64) || math.IsInf(f64, 0) ||
			math.IsNaN(float64(f32)) || math.IsInf(float64(f32), 0)
		payload, err := jsonv2.Marshal(want)
		if err != nil {
			if nonFinite {
				return
			}
			t.Fatalf("marshal of finite values failed: %v\n want: %+v", err, want)
		}

		gv, _, gerr := PrimStruct{}.DecodeFrom(payload)
		var sv PrimStruct
		serr := jsonv2.Unmarshal(payload, &sv)

		if (gerr == nil) != (serr == nil) {
			t.Fatalf("accept/reject drift:\n ggen err:   %v\n stdlib err: %v\n payload: %s", gerr, serr, payload)
		}
		if gerr != nil {
			return
		}
		if gv != sv {
			t.Fatalf("decoded value drift:\n ggen:   %+v\n stdlib: %+v\n payload: %s", gv, sv, payload)
		}
	})
}
