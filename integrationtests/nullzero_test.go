package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"errors"
	"testing"

	"github.com/sirkostya009/ggen"
	"github.com/sirkostya009/ggen/integrationtests/thirdparty"
)

// NullZeroTags pins per-field nullzero. On a nullzero field, null decodes to
// the zero value. Fields without it still reject null.
//
//ggen:generate
type NullZeroTags struct {
	NZStr   string  `json:"nzStr"   pipe:"nullzero"`
	NZInt   int     `json:"nzInt"   pipe:"nullzero"`
	NZFloat float64 `json:"nzFloat" pipe:"nullzero"`
	NZBool  bool    `json:"nzBool"  pipe:"nullzero"`
	Strict  string  `json:"strict"`
}

// NullZeroValidated pins nullzero combined with validation. The rules run on
// the zero value that null produced.
//
//ggen:generate
type NullZeroValidated struct {
	Count int    `json:"count" pipe:"nullzero gte=0"`
	Name  string `json:"name"  pipe:"nullzero minlen=1"`
}

// NullZeroWhole pins struct-level nullzero. Every field accepts null and
// decodes it to the zero value.
//
//ggen:generate nullzero
type NullZeroWhole struct {
	A string `json:"a"`
	B int    `json:"b"`
	C []int  `json:"c"`
}

