package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"testing/iotest"

	"github.com/sirkostya009/ggen"
)

//ggen:generate
type Money struct {
	Amount int `json:"amount"`
}

//ggen:generate
type LooseThing struct {
	// number, or a string-encoded number (fallible converter)
	Count int `json:"count" pipe:". / @AtoiStrict ~ gte=0"`
	// number, or an {amount} object via a ggen-decoded struct converter
	Price int `json:"price" pipe:". / @FromMoney"`
	// null → 0, number, or string
	Opt int `json:"opt" pipe:"nullzero / . / @AtoiStrict"`
}

func AtoiStrict(s string) (int, error) { return strconv.Atoi(s) }
func FromMoney(m Money) int            { return m.Amount }
func DoubleInt(n int) int              { return n * 2 }

//ggen:generate
type ElemInterleave struct {
	// per element: validate lte=10 on the raw value, then double it.
	Nums []int `json:"nums" pipe:"inner:(lte=10 @DoubleInt)"`
}

func TestVariants_ElemInterleave(t *testing.T) {
	t.Parallel()
	got, _, err := ElemInterleave{}.DecodeFrom([]byte(`{"nums":[6,5]}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Nums) != 2 || got.Nums[0] != 12 || got.Nums[1] != 10 {
		t.Errorf("Nums = %v, want [12 10] (lte checked on raw, then doubled)", got.Nums)
	}
	if _, _, err := (ElemInterleave{}).DecodeFrom([]byte(`{"nums":[11]}`)); err == nil {
		t.Error("expected lte=10 failure on raw element 11")
	}
}

func TestVariants_BytesPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name              string
		in                string
		count, price, opt int
		wantErr           bool
	}{
		{"native_number", `{"count":5,"price":99,"opt":2}`, 5, 99, 2, false},
		{"string_converted", `{"count":"7","price":99,"opt":"3"}`, 7, 99, 3, false},
		{"object_converted", `{"count":1,"price":{"amount":42},"opt":0}`, 1, 42, 0, false},
		{"null_opt", `{"count":1,"price":1,"opt":null}`, 1, 1, 0, false},
		{"gte_fails_on_string", `{"count":"-3","price":1,"opt":0}`, 0, 0, 0, true},
		{"unmatched_shape", `{"count":true,"price":1,"opt":0}`, 0, 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, _, err := LooseThing{}.DecodeFrom([]byte(c.in))
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Count != c.count || got.Price != c.price || got.Opt != c.opt {
				t.Errorf("got {Count:%d Price:%d Opt:%d}, want {%d %d %d}",
					got.Count, got.Price, got.Opt, c.count, c.price, c.opt)
			}
		})
	}
}

func TestVariants_StreamPath(t *testing.T) {
	t.Parallel()
	in := []byte(`{"count":"7","price":{"amount":42},"opt":null}`)
	var s ggen.Stream
	s.Reset(bytes.NewReader(in), make([]byte, 0, len(in)))
	got, err := LooseThing{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("stream decode: %v", err)
	}
	if got.Count != 7 || got.Price != 42 || got.Opt != 0 {
		t.Errorf("stream got {Count:%d Price:%d Opt:%d}, want {7 42 0}", got.Count, got.Price, got.Opt)
	}
}

// A converter on a NAMED-PRIMITIVE field: the stream path used to route the
// field through an underlying-typed temp before the converter check and
// assign the named-typed result into it (non-compiling), and the native
// variant claimed the OBJECT shape because a named primitive reports
// KindStruct — so `{"s":42}` hit the dispatch default.
type Score int

func ScoreFromString(s string) (Score, error) { return Score(len(s)), nil }

// A pointer field in a converter dispatch: the nullzero arm emitted
// `*int(0)` (zeroLit converted through the POINTER spelling).
func PtrFromString(s string) (*int, error) { n := len(s); return &n, nil }

//ggen:generate
type ConvNamed struct {
	S Score `json:"s" pipe:". / @ScoreFromString"`
	N *int  `json:"n" pipe:"nullzero / . / @PtrFromString"`
}

func TestVariants_NamedPrimAndPointer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		payload string
		wantS   Score
		wantN   any // nil or int
	}{
		{`{"s":42,"n":7}`, 42, 7},        // both native
		{`{"s":"abc","n":"abcd"}`, 3, 4}, // both converted
		{`{"s":1,"n":null}`, 1, nil},     // nullzero arm
	}
	for _, c := range cases {
		for _, path := range []string{"bytes", "stream"} {
			var got ConvNamed
			var err error
			if path == "bytes" {
				got, _, err = ConvNamed{}.DecodeFrom([]byte(c.payload))
			} else {
				var s ggen.Stream
				s.Reset(bytes.NewReader([]byte(c.payload)), make([]byte, 0, 4))
				got, err = ConvNamed{}.DecodeFromStream(&s)
			}
			if err != nil {
				t.Errorf("%s %s: %v", path, c.payload, err)
				continue
			}
			if got.S != c.wantS {
				t.Errorf("%s %s: S = %d, want %d", path, c.payload, got.S, c.wantS)
			}
			if c.wantN == nil {
				if got.N != nil {
					t.Errorf("%s %s: N = %v, want nil", path, c.payload, *got.N)
				}
			} else if got.N == nil || *got.N != c.wantN.(int) {
				t.Errorf("%s %s: N = %v, want %v", path, c.payload, got.N, c.wantN)
			}
		}
	}
}

// An error-form converter's own error is foreign, so it gets wrapped with the
// field path and payload offset like every other decode failure. It used to
// propagate bare (a *strconv.NumError with no idea which field failed).
func TestVariant_converterErrorCarriesPathAndPos(t *testing.T) {
	t.Parallel()
	_, _, err := LooseThing{}.DecodeFrom([]byte(`{"count":"xyz"}`))
	var pe *ggen.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("got %T %v, want *ggen.ParseError", err, err)
	}
	if len(pe.Path) == 0 || pe.Path[0] != "count" {
		t.Errorf("Path = %v, want [count]", pe.Path)
	}
	if pe.Pos <= 0 {
		t.Errorf("Pos = %d, want a real offset", pe.Pos)
	}
	if _, ok := errors.AsType[*strconv.NumError](err); !ok {
		t.Errorf("wrapping hid the converter's own error: %v", err)
	}

	var st ggen.Stream
	st.Reset(bytes.NewReader([]byte(`{"count":"xyz"}`)), nil)
	_, serr := LooseThing{}.DecodeFromStream(&st)
	var spe *ggen.ParseError
	if !errors.As(serr, &spe) {
		t.Fatalf("stream: got %T %v, want *ggen.ParseError", serr, serr)
	}
	if len(spe.Path) == 0 || spe.Path[0] != "count" {
		t.Errorf("stream: Path = %v, want [count]", spe.Path)
	}
}

// Converter INPUTS resolve like a field of that type would: a pointer input
// takes the pointer scan (null → nil, so the variant claims 'n' too) and an
// unannotated named primitive resolves through its underlying kind. Both
// used to claim the object shape `{` and fall to the encoding/json
// fallback, so the wire number every such converter exists for hit the
// dispatch default.
type R10Score int

//ggen:generate
type R10Money struct {
	Cents int64 `json:"cents"`
}

func R10FromScore(s R10Score) R10Money { return R10Money{Cents: int64(s) * 100} }

func R10FromPtr(p *int64) R10Money {
	if p == nil {
		return R10Money{Cents: -1}
	}
	return R10Money{Cents: *p}
}

func R10PtrLabel(p *int) string {
	if p == nil {
		return "nil"
	}
	return "ptr"
}

//ggen:generate
type R10ConvInputs struct {
	Named R10Money `json:"named" pipe:"@R10FromScore/nullzero"`
	Ptr   R10Money `json:"ptr" pipe:"@R10FromPtr/."`
	Label string   `json:"label" pipe:". / @R10PtrLabel"`
}

func TestVariants_R10ConverterInputs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    string
		named int64
		ptr   int64
		label string
	}{
		{"numbers", `{"named":5,"ptr":7,"label":9}`, 500, 7, "ptr"},
		{"nulls", `{"named":null,"ptr":null,"label":null}`, 0, -1, "nil"},
		{"natives", `{"named":-2,"ptr":{"cents":3},"label":"x"}`, -200, 3, "x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			check := func(path string, got R10ConvInputs, err error) {
				if err != nil {
					t.Fatalf("%s: %v", path, err)
				}
				if got.Named.Cents != c.named || got.Ptr.Cents != c.ptr || got.Label != c.label {
					t.Errorf("%s: got {%d %d %q}, want {%d %d %q}", path,
						got.Named.Cents, got.Ptr.Cents, got.Label, c.named, c.ptr, c.label)
				}
			}
			got, _, err := R10ConvInputs{}.DecodeFrom([]byte(c.in))
			check("bytes", got, err)
			for _, chunk := range []int{1, 3, 64} {
				var s ggen.Stream
				s.Reset(iotest.OneByteReader(bytes.NewReader([]byte(c.in))), make([]byte, 0, chunk))
				got, err := R10ConvInputs{}.DecodeFromStream(&s)
				check(fmt.Sprintf("stream/%d", chunk), got, err)
			}
		})
	}
	if _, _, err := (R10ConvInputs{}).DecodeFrom([]byte(`{"label":true}`)); err == nil {
		t.Error("a shape no variant claims must still fail")
	}
}

// R10BConvPtr: a POINTER field whose decode stage dispatches on shape still
// runs its value steps split by target — built-ins on the pointee (guarded, a
// variant may leave the pointer nil), `@Func` steps on the pointer itself.
//
//ggen:generate
type R10BConvPtr struct {
	N *int `json:"n" pipe:"nullzero / . / @R10BAtoiPtr gte=2"`
}

func R10BAtoiPtr(s string) (*int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return nil, err
	}
	return &n, nil
}

func TestVariants_pointerFieldValueSteps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		ok   bool
		want func(v R10BConvPtr) bool
	}{
		{`{"n":5}`, true, func(v R10BConvPtr) bool { return *v.N == 5 }},
		{`{"n":"7"}`, true, func(v R10BConvPtr) bool { return *v.N == 7 }},
		{`{"n":1}`, false, nil},
		{`{"n":"1"}`, false, nil},
		{`{"n":null}`, true, func(v R10BConvPtr) bool { return v.N == nil }},
	}
	for _, c := range cases {
		got, _, err := R10BConvPtr{}.DecodeFrom([]byte(c.in))
		if (err == nil) != c.ok || (c.want != nil && err == nil && !c.want(got)) {
			t.Errorf("bytes %s: %+v (%v), want ok=%v", c.in, got, err, c.ok)
		}
		var s ggen.Stream
		s.Reset(bytes.NewReader([]byte(c.in)), nil)
		sgot, err := R10BConvPtr{}.DecodeFromStream(&s)
		if (err == nil) != c.ok || (c.want != nil && err == nil && !c.want(sgot)) {
			t.Errorf("stream %s: %+v (%v), want ok=%v", c.in, sgot, err, c.ok)
		}
	}
}
