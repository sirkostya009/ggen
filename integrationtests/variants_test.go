package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/sirkostya009/ggen/scan"
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
	var s scan.Stream
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
				var s scan.Stream
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
