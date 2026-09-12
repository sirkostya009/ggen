package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"database/sql"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen"
)

// chunkReader delivers payload max bytes per Read, stressing Stream.ReadMore's
// per-call grow path.
type chunkReader struct {
	data []byte
	pos  int
	max  int // bytes per Read call
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.pos >= len(c.data) {
		return 0, io.EOF
	}
	n := min(len(p), c.max)
	if c.pos+n > len(c.data) {
		n = len(c.data) - c.pos
	}
	copy(p, c.data[c.pos:c.pos+n])
	c.pos += n
	return n, nil
}

func TestUnmarshal_roundtrip(t *testing.T) {
	t.Parallel()
	got, _, err := Node{}.DecodeFrom(complexPayload)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ID != 42 || got.Name != "hello world" || got.Score != 9.5 || !got.Active {
		t.Errorf("scalars wrong: %+v", got)
	}
	if len(got.Tags) != 3 || got.Tags[0] != "alpha" || got.Tags[2] != "gamma" {
		t.Errorf("Tags = %v", got.Tags)
	}
	if got.Props["k1"] != "v1" || got.Props["k2"] != "v2" {
		t.Errorf("Props = %v", got.Props)
	}
	if len(got.Children) != 2 || got.Children[0].ID != 1 || got.Children[1].Name != "child-2" {
		t.Errorf("Children = %+v", got.Children)
	}
}

func TestRead_roundtrip(t *testing.T) {
	t.Parallel()
	got, _, err := Node{}.DecodeFrom(complexPayload)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Name != "hello world" || got.ID != 42 {
		t.Errorf("got %+v", got)
	}
}

