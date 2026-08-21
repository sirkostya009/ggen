package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"errors"
	"testing"

	"github.com/sirkostya009/ggen"
)

// NullZeroTags pins per-field nullzero: null decodes to the zero value; strict
// fields keep rejecting null.
//
//ggen:generate
type NullZeroTags struct {
	NZStr   string  `json:"nzStr" pipe:"nullzero"`
	NZInt   int     `json:"nzInt" pipe:"nullzero"`
	NZFloat float64 `json:"nzFloat" pipe:"nullzero"`
	NZBool  bool    `json:"nzBool" pipe:"nullzero"`
	Strict  string  `json:"strict"`
}

// NullZeroValidated pins nullzero composing with validation: rules run on the
// zeroed value.
//
//ggen:generate
type NullZeroValidated struct {
	Count int    `json:"count" pipe:"nullzero gte=0"`
	Name  string `json:"name" pipe:"nullzero minlen=1"`
}

// NullZeroWhole pins struct-level nullzero — every value field accepts null.
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
	// null → "" fails minlen=1.
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
