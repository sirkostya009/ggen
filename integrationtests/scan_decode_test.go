//go:build goexperiment.jsonv2

package integrationtests

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/decode/validation"
)

// chunkReader delivers payload one byte at a time (or maxChunk at a time).
// Stress-tests Stream.ReadMore's per-call grow path: every byte forces a
// fresh Read, and the parser makes progress byte-by-byte without ever
// looping inside ReadMore.
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
	got, err := decode.Unmarshal[Node](complexPayload)
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
	got, err := decode.Read[Node](bytes.NewReader(complexPayload))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Name != "hello world" || got.ID != 42 {
		t.Errorf("got %+v", got)
	}
}

func TestUnmarshalSlice_roundtrip(t *testing.T) {
	arr := []byte(`[
		{"street":"A","city":"X","zipCode":"00001"},
		{"street":"B","city":"Y","zipCode":"00002"}
	]`)
	got, err := decode.UnmarshalSlice[Address](arr)
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
	arr := []byte(`[{"street":"A","city":"X","zipCode":"00001"}]`)
	got, err := decode.ReadSlice[Address](bytes.NewReader(arr))
	if err != nil {
		t.Fatalf("ReadSlice: %v", err)
	}
	if len(got) != 1 || got[0].Street != "A" {
		t.Errorf("got %+v", got)
	}
}