func TestUnmarshalSlice_roundtrip(t *testing.T) {
	t.Parallel()
	arr := []byte(`[
		{"street":"A","city":"X","zipCode":"00001"},
		{"street":"B","city":"Y","zipCode":"00002"}
	]`)
	got, err := ggen.UnmarshalSlice[Address](arr)
	if err != nil {
		t.Fatalf("UnmarshalSlice: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Street != "A" || got[1].City != "Y" {
		t.Errorf("got %+v", got)
	}
}

func TestReadSlice_roundtrip(t *testing.T) {
	t.Parallel()
	arr := []byte(`[{"street":"A","city":"X","zipCode":"00001"}]`)
	got, err := ggen.ReadSlice[Address](bytes.NewReader(arr))
	if err != nil {
		t.Fatalf("ReadSlice: %v", err)
	}
	if len(got) != 1 || got[0].Street != "A" {
		t.Errorf("got %+v", got)
	}
}

func TestUnmarshalStream_roundtrip(t *testing.T) {
	t.Parallel()
	var s ggen.Stream
	s.Reset(bytes.NewReader(complexPayload), make([]byte, 0, len(complexPayload)))
	got, err := Node{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("UnmarshalStream: %v", err)
	}
	if got.Name != "hello world" || got.ID != 42 || got.Score != 9.5 {
		t.Errorf("got %+v", got)
	}
	if len(got.Tags) != 3 || len(got.Children) != 2 {
		t.Errorf("slices wrong: %+v", got)
	}
	if got.Children[1].Name != "child-2" {
		t.Errorf("nested struct wrong: %+v", got)
	}
}

func TestUnmarshalStream_chunked(t *testing.T) {
	t.Parallel()
	r := &chunkReader{data: complexPayload, max: 1}
	var s ggen.Stream
	s.Reset(r, nil)
	got, err := Node{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("UnmarshalStream (1-byte chunks): %v", err)
	}
	if got.Name != "hello world" || got.ID != 42 {
		t.Errorf("chunked decode wrong: %+v", got)
	}
	if len(got.Tags) != 3 || got.Tags[2] != "gamma" {
		t.Errorf("tags wrong: %v", got.Tags)
	}
}

func TestUnmarshalStream_tinyInitial(t *testing.T) {
	t.Parallel()
	// Hint smaller than payload forces a mid-parse grow; aliases must survive it.
	var s ggen.Stream
	s.Reset(bytes.NewReader(complexPayload), make([]byte, 0, 32))
	got, err := Node{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("UnmarshalStream (tiny hint): %v", err)
	}
	if got.Name != "hello world" || got.ID != 42 {
		t.Errorf("post-grow alias corrupted: %+v", got)
	}
	if got.Children[0].Name != "child-1" {
		t.Errorf("nested string alias corrupted: %q", got.Children[0].Name)
	}
}

// HugeStringStruct exercises the stream slow path with a multi-MiB body.
//
//ggen:generate
type HugeStringStruct struct {
	Big string `json:"big"`
}

// A 2 MiB string through a tiny buffer decodes losslessly across many grow +
// compaction cycles.
func TestUnmarshalStream_SingleHugeString(t *testing.T) {
	t.Parallel()
	const size = 2 * 1024 * 1024
	huge := strings.Repeat("x", size)
	payload := []byte(`{"big":"` + huge + `"}`)

	var s ggen.Stream
	s.Reset(bytes.NewReader(payload), make([]byte, 0, 64))
	got, err := HugeStringStruct{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("UnmarshalStream: %v", err)
	}
	if len(got.Big) != size {
		t.Errorf("Big len = %d, want %d", len(got.Big), size)
	}
	if !strings.HasPrefix(got.Big, "xxxx") || !strings.HasSuffix(got.Big, "xxxx") {
		t.Errorf("Big content corrupted (prefix/suffix mismatch)")
	}
}

// SkipSpace consumes ~300 KB of whitespace across compaction cycles.
func TestUnmarshalStream_MassiveWhitespace(t *testing.T) {
	t.Parallel()
	gap := strings.Repeat("\n \t", 100_000)
	payload := []byte(`{` + gap + `"big":` + gap + `"hello"` + gap + `}`)

	var s ggen.Stream
	s.Reset(bytes.NewReader(payload), make([]byte, 0, 64))
	got, err := HugeStringStruct{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("UnmarshalStream: %v\npayload size: %d", err, len(payload))
	}
	if got.Big != "hello" {
		t.Errorf("Big = %q, want %q", got.Big, "hello")
	}
}

// SequentialStringsStruct: many string fields read back-to-back so prior
// values must survive later compactions (Stream.String copies its result).
//
//ggen:generate
type SequentialStringsStruct struct {
	A string `json:"a"`
	B string `json:"b"`
	C string `json:"c"`
	D string `json:"d"`
	E string `json:"e"`
	F string `json:"f"`
	G string `json:"g"`
	H string `json:"h"`
}

// Earlier decoded values stay correct after later compactions (a string path
// aliasing s.buf would fail).
func TestUnmarshalStream_ValuesSurviveCompaction(t *testing.T) {
	t.Parallel()
	in := []byte(`{"a":"AAAAAAAA","b":"BBBBBBBB","c":"CCCCCCCC","d":"DDDDDDDD",` +
		`"e":"EEEEEEEE","f":"FFFFFFFF","g":"GGGGGGGG","h":"HHHHHHHH"}`)
	r := &chunkReader{data: in, max: 1}
	var s ggen.Stream
	s.Reset(r, make([]byte, 0, 16))
	got, err := SequentialStringsStruct{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("UnmarshalStream: %v", err)
	}
	want := map[string]string{
		"A": got.A, "B": got.B, "C": got.C, "D": got.D,
		"E": got.E, "F": got.F, "G": got.G, "H": got.H,
	}
	for k, v := range want {
		if v != strings.Repeat(k, 8) {
			t.Errorf("field %q corrupted by compaction: got %q want %q", k, v, strings.Repeat(k, 8))
		}
	}
}

// A json.RawMessage value spanning multiple ReadMore boundaries captures
// intact.
func TestUnmarshalStream_RawJSONAcrossBoundary(t *testing.T) {
	t.Parallel()
	bigInner := `{"deeply":{"nested":{"arr":[` +
		strings.Repeat(`"item","item",`, 200) + `"end"]}}}`
	in := []byte(`{"raw1":` + bigInner + `,"raw2":null,"site":"http://x",` +
		`"big":0,"bigF":"0","bigR":"0","id":"00000000-0000-0000-0000-000000000000",` +
		`"gofrsId":"00000000-0000-0000-0000-000000000000"}`)
	var s ggen.Stream
	s.Reset(bytes.NewReader(in), make([]byte, 0, 64))
	got, err := RichTypes{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("UnmarshalStream: %v\npayload size: %d", err, len(in))
	}
	if !bytes.Equal(bytes.TrimSpace(got.Raw1), []byte(bigInner)) {
		t.Errorf("Raw1 capture corrupted across boundary\n got: %s\nwant: %s",
			got.Raw1, bigInner)
	}
}

// Inline-map keys must be cloned on insertion, else compactions corrupt them.
func TestUnmarshalStream_InlineMapKeyClone(t *testing.T) {
	t.Parallel()
	in := []byte(`{"name":"alice","kAlpha":1,"kBravoo":2,"kCharlie":3,` +
		`"kDelta11":4,"kEcho1234":5,"kFoxtrot1":6,"kGolf12345":7}`)
	r := &chunkReader{data: in, max: 1}
	var s ggen.Stream
	s.Reset(r, make([]byte, 0, 16))
	got, err := EmbedStruct{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("UnmarshalStream: %v", err)
	}
	for _, k := range []string{"kAlpha", "kBravoo", "kCharlie", "kDelta11", "kEcho1234", "kFoxtrot1", "kGolf12345"} {
		if _, ok := got.Extra[k]; !ok {
			t.Errorf("expected inline map key %q in %v", k, got.Extra)
		}
	}
}

// UnknownErrorStruct: closed schema — the unknown key must be cloned before
// going into ggen.UnknownKeyError.
//
//ggen:generate
type UnknownErrorStruct struct {
	Name string `json:"name"`
}

func TestUnmarshalStream_UnknownKeyErrorClone(t *testing.T) {
	t.Parallel()
	in := []byte(`{"name":"alice","SomeUnknownKeyHere":42}`)
	r := &chunkReader{data: in, max: 4}
	var s ggen.Stream
	s.Reset(r, make([]byte, 0, 8))
	_, err := UnknownErrorStruct{}.DecodeFromStream(&s)
	if err == nil {
		t.Fatal("expected UnknownKeyError")
	}
	var ue *ggen.UnknownKeyError
	if !errors.As(err, &ue) {
		t.Fatalf("got %T (%v), want *UnknownKeyError", err, err)
	}
	got := strings.Join(ue.Path, ".")
	if got != "SomeUnknownKeyHere" {
		t.Errorf("Path = %q, want %q (cloning broken)", got, "SomeUnknownKeyHere")
	}
}

// A 40-field object through a 1-byte reader + buf stresses dispatch-loop
// compaction and the bitmask seen-flag path.
func TestUnmarshalStream_TinyBufManyKeys(t *testing.T) {
	t.Parallel()
	var parts []string
	for i := 1; i <= 40; i++ {
		parts = append(parts, formatField(i))
	}
	in := []byte(`{` + strings.Join(parts, ",") + `}`)

	r := &chunkReader{data: in, max: 1}
	var s ggen.Stream
	s.Reset(r, make([]byte, 0, 1))
	got, err := WideStruct{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("UnmarshalStream: %v", err)
	}
	if got.F1 != "v1" || got.F20 != "v20" || got.F40 != "v40" {
		t.Errorf("wide-struct value drift: F1=%q F20=%q F40=%q",
			got.F1, got.F20, got.F40)
	}
}

// formatField produces a `"fN":"vN"` JSON key/value snippet.
func formatField(i int) string {
	n := strconv.Itoa(i)
	return `"f` + n + `":"v` + n + `"`
}

// A hint buf smaller than the value forces repeated slow-path grow +
// compaction.
func TestUnmarshalStream_HintBufSmallerThanValue(t *testing.T) {
	t.Parallel()
	for _, hint := range []int{1, 4, 16, 64, 256} {
		t.Run(strconv.Itoa(hint), func(t *testing.T) {
			t.Parallel()
			r := bytes.NewReader(complexPayload)
			var s ggen.Stream
			s.Reset(r, make([]byte, 0, hint))
			got, err := Node{}.DecodeFromStream(&s)
			if err != nil {
				t.Fatalf("hint=%d: %v", hint, err)
			}
			if got.Name != "hello world" || len(got.Children) != 2 {
				t.Errorf("hint=%d: drift %+v", hint, got)
			}
		})
	}
}

// Variable burst-then-stall chunk sizes, modeling a network reader.
func TestUnmarshalStream_PartialReadAtBoundaries(t *testing.T) {
	t.Parallel()
	r := &burstReader{data: complexPayload, sizes: []int{3, 1, 7, 1, 13, 1, 50, 1}}
	var s ggen.Stream
	s.Reset(r, make([]byte, 0, 16))
	got, err := Node{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("UnmarshalStream: %v", err)
	}
	if got.Name != "hello world" {
		t.Errorf("Name = %q", got.Name)
	}
}

// burstReader yields chunks of explicit sizes, wrapping when exhausted.
type burstReader struct {
	data  []byte
	pos   int
	sizes []int
	idx   int
}

func (b *burstReader) Read(p []byte) (int, error) {
	if b.pos >= len(b.data) {
		return 0, io.EOF
	}
	want := b.sizes[b.idx%len(b.sizes)]
	b.idx++
	if want < 1 {
		want = 1
	}
	if want > len(p) {
		want = len(p)
	}
	if b.pos+want > len(b.data) {
		want = len(b.data) - b.pos
	}
	copy(p, b.data[b.pos:b.pos+want])
	b.pos += want
	return want, nil
}

// Any payload the bytes path accepts, the stream path decodes equal — catches
// chunk-boundary-sensitive bugs.
func TestUnmarshalStream_BytesEqualsStream(t *testing.T) {
	t.Parallel()
	seeds := [][]byte{
		complexPayload,
		[]byte(`{"id":1,"name":"x"}`),
		[]byte(`{"id":1,"name":"x","tags":["a","b"],"props":{"k":"v"}}`),
		[]byte(`{"id":1,"name":"éscape","children":[{"id":2}]}`),
		[]byte(`{"id":1,"name":"x","score":1e-10,"tags":[]}`),
		[]byte(`{"id":-9223372036854775808,"name":"min","score":-1.5e308}`),
	}
	chunks := []int{1, 2, 7, 13, 31, 64, 256}
	for _, seed := range seeds {
		want, _, errBytes := Node{}.DecodeFrom(seed)
		if errBytes != nil {
			continue
		}
		for _, ch := range chunks {
			r := &chunkReader{data: seed, max: ch}
			var s ggen.Stream
			s.Reset(r, make([]byte, 0, 8))
			got, errS := Node{}.DecodeFromStream(&s)
			if errS != nil {
				t.Errorf("stream err with chunk=%d on %s: %v", ch, seed, errS)
				continue
			}
			if got.ID != want.ID || got.Name != want.Name || got.Score != want.Score {
				t.Errorf("stream/bytes drift chunk=%d:\n bytes: %+v\nstream: %+v", ch, want, got)
			}
		}
	}
}

// Same property as BytesEqualsStream, over the fuzz seed set.
func TestUnmarshalStream_AcceptedByBytes_AlsoAcceptedByStream(t *testing.T) {
	t.Parallel()
	for _, seed := range fuzzSeeds {
		want, _, errA := Node{}.DecodeFrom(seed)
		if errA != nil {
			continue
		}
		for _, ch := range []int{1, 4, 16} {
			r := &chunkReader{data: seed, max: ch}
			var s ggen.Stream
			s.Reset(r, make([]byte, 0, 4))
			got, errB := Node{}.DecodeFromStream(&s)
			if errB != nil {
				t.Errorf("seed %q ch=%d: bytes accepted but stream errored: %v", seed, ch, errB)
				continue
			}
			if want.Name != got.Name || want.ID != got.ID {
				t.Errorf("seed %q ch=%d drift", seed, ch)
			}
		}
	}
}

// The stream path threads field-name context into the ParseError like the
// bytes path.
func TestStream_parseErrorFieldName(t *testing.T) {
	t.Parallel()
	var s ggen.Stream
	s.Reset(bytes.NewReader([]byte(`{"street":123,"city":"C","zipCode":"12345"}`)), nil)
	_, err := (Address{}).DecodeFromStream(&s)
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *ggen.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T, want *ggen.ParseError", err)
	}
	if strings.Join(pe.Path, ".") != "street" {
		t.Fatalf("Path = %v; want [street]", pe.Path)
	}
	if !errors.Is(err, ggen.ErrExpectString) {
		t.Fatalf("errors.Is sentinel mismatch: %v", err)
	}
}

