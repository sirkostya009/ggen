//go:build goexperiment.jsonv2

package main

import (
	"bytes"
	jsonv2 "encoding/json/v2"
	"io"
	"testing"

	sonicdecoder "github.com/bytedance/sonic/decoder"
	"github.com/sirkostya009/ggen/decode"
)

// chunkReader delivers payload one byte at a time (or maxChunk at a time).
// Stress-tests Stream.Ensure's "need more bytes" retry loop.
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
	got, err := decode.Unmarshal[SomePayloadRequestStruct](complexPayload)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Field1 != "hello world" {
		t.Errorf("Field1 = %q", got.Field1)
	}
	if len(got.Slice) != 5 || got.Slice[0] != 1 || got.Slice[4] != 5 {
		t.Errorf("Slice = %v", got.Slice)
	}
	if got.Address.Street != "Main 1" || got.Address.City != "Lviv" || got.Address.ZipCode != "79000" {
		t.Errorf("Address = %+v", got.Address)
	}
	if len(got.Contacts) != 2 || got.Contacts[1].Street != "S2" || got.Contacts[0].ZipCode != "00001" {
		t.Errorf("Contacts = %+v", got.Contacts)
	}
	if got.Email != "foo@bar.com" || got.Website != "https://example.com/x" {
		t.Errorf("Email/Website = %q / %q", got.Email, got.Website)
	}
	if got.UserID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("UserID = %q", got.UserID)
	}
	if got.Role != "admin" || got.Age != 30 || got.Quota != 5 {
		t.Errorf("Role/Age/Quota = %q/%d/%d", got.Role, got.Age, got.Quota)
	}
}

func TestRead_roundtrip(t *testing.T) {
	got, err := decode.Read[SomePayloadRequestStruct](bytes.NewReader(complexPayload))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Field1 != "hello world" || got.Role != "admin" {
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

func BenchmarkGenerated_UnmarshalSlice(b *testing.B) {
	arr := []byte(`[{"street":"A","city":"X","zipCode":"00001"},{"street":"B","city":"Y","zipCode":"00002"},{"street":"C","city":"Z","zipCode":"00003"}]`)
	b.ReportAllocs()
	b.SetBytes(int64(len(arr)))
	for b.Loop() {
		if _, err := decode.UnmarshalSlice[Address](arr); err != nil {
			b.Fatal(err)
		}
	}
}

func TestUnmarshalStream_roundtrip(t *testing.T) {
	got, _, err := decode.UnmarshalStream[SomePayloadRequestStruct](
		bytes.NewReader(complexPayload), make([]byte, 0, len(complexPayload)))
	if err != nil {
		t.Fatalf("UnmarshalStream: %v", err)
	}
	if got.Field1 != "hello world" || got.Role != "admin" || got.Age != 30 {
		t.Errorf("got %+v", got)
	}
	if len(got.Slice) != 5 || len(got.Contacts) != 2 {
		t.Errorf("slices wrong: %+v", got)
	}
	if got.Address.City != "Lviv" || got.Contacts[1].Street != "S2" {
		t.Errorf("nested structs wrong: %+v", got)
	}
}

func TestUnmarshalStream_chunked(t *testing.T) {
	// Force Ensure to loop: reader hands out 1 byte per Read call.
	r := &chunkReader{data: complexPayload, max: 1}
	got, _, err := decode.UnmarshalStream[SomePayloadRequestStruct](r, nil)
	if err != nil {
		t.Fatalf("UnmarshalStream (1-byte chunks): %v", err)
	}
	if got.Field1 != "hello world" || got.UserID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("chunked decode wrong: %+v", got)
	}
	if len(got.Slice) != 5 || got.Slice[3] != 4 {
		t.Errorf("slice wrong: %v", got.Slice)
	}
}

func TestUnmarshalStream_tinyInitial(t *testing.T) {
	// Hint smaller than payload — buffer must grow via append mid-parse.
	// Zero-copy strings must remain valid across the grow (GC keeps old
	// backing arrays alive because aliases reference them).
	got, _, err := decode.UnmarshalStream[SomePayloadRequestStruct](
		bytes.NewReader(complexPayload), make([]byte, 0, 32))
	if err != nil {
		t.Fatalf("UnmarshalStream (tiny hint): %v", err)
	}
	if got.Field1 != "hello world" || got.UserID != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("post-grow alias corrupted: %+v", got)
	}
	if got.Address.Street != "Main 1" {
		t.Errorf("nested string alias corrupted: %q", got.Address.Street)
	}
}

func BenchmarkGenerated_UnmarshalStream(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(complexPayload)))
	var r bytes.Reader
	for b.Loop() {
		r.Reset(complexPayload)
		if _, _, err := decode.UnmarshalStream[SomePayloadRequestStruct](&r, make([]byte, 0, len(complexPayload))); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkJSONv2_UnmarshalRead(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(complexPayload)))
	var r bytes.Reader
	for b.Loop() {
		r.Reset(complexPayload)
		var v SomePayloadRequestStruct
		if err := jsonv2.UnmarshalRead(&r, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSonic_StreamDecode(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(complexPayload)))
	var r bytes.Reader
	for b.Loop() {
		r.Reset(complexPayload)
		var v SomePayloadRequestStruct
		if err := sonicdecoder.NewStreamDecoder(&r).Decode(&v); err != nil {
			b.Fatal(err)
		}
	}
}
