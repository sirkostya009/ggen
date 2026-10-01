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

// chunkReader returns at most max bytes of the payload per Read. This
// stresses the path where Stream.ReadMore grows the buffer on each call.
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
	// A buffer smaller than the payload forces the Stream to grow it in the
	// middle of the parse. Decoded values must survive the grow.
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

// HugeStringStruct exercises the stream's slow string path with a string of
// several MiB.
//
//ggen:generate
type HugeStringStruct struct {
	Big string `json:"big"`
}

// A 2 MiB string read through a tiny buffer decodes without loss across many
// cycles of growing and compacting the buffer. Compacting means moving the
// unread bytes to the front of the buffer before reading more.
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

// SkipSpace consumes about 300 KB of whitespace across many compaction
// cycles.
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

// SequentialStringsStruct has many string fields read one after another, so
// earlier values must survive later compactions. Stream.String copies its
// result for this reason.
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

// Earlier decoded values stay correct after later compactions. A string that
// aliased s.buf (pointed into it without a copy) would fail this test.
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

// A json.RawMessage value that spans several ReadMore refills is captured
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

// Inline-map keys must be copied when they are inserted, or later compactions
// corrupt them.
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

// UnknownErrorStruct rejects unknown keys. The unknown key must be copied
// before it goes into ggen.UnknownKeyError.
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

// A 40-field object read 1 byte at a time through a small buffer. This
// stresses compaction in the key dispatch loop and the code that tracks seen
// fields as bits in an integer.
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

// formatField returns a `"fN":"vN"` JSON key/value pair.
func formatField(i int) string {
	n := strconv.Itoa(i)
	return `"f` + n + `":"v` + n + `"`
}

// A buffer smaller than the value forces the slow path to grow and compact
// the buffer many times.
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

// Chunk sizes vary between bursts and single bytes, like a network reader.
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

// burstReader returns chunks of the given sizes, starting over from the first
// size when the list runs out.
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

// Any payload the bytes path accepts, the stream path decodes to an equal
// value. This catches bugs that depend on where a chunk boundary falls.
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

// The stream path puts the field name into the ParseError, like the bytes
// path.
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

// decodeBothPaths runs payload through T's DecodeFrom and DecodeFromStream and
// returns both errors.
func decodeBothPaths[T interface {
	ggen.Decoder[T]
	ggen.StreamDecoder[T]
}](payload string) (bytesErr, streamErr error) {
	var zb T
	_, _, bytesErr = zb.DecodeFrom([]byte(payload))
	_, streamErr = ggen.NewStream(strings.NewReader(payload), nil).Value[T]()
	return bytesErr, streamErr
}

// A trailing comma before `]` or `}` is rejected for every container shape on
// both paths.
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

// Input that ends right after an element comma fails on both paths. The
// stream path must not panic at EOF.
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

// ValidationPosStruct has one minlen rule, so a violation returns a
// *ggen.MinLenError.
//
//ggen:generate
type ValidationPosStruct struct {
	A string `json:"a"`
	B string `json:"b" pipe:"minlen=4"`
}

// posPayload puts the failing "b" far enough after a long "a" that the stream
// buffer compacts before B is validated.
var posPayload = []byte(`{"a":"` + strings.Repeat("x", 64) + `","b":"yy"}`)