// decodeBothPaths runs payload through DecodeFrom and DecodeFromStream of T,
// returning both errors.
func decodeBothPaths[T interface {
	ggen.Decoder[T]
	ggen.StreamDecoder[T]
}](payload string) (bytesErr, streamErr error) {
	var zb T
	_, _, bytesErr = zb.DecodeFrom([]byte(payload))
	_, streamErr = ggen.NewStream(strings.NewReader(payload), nil).Value[T]()
	return bytesErr, streamErr
}

// A trailing comma before a container close is rejected on every container
// shape and both paths.
func TestTrailingCommaRejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload string
		run     func(string) (error, error)
	}{
		{"slice", `{"tags":["a","b",]}`, decodeBothPaths[Node]},
		{"map", `{"props":{"a":"b",}}`, decodeBothPaths[Node]},
		{"struct slice", `{"children":[{"id":1},]}`, decodeBothPaths[Node]},
		{"nested slice inner", `{"nestedInts":[[1,2,],[3]]}`, decodeBothPaths[ExtraStruct]},
		{"nested slice outer", `{"nestedInts":[[1],[2],]}`, decodeBothPaths[ExtraStruct]},
		{"tuple", `{"point":[1.5,2.5,]}`, decodeBothPaths[TupleStruct]},
		{"byte array", `{"byteArray":[1,2,]}`, decodeBothPaths[NativeTypes]},
		{"ptr slice null elem", `{"items":[null,]}`, decodeBothPaths[PtrSliceItemsStruct]},
		{"ptr map value", `{"mp":{"a":1,}}`, decodeBothPaths[NPtrContainersStruct]},
		{"top-level object", `{"id":1,}`, decodeBothPaths[Node]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			bytesErr, streamErr := c.run(c.payload)
			if bytesErr == nil {
				t.Errorf("DecodeFrom accepted %s", c.payload)
			}
			if streamErr == nil {
				t.Errorf("DecodeFromStream accepted %s", c.payload)
			}
		})
	}
}

// Input ending right after an element comma errors on both paths without
// panicking the stream path at EOF.
func TestTruncatedAfterComma(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{`{"tags":["a",`, `{"props":{"a":"b",`} {
		t.Run(payload, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("decode panicked on %q: %v", payload, r)
				}
			}()
			bytesErr, streamErr := decodeBothPaths[Node](payload)
			if bytesErr == nil {
				t.Errorf("DecodeFrom accepted truncated %q", payload)
			}
			if streamErr == nil {
				t.Errorf("DecodeFromStream accepted truncated %q", payload)
			}
		})
	}
}

// ValidationPosStruct carries one minlen rule so a violation returns a concrete
// *ggen.MinLenError.
//
//ggen:generate
type ValidationPosStruct struct {
	A string `json:"a"`
	B string `json:"b" pipe:"minlen=4"`
}

// posPayload places the failing "b" far enough past a long "a" that the stream
// buffer compacts before B is validated.
var posPayload = []byte(`{"a":"` + strings.Repeat("x", 64) + `","b":"yy"}`)

// validation.*Error.Pos is a full-payload byte offset, identical on bytes and
// stream paths despite stream compaction.
func TestValidationError_Pos(t *testing.T) {
	t.Parallel()
	// Bytes path — Pos sits just past the closing quote of "yy".
	_, _, bErr := ValidationPosStruct{}.DecodeFrom(posPayload)
	var bMin *ggen.MinLenError
	if !errors.As(bErr, &bMin) {
		t.Fatalf("bytes: want *MinLenError, got %T: %v", bErr, bErr)
	}
	if !strings.HasSuffix(string(posPayload[:bMin.Pos]), `"yy"`) {
		t.Errorf("bytes: Pos %d does not land just after the b value; prefix=%q",
			bMin.Pos, posPayload[:bMin.Pos])
	}

	// Stream path — 1-byte chunks + tiny buffer force compaction.
	r := &chunkReader{data: posPayload, max: 1}
	var s ggen.Stream
	s.Reset(r, make([]byte, 0, 16))
	_, sErr := ValidationPosStruct{}.DecodeFromStream(&s)
	var sMin *ggen.MinLenError
	if !errors.As(sErr, &sMin) {
		t.Fatalf("stream: want *MinLenError, got %T: %v", sErr, sErr)
	}
	// Same payload, same Pos.
	if sMin.Pos != bMin.Pos {
		t.Errorf("stream: Pos %d != bytes Pos %d (must be relative to full payload)", sMin.Pos, bMin.Pos)
	}
}

// NarrowInts carries fixed-width integer fields narrower than 64-bit across the
// field / map-value / slice-elem / pointer shapes, so the codegen overflow
// guard is exercised on every emit site + both decode paths.
//
//ggen:generate
type NarrowInts struct {
	I8   int8             `json:"i8"`
	I16  int16            `json:"i16"`
	I32  int32            `json:"i32"`
	U8   uint8            `json:"u8"`
	U16  uint16           `json:"u16"`
	U32  uint32           `json:"u32"`
	MapU map[string]uint8 `json:"mapU"`
	SliI []int16          `json:"sliI"`
	PtrU *uint8           `json:"ptrU"`
}