func TestUnmarshalStream_roundtrip(t *testing.T) {
	got, _, err := decode.UnmarshalStream[Node](
		bytes.NewReader(complexPayload), make([]byte, 0, len(complexPayload)))
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
	// Force Ensure to loop: reader hands out 1 byte per Read call.
	r := &chunkReader{data: complexPayload, max: 1}
	got, _, err := decode.UnmarshalStream[Node](r, nil)
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
	// Hint smaller than payload — buffer must grow via append mid-parse.
	// Zero-copy strings must remain valid across the grow (GC keeps old
	// backing arrays alive because aliases reference them).
	got, _, err := decode.UnmarshalStream[Node](
		bytes.NewReader(complexPayload), make([]byte, 0, 32))
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

// HugeStringStruct holds a single string. Used to slam ReadMore(0) and
// stringSlow with a multi-MiB body.
//
//ggen:generate
type HugeStringStruct struct {
	Big string `json:"big"`
}

// TestUnmarshalStream_SingleHugeString: a 2 MiB string fed through a tiny
// initial buffer must decode losslessly. Exercises Stream.String's slow
// path + grow + compaction across many ReadMore(0) calls.
func TestUnmarshalStream_SingleHugeString(t *testing.T) {
	const size = 2 * 1024 * 1024
	huge := strings.Repeat("x", size)
	payload := []byte(`{"big":"` + huge + `"}`)

	got, _, err := decode.UnmarshalStream[HugeStringStruct](
		bytes.NewReader(payload), make([]byte, 0, 64))
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

// TestUnmarshalStream_MassiveWhitespace: SkipSpace must consume ~300 KB
// of inter-field whitespace across many ReadMore compaction cycles.
// Failures here mean the keep>0 cursor adjustment after SkipSpace broke.
func TestUnmarshalStream_MassiveWhitespace(t *testing.T) {
	gap := strings.Repeat("\n \t", 100_000)
	payload := []byte(`{` + gap + `"big":` + gap + `"hello"` + gap + `}`)

	got, _, err := decode.UnmarshalStream[HugeStringStruct](
		bytes.NewReader(payload), make([]byte, 0, 64))
	if err != nil {
		t.Fatalf("UnmarshalStream: %v\npayload size: %d", err, len(payload))
	}
	if got.Big != "hello" {
		t.Errorf("Big = %q, want %q", got.Big, "hello")
	}
}

// SequentialStringsStruct: many string fields read back-to-back across
// compactions. Each prior decode must be byte-identical to its input
// AFTER subsequent compactions, because Stream.String copies its result.
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

// TestUnmarshalStream_ValuesSurviveCompaction: after each field is decoded
// the buffer compacts on the next SkipSpace/dispatch. Owned-copy semantics
// mean earlier values stay correct. Failure means a string path is aliasing
// s.buf and getting clobbered by the memmove.
func TestUnmarshalStream_ValuesSurviveCompaction(t *testing.T) {
	in := []byte(`{"a":"AAAAAAAA","b":"BBBBBBBB","c":"CCCCCCCC","d":"DDDDDDDD",` +
		`"e":"EEEEEEEE","f":"FFFFFFFF","g":"GGGGGGGG","h":"HHHHHHHH"}`)
	r := &chunkReader{data: in, max: 1}
	got, _, err := decode.UnmarshalStream[SequentialStringsStruct](r, make([]byte, 0, 16))
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

// TestUnmarshalStream_RawJSONAcrossBoundary: capture a json.RawMessage
// value that SPANS multiple ReadMore boundaries. The generator wraps
// RawJSON capture in `s.Shift = false; ...; s.Shift = prev` so absolute
// offsets stay stable.
func TestUnmarshalStream_RawJSONAcrossBoundary(t *testing.T) {
	bigInner := `{"deeply":{"nested":{"arr":[` +
		strings.Repeat(`"item","item",`, 200) + `"end"]}}}`
	in := []byte(`{"raw1":` + bigInner + `,"raw2":null,"site":"http://x",` +
		`"big":0,"bigF":"0","bigR":"0","id":"00000000-0000-0000-0000-000000000000",` +
		`"gofrsId":"00000000-0000-0000-0000-000000000000"}`)
	got, _, err := decode.UnmarshalStream[RichTypes](
		bytes.NewReader(in), make([]byte, 0, 64))
	if err != nil {
		t.Fatalf("UnmarshalStream: %v\npayload size: %d", err, len(in))
	}
	if !bytes.Equal(bytes.TrimSpace(got.Raw1), []byte(bigInner)) {
		t.Errorf("Raw1 capture corrupted across boundary\n got: %s\nwant: %s",
			got.Raw1, bigInner)
	}
}

// TestUnmarshalStream_InlineMapKeyClone: catch-all map (json:",inline")
// keys are aliased via Stream.KeyView for dispatch, but the inline-map
// insertion must strings.Clone() the key — otherwise subsequent
// compactions corrupt the map keys.
func TestUnmarshalStream_InlineMapKeyClone(t *testing.T) {
	in := []byte(`{"name":"alice","kAlpha":1,"kBravoo":2,"kCharlie":3,` +
		`"kDelta11":4,"kEcho1234":5,"kFoxtrot1":6,"kGolf12345":7}`)
	r := &chunkReader{data: in, max: 1}
	got, _, err := decode.UnmarshalStream[InlineStruct](r, make([]byte, 0, 16))
	if err != nil {
		t.Fatalf("UnmarshalStream: %v", err)
	}
	for _, k := range []string{"kAlpha", "kBravoo", "kCharlie", "kDelta11", "kEcho1234", "kFoxtrot1", "kGolf12345"} {
		if _, ok := got.Extra[k]; !ok {
			t.Errorf("expected inline map key %q in %v", k, got.Extra)
		}
	}
}

// UnknownErrorStruct: closed schema (default — errors on unknown). The
// generated stream code must strings.Clone the unknown-key string before
// stuffing it into validation.UnknownKeyError.
//
//ggen:generate
type UnknownErrorStruct struct {
	Name string `json:"name"`
}

func TestUnmarshalStream_UnknownKeyErrorClone(t *testing.T) {
	in := []byte(`{"name":"alice","SomeUnknownKeyHere":42}`)
	r := &chunkReader{data: in, max: 4}
	_, _, err := decode.UnmarshalStream[UnknownErrorStruct](r, make([]byte, 0, 8))
	if err == nil {
		t.Fatal("expected UnknownKeyError")
	}
	var ue *validation.UnknownKeyError
	if !errors.As(err, &ue) {
		t.Fatalf("got %T (%v), want *UnknownKeyError", err, err)
	}
	if ue.Field != "SomeUnknownKeyHere" {
		t.Errorf("Field = %q, want %q (cloning broken)", ue.Field, "SomeUnknownKeyHere")
	}
}

// TestUnmarshalStream_TinyBufManyKeys: 40-field object decoded through
// a 1-byte reader + 1-byte initial buf. Stress-tests the dispatch-loop
// compaction shift points + bitmask seen-flag path.
func TestUnmarshalStream_TinyBufManyKeys(t *testing.T) {
	var parts []string
	for i := 1; i <= 40; i++ {
		parts = append(parts, formatField(i))
	}
	in := []byte(`{` + strings.Join(parts, ",") + `}`)

	r := &chunkReader{data: in, max: 1}
	got, _, err := decode.UnmarshalStream[WideStruct](r, make([]byte, 0, 1))
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

// TestUnmarshalStream_HintBufSmallerThanValue: hint buf strictly smaller
// than the JSON value forces the slow-path grow+compaction repeatedly.
// Mirrors what real HTTP-body-sized payloads behave like over a small
// initial pool buffer.
func TestUnmarshalStream_HintBufSmallerThanValue(t *testing.T) {
	for _, hint := range []int{1, 4, 16, 64, 256} {
		t.Run(strconv.Itoa(hint), func(t *testing.T) {
			r := bytes.NewReader(complexPayload)
			got, _, err := decode.UnmarshalStream[Node](r, make([]byte, 0, hint))
			if err != nil {
				t.Fatalf("hint=%d: %v", hint, err)
			}
			if got.Name != "hello world" || len(got.Children) != 2 {
				t.Errorf("hint=%d: drift %+v", hint, got)
			}
		})
	}
}

// TestUnmarshalStream_PartialReadAtBoundaries: reader returns variable
// chunk sizes — bursts then stalls. Models a real network reader.
func TestUnmarshalStream_PartialReadAtBoundaries(t *testing.T) {
	r := &burstReader{data: complexPayload, sizes: []int{3, 1, 7, 1, 13, 1, 50, 1}}
	got, _, err := decode.UnmarshalStream[Node](r, make([]byte, 0, 16))
	if err != nil {
		t.Fatalf("UnmarshalStream: %v", err)
	}
	if got.Name != "hello world" {
		t.Errorf("Name = %q", got.Name)
	}
}

// burstReader yields chunks of explicit sizes from the `sizes` slice,
// wrapping when exhausted. Simulates the variable-burst-then-stall
// pattern real network connections exhibit.
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

// TestUnmarshalStream_BytesEqualsStream: property check — for any
// payload that the bytes path accepts, the stream path must produce a
// deeply-equal result. Catches chunk-boundary-sensitive bugs in
// SkipSpace, KeyView, String escape handling, number scanning.
func TestUnmarshalStream_BytesEqualsStream(t *testing.T) {
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
		want, errBytes := decode.Unmarshal[Node](seed)
		if errBytes != nil {
			continue
		}
		for _, ch := range chunks {
			r := &chunkReader{data: seed, max: ch}
			got, _, errS := decode.UnmarshalStream[Node](r, make([]byte, 0, 8))
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

// TestUnmarshalStream_AcceptedByBytes_AlsoAcceptedByStream: complementary
// check for the same property over the existing fuzz seed set.
func TestUnmarshalStream_AcceptedByBytes_AlsoAcceptedByStream(t *testing.T) {
	for _, seed := range fuzzSeeds {
		want, errA := decode.Unmarshal[Node](seed)
		if errA != nil {
			continue
		}
		for _, ch := range []int{1, 4, 16} {
			r := &chunkReader{data: seed, max: ch}
			got, _, errB := decode.UnmarshalStream[Node](r, make([]byte, 0, 4))
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
