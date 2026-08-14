package integrationtests

//go:generate ../ggen $GOFILE

import (
	"math/big"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/encode"
)

// OmitStruct exercises omitempty / omitzero / string tag options.
//
//ggen:generate
type OmitStruct struct {
	Name     string            `json:"name"`
	Bio      string            `json:"bio,omitempty"`
	Score    float64           `json:"score,omitzero"`
	StrCount int               `json:"count,string"`
	Tags     []string          `json:"tags,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Meta     map[string]string `json:"meta,omitzero"`
	Extra    []string          `json:"extra,omitzero"`
}

func TestOmitEmpty_marshal(t *testing.T) {
	t.Parallel()
	s := OmitStruct{Name: "alice", Score: 0, StrCount: 42}
	out, _ := encode.MarshalString(s)
	if strings.Contains(out, "bio") {
		t.Errorf("expected bio omitted, got %q", out)
	}
	if strings.Contains(out, "tags") {
		t.Errorf("expected tags omitted, got %q", out)
	}
	if !strings.Contains(out, `"name":"alice"`) {
		t.Errorf("name missing: %q", out)
	}
}

func TestOmitEmpty_present(t *testing.T) {
	t.Parallel()
	s := OmitStruct{Name: "a", Bio: "hello", Tags: []string{"x"}, StrCount: 1}
	out, _ := encode.MarshalString(s)
	if !strings.Contains(out, `"bio":"hello"`) {
		t.Errorf("bio missing: %q", out)
	}
	if !strings.Contains(out, `"tags":["x"]`) {
		t.Errorf("tags missing: %q", out)
	}
}

func TestOmitZero_marshal(t *testing.T) {
	t.Parallel()
	s := OmitStruct{Name: "x", Score: 0, StrCount: 1}
	out, _ := encode.MarshalString(s)
	if strings.Contains(out, "score") {
		t.Errorf("expected score omitted, got %q", out)
	}

	s.Score = 3.14
	out, _ = encode.MarshalString(s)
	if !strings.Contains(out, `"score":3.14`) {
		t.Errorf("score missing: %q", out)
	}
}

func TestStringTag_marshal(t *testing.T) {
	t.Parallel()
	s := OmitStruct{Name: "x", StrCount: 42}
	out, _ := encode.MarshalString(s)
	if !strings.Contains(out, `"count":"42"`) {
		t.Errorf("expected quoted count, got %q", out)
	}
}

func TestStringTag_unmarshal(t *testing.T) {
	t.Parallel()
	input := []byte(`{"name":"x","count":"99"}`)
	got, _, err := OmitStruct{}.DecodeFrom(input)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.StrCount != 99 {
		t.Errorf("StrCount = %d, want 99", got.StrCount)
	}
}

func TestStringTag_unmarshalBadString(t *testing.T) {
	t.Parallel()
	input := []byte(`{"name":"x","count":"abc"}`)
	if _, _, err := (OmitStruct{}).DecodeFrom(input); err == nil {
		t.Error("expected parse error for non-numeric string")
	}
}

func TestStringTag_unmarshalExpectsString(t *testing.T) {
	t.Parallel()
	input := []byte(`{"name":"x","count":99}`)
	if _, _, err := (OmitStruct{}).DecodeFrom(input); err == nil {
		t.Error("expected error when count is bare number instead of string-wrapped")
	}
}

func TestOmit_roundtrip(t *testing.T) {
	t.Parallel()
	orig := OmitStruct{Name: "alice", Bio: "dev", Score: 9.5, StrCount: 42, Tags: []string{"go", "rust"}}
	out, _ := encode.Marshal(orig)
	got, _, err := OmitStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.Name != orig.Name || got.Bio != orig.Bio || got.Score != orig.Score ||
		got.StrCount != orig.StrCount || len(got.Tags) != len(orig.Tags) {
		t.Errorf("roundtrip: got %+v want %+v", got, orig)
	}
}

// --- json:",string" width variants -----------------------------------------

// StringTagStruct exercises ,string across every numeric width plus bool
// (bool is a no-op: stays bare true/false). *int + ,string lives in
// brokencodegen_test.go.
//
//ggen:generate
type StringTagStruct struct {
	I8  int8    `json:"i8,string"`
	I16 int16   `json:"i16,string"`
	I32 int32   `json:"i32,string"`
	I64 int64   `json:"i64,string"`
	U8  uint8   `json:"u8,string"`
	U16 uint16  `json:"u16,string"`
	U32 uint32  `json:"u32,string"`
	U64 uint64  `json:"u64,string"`
	F32 float32 `json:"f32,string"`
	F64 float64 `json:"f64,string"`
	B   bool    `json:"b,string"`
}

func TestStringTag_AllVariants_marshal(t *testing.T) {
	t.Parallel()
	in := StringTagStruct{
		I8: -8, I16: 16, I32: -32, I64: 64,
		U8: 8, U16: 16, U32: 32, U64: 64,
		F32: 1.25, F64: 2.5, B: true,
	}
	out, err := encode.MarshalString(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{
		`"i8":"-8"`, `"i16":"16"`, `"i32":"-32"`, `"i64":"64"`,
		`"u8":"8"`, `"u16":"16"`, `"u32":"32"`, `"u64":"64"`,
		`"f32":"1.25"`, `"f64":"2.5"`, `"b":true`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
}

func TestStringTag_AllVariants_unmarshal(t *testing.T) {
	t.Parallel()
	in := []byte(`{"i8":"-8","i16":"16","i32":"-32","i64":"64",` +
		`"u8":"8","u16":"16","u32":"32","u64":"64",` +
		`"f32":"1.25","f64":"2.5","b":true}`)
	got, _, err := StringTagStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.I8 != -8 || got.I16 != 16 || got.I32 != -32 || got.I64 != 64 {
		t.Errorf("signed: %+v", got)
	}
	if got.U8 != 8 || got.U16 != 16 || got.U32 != 32 || got.U64 != 64 {
		t.Errorf("unsigned: %+v", got)
	}
	if got.F32 != 1.25 || got.F64 != 2.5 {
		t.Errorf("float: %+v", got)
	}
	if !got.B {
		t.Errorf("bool: %v", got.B)
	}
}

// TestStringTag_JSONSize_NoRealloc lives in jsonsize_test.go.

// BigOmit pins that omitempty never drops a big.Int/Float/Rat: a zero one
// encodes as `0` / `"0"`, which is not a JSON-empty value (v1 never omits a
// struct; jsonv2 omits only null/""/{}/[]). ggen used to skip the zero.
//
//ggen:generate
type BigOmit struct {
	I big.Int   `json:"i,omitempty"`
	F big.Float `json:"f,omitempty"`
	R big.Rat   `json:"r,omitempty"`
}

func TestOmitEmpty_bigTypesNeverOmitted(t *testing.T) {
	t.Parallel()
	out, err := encode.MarshalString(BigOmit{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"i":`, `"f":`, `"r":`} {
		if !strings.Contains(out, want) {
			t.Errorf("zero big value omitted: want %s in %s", want, out)
		}
	}
}