// TestNarrowIntOverflow pins the overflow guard: an out-of-range value for a
// narrow integer target must be REJECTED with ErrNumberOverflow, not silently
// truncated (uint8 ← 256 = 0, the pre-fix bug), matching encoding/json — on the
// bytes path, the stream path, and every field/map/slice/pointer emit site.
func TestNarrowIntOverflow(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		reject  bool
	}{
		{"u8_over", `{"u8":256}`, true},
		{"u8_max", `{"u8":255}`, false},
		{"i8_over", `{"i8":128}`, true},
		{"i8_under", `{"i8":-129}`, true},
		{"i8_bounds", `{"i8":-128}`, false},
		{"i8_max", `{"i8":127}`, false},
		{"u16_over", `{"u16":65536}`, true},
		{"i16_over", `{"i16":32768}`, true},
		{"i16_under", `{"i16":-32769}`, true},
		{"u32_over", `{"u32":4294967296}`, true},
		{"i32_over", `{"i32":2147483648}`, true},
		{"i32_under", `{"i32":-2147483649}`, true},
		{"map_over", `{"mapU":{"k":300}}`, true},
		{"map_ok", `{"mapU":{"k":200}}`, false},
		{"slice_over", `{"sliI":[1,32768]}`, true},
		{"slice_ok", `{"sliI":[-5,100]}`, false},
		{"ptr_over", `{"ptrU":256}`, true},
		{"ptr_ok", `{"ptrU":42}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var std NarrowInts
			stdReject := json.Unmarshal([]byte(c.payload), &std) != nil
			if stdReject != c.reject {
				t.Fatalf("encoding/json reject=%v, want %v", stdReject, c.reject)
			}
			_, _, bErr := NarrowInts{}.DecodeFrom([]byte(c.payload))
			if (bErr != nil) != c.reject {
				t.Errorf("bytes: reject=%v want %v (err=%v)", bErr != nil, c.reject, bErr)
			}
			if c.reject && bErr != nil && !errors.Is(bErr, ggen.ErrNumberOverflow) {
				t.Errorf("bytes: err=%v, want ErrNumberOverflow", bErr)
			}
			var s ggen.Stream
			s.Reset(strings.NewReader(c.payload), make([]byte, 0, 8))
			_, sErr := NarrowInts{}.DecodeFromStream(&s)
			if (sErr != nil) != c.reject {
				t.Errorf("stream: reject=%v want %v (err=%v)", sErr != nil, c.reject, sErr)
			}
			if c.reject && sErr != nil && !errors.Is(sErr, ggen.ErrNumberOverflow) {
				t.Errorf("stream: err=%v, want ErrNumberOverflow", sErr)
			}
		})
	}
}

// TestInvalidUTF8Rejected pins the decode UTF-8 contract through GENERATED
// code (inline window fast path, ggen.String fall, escape arm, stream path):
// invalid UTF-8 / unpaired surrogates in string values reject with
// ggen.ErrInvalidUTF8, matching jsonv2 (encoding/json v1 instead replaces
// with U+FFFD — an intentional divergence from v1). Valid multi-byte UTF-8
// decodes identically to jsonv2.
func TestInvalidUTF8Rejected(t *testing.T) {
	long := strings.Repeat("x", 40) // past the 32 B inline window → ggen.String fall
	cases := []struct {
		name    string
		payload string
		reject  bool
	}{
		{"short_raw_ff", "{\"street\":\"a\xffb\",\"city\":\"Y\",\"zipCode\":\"12345\"}", true},
		{"long_raw_ff", "{\"street\":\"" + long + "\xff\",\"city\":\"Y\",\"zipCode\":\"12345\"}", true},
		{"truncated_rune", "{\"street\":\"a\xe2(z\",\"city\":\"Y\",\"zipCode\":\"12345\"}", true},
		{"lone_surrogate_escape", `{"street":"\uD83D","city":"Y","zipCode":"12345"}`, true},
		{"invalid_in_escape_string", "{\"street\":\"a\\n\xff\",\"city\":\"Y\",\"zipCode\":\"12345\"}", true},
		{"valid_unicode_short", `{"street":"żółć","city":"Y","zipCode":"12345"}`, false},
		{"valid_unicode_long", `{"street":"` + long + `é😀","city":"Y","zipCode":"12345"}`, false},
		{"valid_escaped_pair", `{"street":"a😀b","city":"Y","zipCode":"12345"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var v2 Address
			v2Reject := jsonv2.Unmarshal([]byte(c.payload), &v2) != nil
			if v2Reject != c.reject {
				t.Fatalf("jsonv2 reject=%v, want %v", v2Reject, c.reject)
			}
			got, _, bErr := Address{}.DecodeFrom([]byte(c.payload))
			if (bErr != nil) != c.reject {
				t.Errorf("bytes: reject=%v want %v (err=%v)", bErr != nil, c.reject, bErr)
			}
			if c.reject && bErr != nil && !errors.Is(bErr, ggen.ErrInvalidUTF8) {
				t.Errorf("bytes: err=%v, want ErrInvalidUTF8", bErr)
			}
			if !c.reject && got.Street != v2.Street {
				t.Errorf("bytes: street %q != jsonv2 %q", got.Street, v2.Street)
			}
			for _, bufCap := range []int{8, 512} {
				var s ggen.Stream
				s.Reset(&chunkReader{data: []byte(c.payload), max: 3}, make([]byte, 0, bufCap))
				sGot, sErr := Address{}.DecodeFromStream(&s)
				if (sErr != nil) != c.reject {
					t.Errorf("stream cap=%d: reject=%v want %v (err=%v)", bufCap, sErr != nil, c.reject, sErr)
				}
				if c.reject && sErr != nil && !errors.Is(sErr, ggen.ErrInvalidUTF8) {
					t.Errorf("stream cap=%d: err=%v, want ErrInvalidUTF8", bufCap, sErr)
				}
				if !c.reject && sGot.Street != v2.Street {
					t.Errorf("stream cap=%d: street %q != jsonv2 %q", bufCap, sGot.Street, v2.Street)
				}
			}
		})
	}
}

// TestRawCaptureInvalidUTF8: captured raw spans (json.RawMessage /
// jsontext.Value) reject invalid UTF-8 BYTES with ggen.ErrInvalidUTF8 (jsonv2
// rejects those too), bytes + stream. Residual divergence: unpaired \uXXXX
// surrogate ESCAPES inside a raw span are ASCII text and pass (jsonv2
// escape-parses raw strings and rejects) — see backlog.
func TestRawCaptureInvalidUTF8(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		reject  bool
	}{
		{"raw_string_invalid", "{\"raw1\":\"a\xffb\"}", true},
		{"raw_nested_invalid", "{\"raw1\":{\"k\":\"a\xffb\"}}", true},
		{"raw2_invalid", "{\"raw2\":[\"a\xe2(z\"]}", true},
		{"raw_clean", `{"raw1":{"k":[1,"ok",null]}}`, false},
		{"raw_valid_unicode", "{\"raw1\":\"żółć\"}", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var v2 RichTypes
			if v2Reject := jsonv2.Unmarshal([]byte(c.payload), &v2) != nil; v2Reject != c.reject {
				t.Fatalf("jsonv2 reject=%v, want %v", v2Reject, c.reject)
			}
			_, _, bErr := RichTypes{}.DecodeFrom([]byte(c.payload))
			if (bErr != nil) != c.reject {
				t.Errorf("bytes: reject=%v want %v (err=%v)", bErr != nil, c.reject, bErr)
			}
			if c.reject && bErr != nil && !errors.Is(bErr, ggen.ErrInvalidUTF8) {
				t.Errorf("bytes: err=%v, want ErrInvalidUTF8", bErr)
			}
			var s ggen.Stream
			s.Reset(&chunkReader{data: []byte(c.payload), max: 3}, make([]byte, 0, 8))
			_, sErr := RichTypes{}.DecodeFromStream(&s)
			if (sErr != nil) != c.reject {
				t.Errorf("stream: reject=%v want %v (err=%v)", sErr != nil, c.reject, sErr)
			}
			if c.reject && sErr != nil && !errors.Is(sErr, ggen.ErrInvalidUTF8) {
				t.Errorf("stream: err=%v, want ErrInvalidUTF8", sErr)
			}
		})
	}
}