// A validation error's Pos is a byte offset into the full payload. It is the
// same on the bytes and stream paths, even though the stream compacts its
// buffer.
func TestValidationError_Pos(t *testing.T) {
	t.Parallel()
	// Bytes path: Pos is just past the closing quote of "yy".
	_, _, bErr := ValidationPosStruct{}.DecodeFrom(posPayload)
	var bMin *ggen.MinLenError
	if !errors.As(bErr, &bMin) {
		t.Fatalf("bytes: want *MinLenError, got %T: %v", bErr, bErr)
	}
	if !strings.HasSuffix(string(posPayload[:bMin.Pos]), `"yy"`) {
		t.Errorf("bytes: Pos %d does not land just after the b value; prefix=%q",
			bMin.Pos, posPayload[:bMin.Pos])
	}

	// Stream path: 1-byte chunks and a tiny buffer force compaction.
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

// NarrowInts has integer fields narrower than 64 bits as plain fields, map
// values, slice elements and pointers. This way the generated overflow check
// runs at every kind of generated site and on both decode paths.
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

// TestNarrowIntOverflow pins the overflow check. A value out of range for a
// narrow integer type must be rejected with ErrNumberOverflow, not silently
// truncated (256 into a uint8 would give 0). This matches encoding/json. It
// is checked on the bytes path, the stream path, and for fields, map values,
// slice elements and pointers.
func TestNarrowIntOverflow(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
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

// TestInvalidUTF8Rejected pins the UTF-8 rules for decode through generated
// code. It covers the inline fast path for short strings, the fallback call
// to ggen.String, strings with escapes, and the stream path. Invalid UTF-8
// in a string value is rejected with ggen.ErrInvalidUTF8. So is an unpaired
// surrogate: a \uD800-\uDFFF escape that is not part of a valid pair. This
// matches jsonv2. encoding/json v1 instead replaces bad bytes with U+FFFD, and
// ggen differs from v1 on purpose. Valid multi-byte UTF-8 decodes the same as
// jsonv2.
func TestInvalidUTF8Rejected(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 40) // longer than the 32-byte inline path, so ggen.String is called
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
			t.Parallel()
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

// TestRawCaptureInvalidUTF8 checks captured raw values (json.RawMessage and
// jsontext.Value) on both paths. Invalid UTF-8 bytes are rejected with
// ggen.ErrInvalidUTF8, as jsonv2 does. One known difference remains: an
// unpaired \uXXXX surrogate escape inside a raw value is plain ASCII text, so
// ggen accepts it. jsonv2 parses the escapes in raw strings and rejects it.
// See .claude/backlog.md.
func TestRawCaptureInvalidUTF8(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
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

// TestMaxDepthNoCrash checks that very deeply nested payloads return
// ggen.ErrMaxDepth through generated code instead of overflowing the
// goroutine stack, which is a fatal crash that cannot be recovered. It covers
// a self-referential struct (Node.Children), an any field, an ignoreunknown
// skip and a RawMessage capture, on both paths.
func TestMaxDepthNoCrash(t *testing.T) {
	t.Parallel()
	const n = 2_000_000 // far past MaxDepth, and deep enough to overflow the stack without the cap
	arr := strings.Repeat("[", n) + strings.Repeat("]", n)
	cases := []struct {
		name    string
		payload string
		decode  func([]byte) error
	}{
		{
			"recursive_struct", strings.Repeat(`{"children":[`, n) + strings.Repeat(`]}`, n),
			func(d []byte) error { _, _, err := Node{}.DecodeFrom(d); return err },
		},
		{
			"any_field", `{"data":` + arr + `}`,
			func(d []byte) error { _, _, err := AnyHolder{}.DecodeFrom(d); return err },
		},
		{
			"ignoreunknown_skip", `{"zz":` + arr + `}`,
			func(d []byte) error { _, _, err := IgnoreUnknownStruct{}.DecodeFrom(d); return err },
		},
		{
			"raw_capture", `{"raw":` + arr + `}`,
			func(d []byte) error { _, _, err := RawHolder{}.DecodeFrom(d); return err },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if err := c.decode([]byte(c.payload)); !errors.Is(err, ggen.ErrMaxDepth) {
				t.Errorf("bytes: want ErrMaxDepth, got %v", err)
			}
		})
	}
	// Stream path: a recursive struct and a skip, deep enough to exceed the
	// cap.
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

// NumGrammar has one field per number decode path: the inline int and uint
// code, float64, and json.Number.
//
//ggen:generate
type NumGrammar struct {
	I int         `json:"i"`
	U uint        `json:"u"`
	F float64     `json:"f"`
	N json.Number `json:"n"`
}

// TestNumberGrammarStrict pins the value decoders to the RFC 8259 number
// grammar, matching jsonv2. The test inputs are Go number spellings that
// strconv.ParseFloat accepts but JSON forbids. The skip path (skipNumber, used
// for RawMessage and ignoreunknown) rejects them too, so decoding and skipping
// agree on the same bytes.
func TestNumberGrammarStrict(t *testing.T) {
	t.Parallel()
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
			t.Parallel()
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
			// The skip path (RawMessage capture) must agree with the value
			// path.
			raw := `{"raw":` + c.payload[strings.Index(c.payload, ":")+1:]
			_, _, rErr := RawHolder{}.DecodeFrom([]byte(raw))
			if (rErr != nil) != c.reject {
				t.Errorf("skip/raw: reject=%v want %v (err=%v)", rErr != nil, c.reject, rErr)
			}
		})
	}
}

// NarrowFloats pins the float32 version of the narrow-int check. A value out
// of float32 range, such as 1e39, must be rejected. A plain conversion from
// float64 would give +Inf with a nil error. encoding/json v1 and jsonv2 both
// reject it.
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
	// The float32 rounding boundary. Values that round down to MaxFloat32 are
	// accepted. The stdlib does the same: a value is rejected when it
	// converts to Inf, not when it exceeds MaxFloat32.
	got, _, err := (NarrowFloats{}).DecodeFrom([]byte(`{"f":3.4028235e38}`))
	if err != nil || got.F != math.MaxFloat32 {
		t.Errorf("boundary: got %v, %v", got.F, err)
	}
}

// TestNarrowFloat_RoundsDecimalOnce pins single rounding at every float32
// site: field, slice, map, pointer and `,string`, on both paths. Parsing as
// float64 and then converting to float32 rounds the decimal twice, which can
// give a different result. The first three inputs are the shortest float64
// forms of float32 rounding midpoints (what json.Marshal of such a float64
// emits). Double rounding lands them one ulp (one step between adjacent
// floats) off. The last input sits just below the overflow midpoint, and
// double rounding rejects it as an overflow. Both encoding/json v1 and jsonv2
// parse directly at 32 bits.
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

