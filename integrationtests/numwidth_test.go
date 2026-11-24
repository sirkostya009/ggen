package integrationtests

//go:generate ../ggen $GOFILE

import (
	"math"
	"testing"
)

// NumWidths holds every fixed-width number kind as a field, a slice element
// and a map value, so JSONSize budgets each kind by its own width.
//
//ggen:generate
type NumWidths struct {
	I8   int8              `json:"i8"`
	I16  int16             `json:"i16"`
	I32  int32             `json:"i32"`
	U16  uint16            `json:"u16"`
	U32  uint32            `json:"u32"`
	F32  float32           `json:"f32"`
	U8s  []uint16          `json:"u8s"`
	I32s []int32           `json:"i32s"`
	F32s []float32         `json:"f32s"`
	Mi8  map[string]int8   `json:"mi8"`
	Mu32 map[string]uint32 `json:"mu32"`
	Q16  int16             `json:"q16,string"`
}

//ggen:generate
type NumWidthsSmall struct {
	U8  uint8      `json:"u8"`
	I8s [2]int8    `json:"i8s"`
	S16 Int16Alias `json:"s16"`
}

//ggen:generate
type Int16Alias int16

func TestJSONSize_NumberWidths_NoRealloc(t *testing.T) {
	t.Parallel()
	f32s := []float32{-math.MaxFloat32, math.SmallestNonzeroFloat32, -9.999999e20, -1.2345678e-6, 1.2345678e-7, -0.1}
	cases := []NumWidths{
		{
			I8: math.MinInt8, I16: math.MinInt16, I32: math.MinInt32,
			U16: math.MaxUint16, U32: math.MaxUint32, F32: -9.999999e20,
			U8s:  []uint16{math.MaxUint16, math.MaxUint16},
			I32s: []int32{math.MinInt32, math.MinInt32, math.MinInt32},
			F32s: f32s,
			Mi8:  map[string]int8{"a": math.MinInt8, "b": math.MinInt8},
			Mu32: map[string]uint32{"a": math.MaxUint32},
			Q16:  math.MinInt16,
		},
	}
	for _, f := range f32s {
		cases = append(cases, NumWidths{F32: f, F32s: []float32{f}})
	}
	for _, v := range cases {
		buf := make([]byte, 0, v.JSONSize())
		out, err := v.AppendJSON(buf)
		if err != nil {
			t.Fatal(err)
		}
		if cap(out) != cap(buf) {
			t.Fatalf("AppendJSON grew the buffer: JSONSize %d, wrote %d: %s", cap(buf), len(out), out)
		}
	}
	s := NumWidthsSmall{U8: math.MaxUint8, I8s: [2]int8{math.MinInt8, math.MinInt8}, S16: math.MinInt16}
	out, err := s.AppendJSON(make([]byte, 0, s.JSONSize()))
	if err != nil {
		t.Fatal(err)
	}
	// At each kind's extreme the budget is exact.
	if len(out) != s.JSONSize() {
		t.Fatalf("NumWidthsSmall wrote %d bytes, JSONSize %d: %s", len(out), s.JSONSize(), out)
	}
}

func TestJSONSize_Float32Widest(t *testing.T) {
	t.Parallel()
	// A float32 never needs more than 22 bytes: sign plus 21 integer digits
	// just under 1e21. Walk a spread of bit patterns, every exponent.
	widest := 0
	for bits := uint32(0); bits < 1<<31; bits += 4099 {
		for _, sign := range []uint32{0, 1 << 31} {
			f := math.Float32frombits(bits | sign)
			if math.IsNaN(float64(f)) || math.IsInf(float64(f), 0) {
				continue
			}
			v := NumWidths{F32: f}
			out, err := v.AppendJSON(nil)
			if err != nil {
				t.Fatal(err)
			}
			widest = max(
				widest,
				len(out)-len(`{"i8":0,"i16":0,"i32":0,"u16":0,"u32":0,"f32":,"u8s":null,"i32s":null,"f32s":null,"mi8":null,"mu32":null,"q16":"0"}`),
			)
		}
	}
	if widest > 22 {
		t.Fatalf("float32 wrote %d bytes, budget 22", widest)
	}
	t.Logf("widest float32: %d bytes", widest)
}