// TestMaxDepthNoCrash: deeply-nested payloads that formerly overflowed the
// goroutine stack (fatal, unrecoverable) now return ggen.ErrMaxDepth cleanly
// through generated code — self-referential struct (Node.Children), any field,
// ignoreunknown skip, and RawMessage capture, bytes + stream.
func TestMaxDepthNoCrash(t *testing.T) {
	const n = 2_000_000 // well past MaxDepth and past the old ~2.5MB crash point
	arr := strings.Repeat("[", n) + strings.Repeat("]", n)
	cases := []struct {
		name    string
		payload string
		decode  func([]byte) error
	}{
		{"recursive_struct", strings.Repeat(`{"children":[`, n) + strings.Repeat(`]}`, n),
			func(d []byte) error { _, _, err := Node{}.DecodeFrom(d); return err }},
		{"any_field", `{"data":` + arr + `}`,
			func(d []byte) error { _, _, err := AnyHolder{}.DecodeFrom(d); return err }},
		{"ignoreunknown_skip", `{"zz":` + arr + `}`,
			func(d []byte) error { _, _, err := IgnoreUnknownStruct{}.DecodeFrom(d); return err }},
		{"raw_capture", `{"raw":` + arr + `}`,
			func(d []byte) error { _, _, err := RawHolder{}.DecodeFrom(d); return err }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := c.decode([]byte(c.payload)); !errors.Is(err, ggen.ErrMaxDepth) {
				t.Errorf("bytes: want ErrMaxDepth, got %v", err)
			}
		})
	}
	// Stream path: recursive struct + skip, deep enough to exceed the cap.
	rec := strings.Repeat(`{"children":[`, n) + strings.Repeat(`]}`, n)
	var s ggen.Stream
	s.Reset(strings.NewReader(rec), make([]byte, 0, 4096))
	if _, err := (Node{}).DecodeFromStream(&s); !errors.Is(err, ggen.ErrMaxDepth) {
		t.Errorf("stream recursive: want ErrMaxDepth, got %v", err)
	}
	s.Reset(strings.NewReader(`{"zz":`+arr+`}`), make([]byte, 0, 4096))
	if _, err := (IgnoreUnknownStruct{}).DecodeFromStream(&s); !errors.Is(err, ggen.ErrMaxDepth) {
		t.Errorf("stream skip: want ErrMaxDepth, got %v", err)
	}
}

//ggen:generate
type AnyHolder struct {
	Data any `json:"data"`
}

//ggen:generate
type RawHolder struct {
	Raw json.RawMessage `json:"raw"`
}

// NumGrammar carries one field per number decode path: inline int/uint
// emitters, float64, and json.Number.
//
//ggen:generate
type NumGrammar struct {
	I int         `json:"i"`
	U uint        `json:"u"`
	F float64     `json:"f"`
	N json.Number `json:"n"`
}

