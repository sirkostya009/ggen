//go:build goexperiment.jsonv2

package integrationtests

import (
	"bytes"
	"io"
	"testing"

	"github.com/sirkostya009/ggen/decode"
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