// The tests below pin two things. Structural errors in slices on the bytes
// path are wrapped in *ggen.ParseError with a path and a position. And error
// positions from the scan functions reach the caller unchanged, with the bytes
// and stream paths agreeing on the same payload.

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

// Errors at the start of a pipe: variant value are wrapped like every other
// structural error. The cases are: no variant matches the shape, the value is
// truncated, and a malformed null.
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
	// ggen.String fails inside the value. It returns the error position, and
	// the generated NewParseErr call records it unchanged.
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
	// The bytes path records the start of the string content (ggen.String's
	// error position). The stream path records the value start. Both are real
	// positions, never 0.
	if spe.Pos < 8 {
		t.Errorf("stream: Pos = %d, want >= 8 (at the value)", spe.Pos)
	}
}

// Stream ParseError positions must be offsets into the full payload
// (s.Offset()), not s.Pos, which is relative to the buffer. The Stream holds
// only a window of the input in its buffer. Once a refill compacts the buffer
// and the window moves, the two values differ. With the wrong one, a small
// buffer and a chunked reader would report a position near 0 for a failure
// thousands of bytes in, and the value would change with the chunk size. A
// 16-byte payload cannot catch this because the window never moves.
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
	sb.WriteString(`],"id":true}`) // id is an int, so this fails far into the payload
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

// When generated code refills at the start of a value and the reader has no
// more data, it reports the same grammar error the bytes path reports for
// the truncated input. ggen.NotEOF in streamReadMore does this mapping. A raw
// io.ErrUnexpectedEOF must not reach the caller from the null check, map,
// slice or variant code. The test cuts a payload with nulls at every prefix
// length.
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

// decodeBothChunked is decodeBothPaths with the stream reading chunk bytes per
// Read into a small buffer, so the buffer compacts in the middle of a value.
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

// The stream scan functions reset s.Pos on every error return, so a refill
// that compacted the buffer cannot leave a stale cursor. The reset cursor is
// the same error position the bytes path reports (where scanning stopped).
// For the user this means ParseError.Pos and the sentinel error are the same
// on both paths and at every chunk size. The cases cover integer overflow, a
// malformed number under an ignored unknown key, and a RawMessage value that
// is malformed deep inside.
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

// `format:array` byte elements reject values above 255 with
// ErrNumberOverflow instead of silently wrapping. encoding/json v1 and jsonv2
// both reject them.
func TestFormatArray_ByteOverflow(t *testing.T) {
	t.Parallel()
	bytesErr, streamErr := decodeBothPaths[NativeTypes](`{"byteArray":[300]}`)
	for path, err := range map[string]error{"bytes": bytesErr, "stream": streamErr} {
		if !errors.Is(err, ggen.ErrNumberOverflow) {
			t.Errorf("%s: got %v, want ErrNumberOverflow", path, err)
		}
	}
}

// decodeBothPathsChunked is decodeBothPaths with the stream reading chunk
// bytes per Read into a small buffer.
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

// A control byte inside a string whose closing quote never arrives makes the
// input malformed, not just truncated. Both paths report ErrBadString at the
// same offset.
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

// R10BoolShapes has a bool at every kind of site where generated code calls
// Bool: field, slice element, array element and map value.
//
//ggen:generate
type R10BoolShapes struct {
	Flag  bool            `json:"flag"`
	Flags []bool          `json:"flags"`
	Pair  [2]bool         `json:"pair"`
	ByKey map[string]bool `json:"byKey"`
}

// Every generated Bool site reports the give-up position: the first byte that
// breaks the true or false literal, or the end of a truncated one. This holds
// on both paths and at every chunk size, and matches jsonv2's offset.
// ggen.Bool itself reports the start of the literal. The generated error
// branch computes the give-up position with ggen.BoolEnd. Stream.Bool leaves
// Pos at the give-up position.
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
		{
			"any_field", `{"name":"a","body":falsy}`, 23,
			func(p string, c int) (error, error) { return decodeBothPathsChunked[AnyStruct](p, c) },
		},
		{
			"alias", `trux`, 3,
			func(p string, c int) (error, error) { return decodeBothPathsChunked[AliasBool](p, c) },
		},
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

// R10HeadShapes checks the error reported when input runs out at the start
// of a value. The error follows the JSON shape expected there, not the
// field's Go kind. Examples: the leaf of a pointer chain, the inner value of
// a sql.Null* type, the quote a `,string` value opens with, and the bracket of
// a format:array []byte.
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

// An `n` that does not spell `null` gives ErrBadLiteral for every kind that
// accepts null, on both paths. jsonv2 reports a literal error there too. This
// includes input that ends in the middle of the literal.
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