// TestNumberGrammarStrict pins the VALUE decoders to the RFC 8259 number
// grammar, matching jsonv2. These forms are Go-number-isms that
// strconv.ParseFloat accepts but JSON forbids; ggen used to accept them while
// its own SKIP path (skipNumber, used for RawMessage/ignoreunknown) already
// rejected them — so decoding and skipping disagreed on the same bytes.
func TestNumberGrammarStrict(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		reject  bool
	}{
		{"int_leading_zero", `{"i":01}`, true},
		{"int_multi_leading_zero", `{"i":0007}`, true},
		{"int_neg_leading_zero", `{"i":-01}`, true},
		{"int_plain_zero", `{"i":0}`, false},
		{"int_neg_zero", `{"i":-0}`, false},
		{"int_plain", `{"i":123}`, false},
		{"uint_leading_zero", `{"u":01}`, true},
		{"uint_plain_zero", `{"u":0}`, false},
		{"float_bare_dot", `{"f":.5}`, true},
		{"float_trailing_dot", `{"f":1.}`, true},
		{"float_leading_plus", `{"f":+1}`, true},
		{"float_leading_zero", `{"f":01.5}`, true},
		{"float_exp_no_digits", `{"f":1e}`, true},
		{"float_plain", `{"f":1.5}`, false},
		{"float_exp", `{"f":1.5e-3}`, false},
		{"float_zero_frac", `{"f":0.5}`, false},
		{"number_leading_zero", `{"n":012}`, true},
		{"number_bare_dot", `{"n":.5}`, true},
		{"number_plain", `{"n":12.5}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var v2 NumGrammar
			if v2Reject := jsonv2.Unmarshal([]byte(c.payload), &v2) != nil; v2Reject != c.reject {
				t.Fatalf("jsonv2 reject=%v, want %v", v2Reject, c.reject)
			}
			got, _, bErr := NumGrammar{}.DecodeFrom([]byte(c.payload))
			if (bErr != nil) != c.reject {
				t.Errorf("bytes: reject=%v want %v (err=%v)", bErr != nil, c.reject, bErr)
			}
			if !c.reject && got != v2 {
				t.Errorf("bytes: %+v != jsonv2 %+v", got, v2)
			}
			for _, bufCap := range []int{8, 512} {
				var s ggen.Stream
				s.Reset(&chunkReader{data: []byte(c.payload), max: 3}, make([]byte, 0, bufCap))
				sGot, sErr := NumGrammar{}.DecodeFromStream(&s)
				if (sErr != nil) != c.reject {
					t.Errorf("stream cap=%d: reject=%v want %v (err=%v)", bufCap, sErr != nil, c.reject, sErr)
				}
				if !c.reject && sErr == nil && sGot != v2 {
					t.Errorf("stream cap=%d: %+v != jsonv2 %+v", bufCap, sGot, v2)
				}
			}
			// Skip path (RawMessage capture) must agree with the value path —
			// this asymmetry is exactly what the fix removes.
			raw := `{"raw":` + c.payload[strings.Index(c.payload, ":")+1:]
			_, _, rErr := RawHolder{}.DecodeFrom([]byte(raw))
			if (rErr != nil) != c.reject {
				t.Errorf("skip/raw: reject=%v want %v (err=%v)", rErr != nil, c.reject, rErr)
			}
		})
	}
}

// NarrowFloats pins the float sibling of the narrow-int guard: float32
// positions used to bare-cast float64 scans, so 1e39 decoded to +Inf with a
// nil error while v1 and jsonv2 both reject out-of-range float32.
//
//ggen:generate
type NarrowFloats struct {
	F  float32            `json:"f"`
	Fs []float32          `json:"fs"`
	Fm map[string]float32 `json:"fm"`
	Fp *float32           `json:"fp"`
	Fq float32            `json:"fq,string"`
}

func TestNarrowFloatOverflow(t *testing.T) {
	t.Parallel()
	bad := []string{
		`{"f":1e39}`, `{"f":-1e39}`, `{"fs":[1,1e39]}`, `{"fm":{"k":1e39}}`, `{"fp":1e39}`, `{"fq":"1e39"}`,
	}
	for _, in := range bad {
		if _, _, err := (NarrowFloats{}).DecodeFrom([]byte(in)); !errors.Is(err, ggen.ErrNumberOverflow) {
			t.Errorf("bytes %s: got %v, want ErrNumberOverflow", in, err)
		}
		var s ggen.Stream
		s.Reset(&chunkReader{data: []byte(in), max: 1}, nil)
		if _, err := (NarrowFloats{}).DecodeFromStream(&s); !errors.Is(err, ggen.ErrNumberOverflow) {
			t.Errorf("stream %s: got %v, want ErrNumberOverflow", in, err)
		}
		var std NarrowFloats
		if json.Unmarshal([]byte(in), &std) == nil {
			t.Errorf("stdlib accepted %s — differential premise broken", in)
		}
	}
	// The float32 rounding boundary: values that round DOWN to MaxFloat32
	// stay accepted (stdlib does the same — the reject line is "converts to
	// Inf", not MaxFloat32).
	got, _, err := (NarrowFloats{}).DecodeFrom([]byte(`{"f":3.4028235e38}`))
	if err != nil || got.F != math.MaxFloat32 {
		t.Errorf("boundary: got %v, %v", got.F, err)
	}
}

// TestNarrowFloat_RoundsDecimalOnce pins single rounding at every float32
// site (field, slice, map, pointer, `,string`; bytes + stream). Narrowing a
// float64 scan rounds the decimal twice: the first three tokens are the
// shortest float64 forms of float32 rounding midpoints — what json.Marshal of
// such a float64 emits — and landed one ulp off; the last sits just below
// the overflow midpoint and was rejected as an overflow. Both stdlibs parse
// at 32 bits.
func TestNarrowFloat_RoundsDecimalOnce(t *testing.T) {
	t.Parallel()
	pick := func(v NarrowFloats) float32 {
		switch {
		case v.Fp != nil:
			return *v.Fp
		case len(v.Fs) == 1:
			return v.Fs[0]
		case len(v.Fm) == 1:
			return v.Fm["k"]
		case v.Fq != 0:
			return v.Fq
		}
		return v.F
	}
	for _, tok := range []string{"1.0000000596046448", "1.0000001788139343", "1.0000002980232239", "3.4028235677973366e38"} {
		for _, payload := range []string{
			`{"f":` + tok + `}`, `{"fs":[` + tok + `]}`, `{"fm":{"k":` + tok + `}}`, `{"fp":` + tok + `}`, `{"fq":"` + tok + `"}`,
		} {
			var v1, v2 NarrowFloats
			if err := json.Unmarshal([]byte(payload), &v1); err != nil {
				t.Fatalf("encoding/json rejects %s: %v", payload, err)
			}
			if err := jsonv2.Unmarshal([]byte(payload), &v2); err != nil {
				t.Fatalf("jsonv2 rejects %s: %v", payload, err)
			}
			want := pick(v1)
			if pick(v2) != want {
				t.Fatalf("oracles disagree on %s: %v vs %v", payload, want, pick(v2))
			}
			got, _, err := (NarrowFloats{}).DecodeFrom([]byte(payload))
			if err != nil || pick(got) != want {
				t.Errorf("bytes %s: got %v (%v), want %v", payload, pick(got), err, want)
			}
			var s ggen.Stream
			s.Reset(&chunkReader{data: []byte(payload), max: 1}, nil)
			sgot, err := (NarrowFloats{}).DecodeFromStream(&s)
			if err != nil || pick(sgot) != want {
				t.Errorf("stream %s: got %v (%v), want %v", payload, pick(sgot), err, want)
			}
		}
	}
}

// Round-5 pins: bytes-path slice structural errors wrap in *ggen.ParseError
// (path + pos), and scan-primitive error positions surface end-to-end,
// bytes/stream agreeing on the identical payload.

func nodeStreamErr(t *testing.T, payload []byte) error {
	t.Helper()
	var s ggen.Stream
	s.Reset(bytes.NewReader(payload), make([]byte, 0, len(payload)))
	_, err := Node{}.DecodeFromStream(&s)
	return err
}

func TestParseError_SliceStructural(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload string
	}{
		{"wrong_shape", `{"tags":{}}`},
		{"truncated_after_comma", `{"tags":["a",`},
		{"close_after_comma", `{"tags":["a",]}`},
		{"unclosed", `{"tags":["a"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			payload := []byte(c.payload)
			_, _, berr := Node{}.DecodeFrom(payload)
			if berr == nil {
				t.Fatal("bytes: expected error")
			}
			var pe *ggen.ParseError
			if !errors.As(berr, &pe) {
				t.Fatalf("bytes: got %T %v, want *ggen.ParseError", berr, berr)
			}
			if len(pe.Path) == 0 || pe.Path[0] != "tags" {
				t.Errorf("bytes: Path = %v, want [tags ...]", pe.Path)
			}
			if pe.Pos < 8 {
				t.Errorf("bytes: Pos = %d, want >= 8 (inside the value)", pe.Pos)
			}
			serr := nodeStreamErr(t, payload)
			var spe *ggen.ParseError
			if !errors.As(serr, &spe) {
				t.Fatalf("stream: got %T %v, want *ggen.ParseError", serr, serr)
			}
			if len(spe.Path) == 0 || spe.Path[0] != "tags" {
				t.Errorf("stream: Path = %v, want [tags ...]", spe.Path)
			}
		})
	}
}

// The pipe: variant dispatch heads (no matching shape, truncated value,
// malformed null arm) wrap like every other structural guard.
func TestParseError_VariantDispatch(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ name, payload string }{
		{"no_matching_shape", `{"count":true}`},
		{"truncated_at_value", `{"count":`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := LooseThing{}.DecodeFrom([]byte(c.payload))
			var pe *ggen.ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("got %T %v, want *ggen.ParseError", err, err)
			}
			if len(pe.Path) == 0 || pe.Path[0] != "count" {
				t.Errorf("Path = %v, want [count]", pe.Path)
			}
			if pe.Pos < 9 {
				t.Errorf("Pos = %d, want >= 9 (at the value)", pe.Pos)
			}
		})
	}
}

func TestParseError_ScanPrimitivePos(t *testing.T) {
	t.Parallel()
	// ggen.String fails inside the value; runtime now returns the error
	// position, which the emitted NewParseErr site stamps unmodified.
	payload := []byte(`{"name":"ab` + "\x01" + `c"}`)
	_, _, berr := Node{}.DecodeFrom(payload)
	var bpe *ggen.ParseError
	if !errors.As(berr, &bpe) {
		t.Fatalf("bytes: got %T %v, want *ggen.ParseError", berr, berr)
	}
	if bpe.Pos < 8 {
		t.Errorf("bytes: Pos = %d, want >= 8 (error inside the string)", bpe.Pos)
	}
	serr := nodeStreamErr(t, payload)
	var spe *ggen.ParseError
	if !errors.As(serr, &spe) {
		t.Fatalf("stream: got %T %v, want *ggen.ParseError", serr, serr)
	}
	// Bytes stamps the string-content start (ggen.String's error pos), the
	// stream the value start — both real positions, never the old 0.
	if spe.Pos < 8 {
		t.Errorf("stream: Pos = %d, want >= 8 (at the value)", spe.Pos)
	}
}

