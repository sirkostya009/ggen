package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"errors"
	"testing"

	"github.com/sirkostya009/ggen/decode/validation"
	"github.com/sirkostya009/ggen/scan"
)

// NullZeroTags pins per-field json:",nullzero": an explicit JSON null decodes
// to the Go zero value instead of erroring. Strict (non-nullzero) fields keep
// rejecting null (ggen's default divergence from stdlib).
//
//ggen:generate
type NullZeroTags struct {
	NZStr   string  `json:"nzStr,nullzero"`
	NZInt   int     `json:"nzInt,nullzero"`
	NZFloat float64 `json:"nzFloat,nullzero"`
	NZBool  bool    `json:"nzBool,nullzero"`
	Strict  string  `json:"strict"`
}

// NullZeroValidated pins nullzero composing with validation: null sets the
// zero value, then the field's rules run on that zero (a null key is "present",
// so required-style checks see it).
//
//ggen:generate
type NullZeroValidated struct {
	Count int    `json:"count,nullzero" ggen:"gte=0"`   // null → 0, passes gte=0
	Name  string `json:"name,nullzero" ggen:"minlen=1"` // null → "", fails minlen=1
}

// NullZeroWhole pins the struct-level `nullzero` annotation — every non-pointer
// value field accepts null. The slice field keeps its own native null handling.
//
//ggen:generate nullzero
type NullZeroWhole struct {
	A string `json:"a"`
	B int    `json:"b"`
	C []int  `json:"c"`
}

func TestNullZero_tag_bytes(t *testing.T) {
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
	// A non-nullzero scalar still hard-errors on explicit null.
	if _, _, err := (NullZeroTags{}).DecodeFrom([]byte(`{"strict":null}`)); err == nil {
		t.Fatal("expected error on null into strict field, got nil")
	}
}

func TestNullZero_nonNull_stillDecodes(t *testing.T) {
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
	// null → 0 passes gte=0.
	if got, _, err := (NullZeroValidated{}).DecodeFrom([]byte(`{"count":null}`)); err != nil {
		t.Fatalf("count null: unexpected error: %v", err)
	} else if got.Count != 0 {
		t.Errorf("count = %d, want 0", got.Count)
	}
	// null → "" fails minlen=1 — validation applies to the zeroed value.
	_, _, err := NullZeroValidated{}.DecodeFrom([]byte(`{"name":null}`))
	if err == nil {
		t.Fatal("name null: expected minlen validation error, got nil")
	}
	var mle *validation.MinLenError
	if !errors.As(err, &mle) {
		t.Errorf("name null: got %T (%v), want *validation.MinLenError", err, err)
	}
}

func TestNullZero_whole_annotation(t *testing.T) {
	got, _, err := NullZeroWhole{}.DecodeFrom([]byte(`{"a":null,"b":null,"c":null}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.A != "" || got.B != 0 || got.C != nil {
		t.Errorf("got %+v, want zero", got)
	}
}

func TestNullZero_tag_stream(t *testing.T) {
	in := []byte(`{"nzStr":null,"nzInt":null,"nzFloat":null,"nzBool":null,"strict":"keep"}`)
	var s scan.Stream
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
	in := []byte(`{"strict":null}`)
	var s scan.Stream
	s.Reset(bytes.NewReader(in), make([]byte, 0, len(in)))
	if _, err := (NullZeroTags{}).DecodeFromStream(&s); err == nil {
		t.Fatal("expected error on null into strict field (stream), got nil")
	}
}
