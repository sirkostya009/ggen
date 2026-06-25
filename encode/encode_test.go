package encode

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// TestAppendFloatStdlibParity: float wire bytes must match stdlib (v1 and
// v2 agree on ES6-style formatting): 'f' notation while the decimal
// exponent sits in [-6, 21), 'e' otherwise — with no zero-padded negative
// exponent ("1e-7", not "1e-07"). Every row is cross-checked against
// encoding/json v1 so the table can't drift.
func TestAppendFloatStdlibParity(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v    float64
		bits int
		want string
	}{
		{1e6, 64, "1000000"},
		{123456789, 64, "123456789"},
		{1e20, 64, "100000000000000000000"},
		{1e21, 64, "1e+21"},
		{1e-6, 64, "0.000001"},
		{1e-7, 64, "1e-7"},
		{-1e-7, 64, "-1e-7"},
		{0.1, 64, "0.1"},
		{0, 64, "0"},
		{math.MaxFloat64, 64, "1.7976931348623157e+308"},
		{5e-324, 64, "5e-324"},
		{float64(float32(3.4e38)), 32, "3.4e+38"},
		{float64(float32(1e7)), 32, "10000000"},
		{float64(float32(1e-7)), 32, "1e-7"},
	}
	for _, c := range cases {
		got, err := AppendFloat(nil, c.v, c.bits)
		if err != nil {
			t.Errorf("AppendFloat(%v, %d): %v", c.v, c.bits, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("AppendFloat(%v, %d) = %s, want %s", c.v, c.bits, got, c.want)
		}
		var sv []byte
		if c.bits == 32 {
			sv, _ = json.Marshal(float32(c.v))
		} else {
			sv, _ = json.Marshal(c.v)
		}
		if string(sv) != c.want {
			t.Errorf("table drift: stdlib emits %s for %v/%d, table says %s", sv, c.v, c.bits, c.want)
		}
	}
}

// fatItem is a minimal Marshaler whose JSONSize depends on its content —
// the zero value reports 2 bytes while populated items report much more,
// exposing zero-value-based presizing in MarshalSlice.
type fatItem struct{ s string }

func (f fatItem) JSONSize() int { return 2 + 2*len(f.s) }
func (f fatItem) AppendJSON(dst []byte) ([]byte, error) {
	dst = append(dst, '"')
	return AppendStringNoHTML(dst, f.s), nil
}

// TestMarshalSlicePointerElems: instantiating MarshalSlice with a pointer
// type must not panic (`var zero *T; zero.JSONSize()` derefs nil), and nil
// elements must marshal as JSON null.
func TestMarshalSlicePointerElems(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("MarshalSlice([]*T) panicked: %v", r)
		}
	}()
	a, b := fatItem{s: "a"}, fatItem{s: "b"}
	got, err := MarshalSlice([]*fatItem{&a, nil, &b})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `["a",null,"b"]` {
		t.Errorf("MarshalSlice = %s, want [\"a\",null,\"b\"]", got)
	}
}

// TestMarshalSliceSingleAlloc: the output buffer must be presized from the
// items' actual JSONSize sum — not the zero value's — so marshaling does a
// single allocation instead of walking the append growth chain.
func TestMarshalSliceSingleAlloc(t *testing.T) {
	items := make([]fatItem, 64)
	for i := range items {
		items[i] = fatItem{s: strings.Repeat("x", 100)}
	}
	allocs := testing.AllocsPerRun(50, func() {
		if _, err := MarshalSlice(items); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 1 {
		t.Errorf("MarshalSlice did %v allocs, want 1 (presized from zero-value JSONSize?)", allocs)
	}
}