// Stream ParseError positions must be full-payload offsets (s.Offset()), not
// the buffer-relative s.Pos: once a compacting refill slides the window the
// two diverge, so a small buffer + chunked reader used to report a position
// near 0 for a failure thousands of bytes in — and the value changed with the
// chunk size. A 16-byte payload cannot catch it (the window never slides).
func TestParseError_StreamPosIsPayloadOffset(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	sb.WriteString(`{"tags":[`)
	for i := range 500 {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`"aaaaaaaaaaaaaaaa"`)
	}
	sb.WriteString(`],"id":true}`) // id is an int: fails far into the payload
	payload := []byte(sb.String())
	wantAtLeast := bytes.Index(payload, []byte(`"id":true`))

	_, _, berr := Node{}.DecodeFrom(payload)
	var bpe *ggen.ParseError
	if !errors.As(berr, &bpe) {
		t.Fatalf("bytes: got %T %v, want *ggen.ParseError", berr, berr)
	}
	if bpe.Pos < wantAtLeast {
		t.Errorf("bytes: Pos = %d, want >= %d", bpe.Pos, wantAtLeast)
	}
	for _, chunk := range []int{1, 7, 64, 512} {
		var s ggen.Stream
		s.Reset(&chunkReader{data: payload, max: chunk}, make([]byte, 0, 64))
		_, serr := Node{}.DecodeFromStream(&s)
		var spe *ggen.ParseError
		if !errors.As(serr, &spe) {
			t.Fatalf("stream chunk=%d: got %T %v, want *ggen.ParseError", chunk, serr, serr)
		}
		if spe.Pos < wantAtLeast {
			t.Errorf("stream chunk=%d: Pos = %d, want >= %d (payload offset, not buffer-relative)",
				chunk, spe.Pos, wantAtLeast)
		}
	}
}

// Round-9: generated value-head refills map a DRAINED reader to the same
// grammar sentinel the bytes path reports for the truncation (ggen.NotEOF in
// streamReadMore) — raw io.ErrUnexpectedEOF used to leak at null-peek/map/
// slice/variant heads. Sweeps every prefix cut of a null-bearing payload.
func TestTruncationSentinelParity(t *testing.T) {
	t.Parallel()
	payload := `{"id":1,"tags":null,"props":null,"children":null,"name":"x"}`
	for cut := 1; cut < len(payload); cut++ {
		bytesErr, streamErr := decodeBothPaths[Node](payload[:cut])
		if bytesErr == nil || streamErr == nil {
			t.Fatalf("cut %d accepted: bytes=%v stream=%v", cut, bytesErr, streamErr)
		}
		if errors.Is(streamErr, io.ErrUnexpectedEOF) && !errors.Is(bytesErr, io.ErrUnexpectedEOF) {
			t.Errorf("cut %d %q: stream leaks raw EOF (%v), bytes says %v",
				cut, payload[:cut], streamErr, bytesErr)
		}
	}
}

// decodeBothChunked is decodeBothPaths with the stream fed chunk bytes per
// Read through a small window, so the window compacts mid-value.
func decodeBothChunked[T interface {
	ggen.Decoder[T]
	ggen.StreamDecoder[T]
}](payload string, chunk int) (bytesErr, streamErr error) {
	var zb T
	_, _, bytesErr = zb.DecodeFrom([]byte(payload))
	var s ggen.Stream
	s.Reset(&chunkReader{data: []byte(payload), max: chunk}, make([]byte, 0, 64))
	_, streamErr = s.Value[T]()
	return bytesErr, streamErr
}

// Stream scan primitives rebase s.Pos on every error return, so a compacting
// refill cannot leave a stale cursor behind, and the rebased cursor is the
// bytes-path error position (where scanning stopped). The user-visible
// contract that buys: ParseError.Pos and the sentinel are the SAME on both
// paths and at every chunk size. Integer overflow used to report 236/239/218
// at chunk 1/7/64 for one payload; a malformed number under an ignoreunknown
// key stamped a position past the document; a RawMessage span malformed deep
// inside reported the span's first byte on the stream only.
func TestParseError_StreamPosChunkInvariant(t *testing.T) {
	t.Parallel()
	pad := `{"tags":["` + strings.Repeat("a", 200) + `"],`
	rawPad := `{"raw":{"nested":[1,2,3],"s":"` + strings.Repeat("b", 200) + `"`
	cases := []struct {
		name, payload string
		run           func(string, int) (error, error)
	}{
		{"ctrl_in_string", `{"name":"` + strings.Repeat("a", 200) + "\x01" + `x"}`, decodeBothChunked[Node]},
		{"int_bad_digit", pad + `"id":12x3}`, decodeBothChunked[Node]},
		{"int_leading_zero", pad + `"id":0123}`, decodeBothChunked[Node]},
		{"int_overflow", pad + `"id":99999999999999999999}`, decodeBothChunked[Node]},
		{"int_sign_only", pad + `"id":-}`, decodeBothChunked[Node]},
		{"float_bad", pad + `"score":1.2.3}`, decodeBothChunked[Node]},
		{"float_trail_dot", pad + `"score":1.}`, decodeBothChunked[Node]},
		{"bad_key", pad + `5:1}`, decodeBothChunked[Node]},
		{"unterminated_str", pad + `"name":"abc`, decodeBothChunked[Node]},
		{"ctrl_in_open_tail", pad + "\"name\":\"abc\x01", decodeBothChunked[Node]},
		{"truncated_escape", pad + `"name":"a\u12`, decodeBothChunked[Node]},
		{"bad_literal", pad + `"id":tru}`, decodeBothChunked[Node]},
		{"bad_bool", pad + `"active":tru}`, decodeBothChunked[Node]},
		{"bad_bool_byte", pad + `"active":trux}`, decodeBothChunked[Node]},
		{"ignored_key_bad_number", `{"name":"x","zz":1.}`, decodeBothChunked[IgnoreUnknownStruct]},
		{"ignored_key_bad_literal", pad + `"zz":nulx}`, decodeBothChunked[IgnoreUnknownStruct]},
		{"raw_span_malformed", rawPad + `, "x":}}`, decodeBothChunked[RawOnly]},
		{"raw_span_truncated", rawPad, decodeBothChunked[RawOnly]},
		{"embed_raw_span", `{"name":"a","k":{"x":[1,2,]}}`, decodeBothChunked[EmbedRawStruct]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, chunk := range []int{1, 7, 64, 512, 4096} {
				bytesErr, streamErr := tc.run(tc.payload, chunk)
				var bpe, spe *ggen.ParseError
				if !errors.As(bytesErr, &bpe) {
					t.Fatalf("bytes: got %T %v, want *ggen.ParseError", bytesErr, bytesErr)
				}
				if !errors.As(streamErr, &spe) {
					t.Fatalf("stream chunk=%d: got %T %v, want *ggen.ParseError", chunk, streamErr, streamErr)
				}
				if !errors.Is(streamErr, bpe.Err) {
					t.Errorf("stream chunk=%d: sentinel %v, bytes says %v", chunk, spe.Err, bpe.Err)
				}
				if spe.Pos != bpe.Pos || spe.Pos > len(tc.payload) {
					t.Errorf("stream chunk=%d: Pos = %d, bytes %d (payload len %d)", chunk, spe.Pos, bpe.Pos, len(tc.payload))
				}
			}
		})
	}
}

// Round-9: `format:array` byte elements reject >255 with ErrNumberOverflow
// (encoding/json v1 and jsonv2 both reject) instead of silently wrapping.
func TestFormatArray_ByteOverflow(t *testing.T) {
	t.Parallel()
	bytesErr, streamErr := decodeBothPaths[NativeTypes](`{"byteArray":[300]}`)
	for path, err := range map[string]error{"bytes": bytesErr, "stream": streamErr} {
		if !errors.Is(err, ggen.ErrNumberOverflow) {
			t.Errorf("%s: got %v, want ErrNumberOverflow", path, err)
		}
	}
}