func TestNullZero_tag_bytes(t *testing.T) {
	t.Parallel()
	in := []byte(`{"nzStr":null,"nzInt":null,"nzFloat":null,"nzBool":null,"strict":"keep"}`)
	got, _, err := NullZeroTags{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := NullZeroTags{Strict: "keep"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestNullZero_strict_rejects(t *testing.T) {
	t.Parallel()
	if _, _, err := (NullZeroTags{}).DecodeFrom([]byte(`{"strict":null}`)); err == nil {
		t.Fatal("expected error on null into strict field, got nil")
	}
}

func TestNullZero_nonNull_stillDecodes(t *testing.T) {
	t.Parallel()
	in := []byte(`{"nzStr":"hi","nzInt":7,"nzFloat":1.5,"nzBool":true,"strict":"k"}`)
	got, _, err := NullZeroTags{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := NullZeroTags{NZStr: "hi", NZInt: 7, NZFloat: 1.5, NZBool: true, Strict: "k"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestNullZero_validation_runs_on_zero(t *testing.T) {
	t.Parallel()
	if got, _, err := (NullZeroValidated{}).DecodeFrom([]byte(`{"count":null}`)); err != nil {
		t.Fatalf("count null: unexpected error: %v", err)
	} else if got.Count != 0 {
		t.Errorf("count = %d, want 0", got.Count)
	}
	// null becomes "", which fails minlen=1.
	_, _, err := NullZeroValidated{}.DecodeFrom([]byte(`{"name":null}`))
	if err == nil {
		t.Fatal("name null: expected minlen validation error, got nil")
	}
	if _, ok := errors.AsType[*ggen.MinLenError](err); !ok {
		t.Errorf("name null: got %T (%v), want *ggen.MinLenError", err, err)
	}
}

func TestNullZero_whole_annotation(t *testing.T) {
	t.Parallel()
	got, _, err := NullZeroWhole{}.DecodeFrom([]byte(`{"a":null,"b":null,"c":null}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.A != "" || got.B != 0 || got.C != nil {
		t.Errorf("got %+v, want zero", got)
	}
}

func TestNullZero_tag_stream(t *testing.T) {
	t.Parallel()
	in := []byte(`{"nzStr":null,"nzInt":null,"nzFloat":null,"nzBool":null,"strict":"keep"}`)
	var s ggen.Stream
	s.Reset(bytes.NewReader(in), make([]byte, 0, len(in)))
	got, err := NullZeroTags{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := NullZeroTags{Strict: "keep"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestNullZero_stream_strict_rejects(t *testing.T) {
	t.Parallel()
	in := []byte(`{"strict":null}`)
	var s ggen.Stream
	s.Reset(bytes.NewReader(in), make([]byte, 0, len(in)))
	if _, err := (NullZeroTags{}).DecodeFromStream(&s); err == nil {
		t.Fatal("expected error on null into strict field (stream), got nil")
	}
}

type NullZeroDigest [4]byte

// NullZeroByteArrays pins nullzero on fixed byte arrays, which encode as
// strings. null decodes to the zero array.
//
//ggen:generate nullzero
type NullZeroByteArrays struct {
	H [4]byte        `json:"h"`
	X [2]byte        `json:"x,format:hex"`
	D NullZeroDigest `json:"d"`
}

//ggen:generate
type NullZeroByteArrayTag struct {
	H [4]byte `json:"h" pipe:"nullzero"`
}

func TestNullZero_byteArrays(t *testing.T) {
	t.Parallel()
	const in = `{"h":null,"x":null,"d":null}`
	prev := NullZeroByteArrays{H: [4]byte{1}, X: [2]byte{2}, D: NullZeroDigest{3}}
	got, _, err := prev.DecodeFrom([]byte(in))
	if err != nil || got != (NullZeroByteArrays{}) {
		t.Errorf("bytes: %+v %v, want zero", got, err)
	}
	var s ggen.Stream
	s.Reset(bytes.NewReader([]byte(in)), make([]byte, 0, 8))
	sv, err := s.Value[NullZeroByteArrays]()
	if err != nil || sv != (NullZeroByteArrays{}) {
		t.Errorf("stream: %+v %v, want zero", sv, err)
	}
	const tag = `{"h":null}`
	if v, _, err := (NullZeroByteArrayTag{}).DecodeFrom([]byte(tag)); err != nil || v != (NullZeroByteArrayTag{}) {
		t.Errorf("tag bytes: %+v %v", v, err)
	}
	s.Reset(bytes.NewReader([]byte(tag)), make([]byte, 0, 8))
	if v, err := s.Value[NullZeroByteArrayTag](); err != nil || v != (NullZeroByteArrayTag{}) {
		t.Errorf("tag stream: %+v %v", v, err)
	}
	if v, _, err := (NullZeroByteArrayTag{}).DecodeFrom([]byte(`{"h":"AQIDBA=="}`)); err != nil || v.H != [4]byte{1, 2, 3, 4} {
		t.Errorf("non-null: %+v %v", v, err)
	}
}

type NullZeroNames []string

type NullZeroIndex map[string]int

//ggen:generate
type NullZeroTagList []string

// NullZeroNamedContainers pins nullzero on named slice and map types: null
// decodes to their Go zero, nil, as it does without nullzero.
//
//ggen:generate nullzero
type NullZeroNamedContainers struct {
	Names NullZeroNames   `json:"names"`
	Index NullZeroIndex   `json:"index"`
	Tags  NullZeroTagList `json:"tags"`
	N     int             `json:"n"`
}

// NullZeroRanked has fields of types this package cannot name.
//
//ggen:generate nullzero
type NullZeroRanked thirdparty.Ranked

func TestNullZero_namedContainersNil(t *testing.T) {
	t.Parallel()
	const in = `{"names":null,"index":null,"tags":null,"n":null}`
	prev := NullZeroNamedContainers{Names: NullZeroNames{"a"}, Index: NullZeroIndex{"a": 1}, Tags: NullZeroTagList{"t"}, N: 1}
	b, _, err := prev.DecodeFrom([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var s ggen.Stream
	s.Reset(bytes.NewReader([]byte(in)), make([]byte, 0, 8))
	sv, err := s.Value[NullZeroNamedContainers]()
	if err != nil {
		t.Fatal(err)
	}
	for path, v := range map[string]NullZeroNamedContainers{"bytes": b, "stream": sv} {
		if v.Names != nil || v.Index != nil || v.Tags != nil || v.N != 0 {
			t.Errorf("%s: %#v, want every field zero", path, v)
		}
		if out, err := ggen.Marshal(v); err != nil || string(out) != `{"index":null,"n":0,"names":null,"tags":null}` {
			t.Errorf("%s re-marshal: %s %v", path, out, err)
		}
	}
	r, _, err := NullZeroRanked{}.DecodeFrom([]byte(`{"Code":"c","Level":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if want := NullZeroRanked(thirdparty.NewRanked("c", "")); r.Code != want.Code || r.Level != want.Level {
		t.Errorf("ranked: %+v, want %+v", r, want)
	}
}
