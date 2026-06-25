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