// decodeBothPathsChunked is decodeBothPaths with the stream fed chunk bytes
// per Read through a small window.
func decodeBothPathsChunked[T interface {
	ggen.Decoder[T]
	ggen.StreamDecoder[T]
}](payload string, chunk int) (bytesErr, streamErr error) {
	var zb T
	_, _, bytesErr = zb.DecodeFrom([]byte(payload))
	var s ggen.Stream
	s.Reset(&chunkReader{data: []byte(payload), max: chunk}, make([]byte, 0, 16))
	_, streamErr = s.Value[T]()
	return bytesErr, streamErr
}

// A control byte inside a string whose closing quote never arrives is a
// final malformation, not a truncation: both paths report ErrBadString at
// the same offset. The bytes path used to say ErrUnterminated at the end of
// input while the stream said ErrBadString at the span start.
func TestParseError_CtrlInOpenTailParity(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{"{\"name\":\"ab\x01}", "{\"na\x01:1}", "{\"props\":{\"a\":\"b\x01}}", "{\"tags\":[\"a\x01]}"} {
		for _, chunk := range []int{1, 3, 64} {
			berr, serr := decodeBothPathsChunked[Node](payload, chunk)
			var bpe, spe *ggen.ParseError
			if !errors.As(berr, &bpe) || !errors.As(serr, &spe) {
				t.Fatalf("%q chunk=%d: bytes=%v stream=%v, want ParseErrors", payload, chunk, berr, serr)
			}
			if bpe.Err != ggen.ErrBadString {
				t.Errorf("%q: bytes sentinel %v, want ErrBadString", payload, bpe.Err)
			}
			if spe.Err != bpe.Err || spe.Pos != bpe.Pos {
				t.Errorf("%q chunk=%d: stream (%v pos=%d), bytes (%v pos=%d)",
					payload, chunk, spe.Err, spe.Pos, bpe.Err, bpe.Pos)
			}
		}
	}
}

// R10BoolShapes carries a bool at every generated Bool emit site: field,
// slice element, array element, map value.
//
//ggen:generate
type R10BoolShapes struct {
	Flag  bool            `json:"flag"`
	Flags []bool          `json:"flags"`
	Pair  [2]bool         `json:"pair"`
	ByKey map[string]bool `json:"byKey"`
}

// Every generated Bool site stamps the give-up byte — the first byte that
// breaks the literal, or the end of a truncated one — on both paths and at
// every chunk size (jsonv2's offset). ggen.Bool itself reports the literal
// start; the emitted error branch derives the position with ggen.BoolEnd,
// and Stream.Bool leaves Pos there.
func TestParseError_BoolGiveUpPos(t *testing.T) {
	t.Parallel()
	shapes := func(p string, c int) (error, error) { return decodeBothPathsChunked[R10BoolShapes](p, c) }
	cases := []struct {
		name    string
		payload string
		want    int
		run     func(string, int) (error, error)
	}{
		{"field_truncated", `{"flag":tru}`, 11, shapes},
		{"field_bad_byte", `{"flag":trux}`, 11, shapes},
		{"field_false", `{"flag":fals}`, 12, shapes},
		{"slice_elem", `{"flags":[true,fals]}`, 19, shapes},
		{"array_elem", `{"pair":[tru,true]}`, 12, shapes},
		{"map_value", `{"byKey":{"a":trux}}`, 17, shapes},
		{"any_field", `{"name":"a","body":falsy}`, 23,
			func(p string, c int) (error, error) { return decodeBothPathsChunked[AnyStruct](p, c) }},
		{"alias", `trux`, 3,
			func(p string, c int) (error, error) { return decodeBothPathsChunked[AliasBool](p, c) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, chunk := range []int{1, 3, 64} {
				berr, serr := tc.run(tc.payload, chunk)
				for path, err := range map[string]error{"bytes": berr, "stream": serr} {
					var pe *ggen.ParseError
					if !errors.As(err, &pe) || pe.Err != ggen.ErrBadBool {
						t.Fatalf("%s chunk=%d: got %v, want an ErrBadBool ParseError", path, chunk, err)
					}
					if pe.Pos != tc.want {
						t.Errorf("%s chunk=%d: Pos = %d, want %d", path, chunk, pe.Pos, tc.want)
					}
				}
			}
		})
	}
}

// The value-head refill sentinel follows the WIRE shape, not the field's Go
// kind: a pointer chain's leaf, sql.Null*'s inner, the quote a `,string`
// value opens with, the bracket of a format:array []byte.
//
//ggen:generate
type R10HeadShapes struct {
	PP **int          `json:"pp"`
	PG *sql.Null[int] `json:"pg"`
	BA []byte         `json:"ba,format:array"`
	PS *int           `json:"ps,string"`
}

//ggen:generate nullzero
type R10HeadShapesNZ struct {
	IS int `json:"is,string"`
}

func TestTruncationSentinelParity_WireShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		payload string
		run     func(string) (error, error)
	}{
		{`{"pp":`, decodeBothPaths[R10HeadShapes]},
		{`{"pg":`, decodeBothPaths[R10HeadShapes]},
		{`{"ba":`, decodeBothPaths[R10HeadShapes]},
		{`{"ps":`, decodeBothPaths[R10HeadShapes]},
		{`{"is":`, decodeBothPaths[R10HeadShapesNZ]},
	}
	for _, c := range cases {
		bytesErr, streamErr := c.run(c.payload)
		var bpe, spe *ggen.ParseError
		if !errors.As(bytesErr, &bpe) || !errors.As(streamErr, &spe) {
			t.Fatalf("%q: bytes=%v stream=%v", c.payload, bytesErr, streamErr)
		}
		if bpe.Err != spe.Err {
			t.Errorf("%q: bytes sentinel %v, stream sentinel %v", c.payload, bpe.Err, spe.Err)
		}
	}
}

// An `n` that does not spell `null` is ErrBadLiteral at every null-accepting
// kind on both paths (jsonv2 reports a literal error there too), including
// input that runs out mid-literal.
func TestNullPeekSentinelParity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		run  func(string) (error, error)
		in   string
	}{
		{"slice", decodeBothPaths[Node], `{"tags":nulx}`},
		{"slice_short", decodeBothPaths[Node], `{"tags":n}`},
		{"slice_truncated", decodeBothPaths[Node], `{"tags":nul`},
		{"map", decodeBothPaths[Node], `{"props":nul}`},
		{"struct_slice", decodeBothPaths[Node], `{"children":nx}`},
		{"ptr_string", decodeBothPaths[PointerStruct], `{"name":nope}`},
		{"ptr_bool", decodeBothPaths[PointerStruct], `{"enabled":nay}`},
		{"sqlnull_int", decodeBothPaths[SQLNullStruct], `{"i":nan}`},
		{"nullzero_int", decodeBothPaths[NullZeroTags], `{"nzInt":nix}`},
		{"bytes", decodeBothPaths[NativeTypes], `{"blob":nb}`},
	}
	for _, c := range cases {
		bytesErr, streamErr := c.run(c.in)
		if !errors.Is(bytesErr, ggen.ErrBadLiteral) {
			t.Errorf("%s bytes: %v", c.name, bytesErr)
		}
		if !errors.Is(streamErr, ggen.ErrBadLiteral) {
			t.Errorf("%s stream: %v", c.name, streamErr)
		}
	}
}
