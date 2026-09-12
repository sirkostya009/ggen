package integrationtests

//go:generate ../ggen $GOFILE

import (
	jsonv2 "encoding/json/v2"
	"errors"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen"
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
	out, _ := ggen.MarshalString(s)
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
	out, _ := ggen.MarshalString(s)
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
	out, _ := ggen.MarshalString(s)
	if strings.Contains(out, "score") {
		t.Errorf("expected score omitted, got %q", out)
	}

	s.Score = 3.14
	out, _ = ggen.MarshalString(s)
	if !strings.Contains(out, `"score":3.14`) {
		t.Errorf("score missing: %q", out)
	}
}

func TestStringTag_marshal(t *testing.T) {
	t.Parallel()
	s := OmitStruct{Name: "x", StrCount: 42}
	out, _ := ggen.MarshalString(s)
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
	out, _ := ggen.Marshal(orig)
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
	out, err := ggen.MarshalString(in)
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

// The quoted text of a `,string` field takes the bare number grammar, range
// and narrowing checks. strconv alone also took Go-literal spellings jsonv2
// rejects, and a NaN or Inf admitted that way could not be marshaled back.
func TestStringTag_quotedTextTakesNumberGrammar(t *testing.T) {
	t.Parallel()
	reject := []struct {
		payload string
		want    error
	}{
		{`{"f64":"NaN"}`, ggen.ErrBadNumber}, {`{"f64":"Infinity"}`, ggen.ErrBadNumber}, {`{"f64":"-Infinity"}`, ggen.ErrBadNumber},
		{`{"f64":"+1"}`, ggen.ErrBadNumber}, {`{"f64":"01"}`, ggen.ErrBadNumber}, {`{"f64":"1_0"}`, ggen.ErrBadNumber},
		{`{"f64":"1."}`, ggen.ErrBadNumber}, {`{"f64":".5"}`, ggen.ErrBadNumber}, {`{"f64":""}`, ggen.ErrBadNumber},
		{`{"f64":" 1"}`, ggen.ErrBadNumber}, {`{"f64":"1 "}`, ggen.ErrBadNumber},
		{`{"i64":"+1"}`, ggen.ErrBadNumber}, {`{"i64":"01"}`, ggen.ErrBadNumber}, {`{"i64":"1.0"}`, ggen.ErrBadNumber},
		{`{"i64":"1e2"}`, ggen.ErrBadNumber}, {`{"u64":"01"}`, ggen.ErrBadNumber}, {`{"u64":"-1"}`, ggen.ErrBadNumber},
		{`{"i8":"300"}`, ggen.ErrNumberOverflow}, {`{"f32":"1e39"}`, ggen.ErrNumberOverflow},
	}
	for _, c := range reject {
		var std StringTagStruct
		if jsonv2.Unmarshal([]byte(c.payload), &std) == nil {
			t.Fatalf("jsonv2 accepts %s — differential premise broken", c.payload)
		}
		if _, _, err := (StringTagStruct{}).DecodeFrom([]byte(c.payload)); !errors.Is(err, c.want) {
			t.Errorf("bytes %s: %v, want %v", c.payload, err, c.want)
		}
		var s ggen.Stream
		s.Reset(strings.NewReader(c.payload), nil)
		if _, err := (StringTagStruct{}).DecodeFromStream(&s); !errors.Is(err, c.want) {
			t.Errorf("stream %s: %v, want %v", c.payload, err, c.want)
		}
	}
	got, _, err := StringTagStruct{}.DecodeFrom([]byte(`{"f64":"-1.5e2","f32":"1.0000000596046448","i64":"-7","u64":"0","i8":"-128"}`))
	if err != nil || got.F64 != -150 || got.F32 != 1.0000001 || got.I64 != -7 || got.U64 != 0 || got.I8 != -128 {
		t.Errorf("well-formed quoted numbers: %+v (%v)", got, err)
	}
}

// BigOmit pins that omitempty never drops a big.Int/Float/Rat: a zero one
// encodes as `0` / `"0"`, which is not a JSON-empty value (v1 never omits a
// struct; jsonv2 omits only null/""/{}/[]). ggen used to skip the zero.
//
//ggen:generate
type BigOmit struct {
	I big.Int   `json:"i"`
	F big.Float `json:"f"`
	R big.Rat   `json:"r"`
}

func TestOmitEmpty_bigTypesNeverOmitted(t *testing.T) {
	t.Parallel()
	out, err := ggen.MarshalString(BigOmit{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"i":`, `"f":`, `"r":`} {
		if !strings.Contains(out, want) {
			t.Errorf("zero big value omitted: want %s in %s", want, out)
		}
	}
}

// R10OmitEmpty pins omitempty's JSON-empty rule (null / "" / [] / {}) on the
// kinds whose empty encoding is not Go-zero-shaped: text kinds that encode
// "", `any`, pointers to empty values and a `[0]T`. url.URL is checked
// against ggen's own string wire (jsonv2 encodes it as an object). `In` is
// the struct case: a struct never counts as empty (omitempty on a struct
// FIELD is a generate-time error), so the pointer omits on nil alone and a
// non-nil one emits `{}`. Raw pins omitzero on a `[N]byte`, which compared
// an array against nil.
//
//ggen:generate
type R10OmitEmpty struct {
	IP   net.IP        `json:"ip,omitempty"`
	Addr *netip.Addr   `json:"addr,omitempty"`
	Pfx  *netip.Prefix `json:"pfx,omitempty"`
	Site *url.URL      `json:"site,omitempty"`
	In   *R10OmitInner `json:"in,omitempty"`
	Any  any           `json:"any,omitempty"`
	PS   *string       `json:"ps,omitempty"`
	PL   *[]int        `json:"pl,omitempty"`
	PPS  **string      `json:"pps,omitempty"`
	Zero [0]int        `json:"zero,omitempty"`
	Raw  [16]byte      `json:"raw,omitzero"`
}

//ggen:generate
type R10OmitInner struct {
	A string `json:"a,omitempty"`
	N int    `json:"n,omitzero"`
}

func TestOmitEmpty_JSONEmptyKinds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		v    R10OmitEmpty
		want string
	}{
		{"zero", R10OmitEmpty{}, "{}"},
		{"any_empty_string", R10OmitEmpty{Any: ""}, "{}"},
		{"any_empty_slice", R10OmitEmpty{Any: []any{}}, "{}"},
		{"any_empty_map", R10OmitEmpty{Any: map[string]any{}}, "{}"},
		{"any_typed_empty_slice", R10OmitEmpty{Any: []int{}}, "{}"},
		{"any_nil_pointer", R10OmitEmpty{Any: (*int)(nil)}, "{}"},
		{"any_zero_number", R10OmitEmpty{Any: 0}, `{"any":0}`},
		{"ptr_empty_string", R10OmitEmpty{PS: new("")}, "{}"},
		{"ptr_empty_slice", R10OmitEmpty{PL: &[]int{}}, "{}"},
		{"ptr_ptr_empty_string", R10OmitEmpty{PPS: new(new(""))}, "{}"},
		{"ptr_string", R10OmitEmpty{PS: new("x")}, `{"ps":"x"}`},
		{"ptr_slice", R10OmitEmpty{PL: &[]int{1}}, `{"pl":[1]}`},
		{"ptr_ptr_string", R10OmitEmpty{PPS: new(new("x"))}, `{"pps":"x"}`},
		{"inner_all_omitted", R10OmitEmpty{In: &R10OmitInner{}}, `{"in":{}}`},
		{"inner_populated", R10OmitEmpty{In: &R10OmitInner{N: 2}}, `{"in":{"n":2}}`},
		{"text_zero", R10OmitEmpty{Addr: &netip.Addr{}, Pfx: &netip.Prefix{}, Site: &url.URL{}}, "{}"},
		{"text_kinds", R10OmitEmpty{
			IP: net.ParseIP("10.0.0.1"), Addr: new(netip.MustParseAddr("::1")),
			Pfx: new(netip.MustParsePrefix("10.0.0.0/8")), Site: &url.URL{Scheme: "https", Host: "x.io"},
		}, `{"addr":"::1","ip":"10.0.0.1","pfx":"10.0.0.0/8","site":"https://x.io"}`},
		{"byte_array_omitzero", R10OmitEmpty{Raw: [16]byte{1}}, `{"raw":"AQAAAAAAAAAAAAAAAAAAAA=="}`},
	}
	for _, c := range cases {
		out, err := ggen.MarshalString(c.v)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if out != c.want {
			t.Errorf("%s: got %s, want %s", c.name, out, c.want)
		}
	}
}
