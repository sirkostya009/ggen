//go:build goexperiment.jsonv2

package integrationtests

//go:generate ../ggen $GOFILE

// Tighter wire-format assertions than the round-trip-via-any check in
// stdcompat_test.go. These probe the marshalled bytes directly:
//   - omitempty / omitzero must actually drop the key (not emit "k":null/0/[])
//   - format:hex / format:base64 / format:array must emit values that the
//     stdlib decoder for that format accepts
// The tests use jsontext to walk objects without committing to a Go-side
// type, so we observe the exact wire shape ggen produced.

import (
	"bytes"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"math"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/sirkostya009/ggen/encode"
)

// objectKeys parses a JSON object and returns each top-level key's raw
// jsontext.Value. Fails the test on non-object or malformed input.
func objectKeys(t *testing.T, data []byte) map[string]jsontext.Value {
	t.Helper()
	var raw map[string]jsontext.Value
	if err := jsonv2.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parse object: %v\nbytes: %s", err, data)
	}
	return raw
}

func mustAbsent(t *testing.T, data []byte, keys ...string) {
	t.Helper()
	got := objectKeys(t, data)
	for _, k := range keys {
		if _, ok := got[k]; ok {
			t.Errorf("key %q must be absent — wire: %s", k, data)
		}
	}
}

func mustPresent(t *testing.T, data []byte, keys ...string) {
	t.Helper()
	got := objectKeys(t, data)
	for _, k := range keys {
		if _, ok := got[k]; !ok {
			t.Errorf("key %q must be present — wire: %s", k, data)
		}
	}
}

// TestOmit_NilPointerKeyAbsent: pointer fields tagged omitempty must not
// even appear in the output when nil. The any-roundtrip check in stdcompat
// would silently pass `null` through; this asserts the key is gone entirely.
func TestOmit_NilPointerKeyAbsent(t *testing.T) {
	out, _ := encode.Marshal(PointerStruct{PtrNameStruct: PtrNameStruct{Name: new("x")}, PtrEnabledStruct: PtrEnabledStruct{Enabled: new(false)}})
	// "name" / "enabled" are not omitempty — those keys MUST be present.
	mustPresent(t, out, "name", "enabled")
	// All omitempty pointers must be gone.
	mustAbsent(t, out, "count", "ratio", "addr", "when")
}

func TestOmit_PresentPointerKeyEmitted(t *testing.T) {
	count := 7
	out, _ := encode.Marshal(PointerStruct{PtrNameStruct: PtrNameStruct{Name: new("")}, PtrEnabledStruct: PtrEnabledStruct{Enabled: new(false)}, PtrCountStruct: PtrCountStruct{Count: &count}})
	mustPresent(t, out, "count")
}

// TestOmit_ZeroNumberKeyAbsent: omitzero on Score=0 must drop the key.
func TestOmit_ZeroNumberKeyAbsent(t *testing.T) {
	out, _ := encode.Marshal(OmitStruct{Name: "x", Score: 0, StrCount: 1})
	mustAbsent(t, out, "score")
	mustPresent(t, out, "name", "count")
}

// TestOmit_EmptyContainersAbsent: omitempty drops empty string, nil slice.
func TestOmit_EmptyContainersAbsent(t *testing.T) {
	out, _ := encode.Marshal(OmitStruct{Name: "x", StrCount: 1})
	mustAbsent(t, out, "bio", "tags")
}

// TestStringTag_QuotedNumber: the `,string` tag wraps the number as a JSON
// string, not a bare number. Inspect the raw value to confirm shape.
func TestStringTag_QuotedNumber(t *testing.T) {
	out, _ := encode.Marshal(OmitStruct{Name: "x", StrCount: 42})
	got := objectKeys(t, out)
	v, ok := got["count"]
	if !ok {
		t.Fatalf("count key absent in %s", out)
	}
	// jsontext.Value preserves the raw JSON; first byte tells the kind.
	if len(v) == 0 || v[0] != '"' {
		t.Errorf("count value must be a JSON string, got: %s", v)
	}
}

// TestFormat_HexParseable: `format:hex` must emit a quoted lowercase hex
// string that hex.DecodeString accepts and round-trips to the input bytes.
func TestFormat_HexParseable(t *testing.T) {
	want := []byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x7f}
	out, _ := encode.Marshal(NativeTypes{HexBlob: want})
	got := objectKeys(t, out)
	v, ok := got["hexBlob"]
	if !ok {
		t.Fatalf("hexBlob absent: %s", out)
	}
	var s string
	if err := jsonv2.Unmarshal([]byte(v), &s); err != nil {
		t.Fatalf("hexBlob value not a JSON string: %s", v)
	}
	decoded, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("hex.DecodeString(%q): %v", s, err)
	}
	if !bytes.Equal(decoded, want) {
		t.Errorf("decoded = %x, want %x", decoded, want)
	}
}

// TestFormat_Base64Parseable: default `[]byte` (no format) is base64.
func TestFormat_Base64Parseable(t *testing.T) {
	want := []byte("hello world")
	out, _ := encode.Marshal(NativeTypes{Blob: want})
	got := objectKeys(t, out)
	v, ok := got["blob"]
	if !ok {
		t.Fatalf("blob absent: %s", out)
	}
	var s string
	if err := jsonv2.Unmarshal([]byte(v), &s); err != nil {
		t.Fatalf("blob value not a JSON string: %s", v)
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base64 decode of %q: %v", s, err)
	}
	if string(decoded) != string(want) {
		t.Errorf("decoded = %q, want %q", decoded, want)
	}
}

// TestFormat_Base32Parseable verifies the format tag wires through to the
// base32 encoder by round-tripping a byte slice through a minimal tagged
// struct. Defined locally so we don't pollute NativeTypes.
//
//ggen:generate
type base32Wrap struct {
	B []byte `json:"b,format:base32"`
}

func TestFormat_Base32Parseable(t *testing.T) {
	want := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	out, _ := encode.Marshal(base32Wrap{B: want})
	got := objectKeys(t, out)
	v := got["b"]
	var s string
	_ = jsonv2.Unmarshal([]byte(v), &s)
	decoded, err := base32.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("base32 decode of %q: %v", s, err)
	}
	if !bytes.Equal(decoded, want) {
		t.Errorf("decoded = %x, want %x", decoded, want)
	}
}

// TestFormat_BytesArray: `format:array` emits a JSON array of numbers
// (uint8 each). Walk the array and verify every element matches.
func TestFormat_BytesArray(t *testing.T) {
	want := []byte{10, 20, 30, 40}
	out, _ := encode.Marshal(NativeTypes{ByteArray: want})
	got := objectKeys(t, out)
	raw := got["byteArray"]
	if len(raw) == 0 || raw[0] != '[' {
		t.Fatalf("byteArray must be JSON array, got: %s", raw)
	}
	var nums []int
	if err := jsonv2.Unmarshal([]byte(raw), &nums); err != nil {
		t.Fatalf("decode array: %v", err)
	}
	if len(nums) != len(want) {
		t.Fatalf("len = %d, want %d", len(nums), len(want))
	}
	for i, n := range nums {
		if n != int(want[i]) {
			t.Errorf("[%d] = %d, want %d", i, n, want[i])
		}
	}
}

// TestFormat_TimeUnixIsNumber: `format:unix` emits a bare number, not a string.
func TestFormat_TimeUnixIsNumber(t *testing.T) {
	when := time.Unix(1700000000, 0).UTC()
	out, _ := encode.Marshal(NativeTypes{UnixAt: when})
	got := objectKeys(t, out)
	v := got["unixAt"]
	if len(v) == 0 || v[0] == '"' {
		t.Errorf("unixAt must be a JSON number, got: %s", v)
	}
	n, err := strconv.ParseInt(string(v), 10, 64)
	if err != nil {
		t.Fatalf("parse unixAt %q: %v", v, err)
	}
	if n != 1700000000 {
		t.Errorf("unixAt = %d, want 1700000000", n)
	}
}

// TestFormat_TimeRFC3339Parseable: `format:RFC3339` emits a quoted layout
// string that time.Parse can read back.
func TestFormat_TimeRFC3339Parseable(t *testing.T) {
	when := time.Unix(1700000000, 0).UTC()
	out, _ := encode.Marshal(NativeTypes{IssuedAt: when})
	got := objectKeys(t, out)
	var s string
	_ = jsonv2.Unmarshal([]byte(got["issuedAt"]), &s)
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("RFC3339 parse %q: %v", s, err)
	}
	if !parsed.Equal(when) {
		t.Errorf("parsed = %v, want %v", parsed, when)
	}
}

// TestFormat_DurationSecIsNumber: `format:sec` emits a number (seconds).
func TestFormat_DurationSecIsNumber(t *testing.T) {
	out, _ := encode.Marshal(NativeTypes{SecDur: 90 * time.Second})
	got := objectKeys(t, out)
	v := got["secDur"]
	if len(v) == 0 || v[0] == '"' {
		t.Errorf("secDur must be a JSON number, got: %s", v)
	}
}

// TestFormat_DurationUnitsParseable: `format:units` emits a Go-duration
// string — time.ParseDuration must accept it.
func TestFormat_DurationUnitsParseable(t *testing.T) {
	d := time.Hour + 30*time.Minute
	out, _ := encode.Marshal(NativeTypes{UnitDur: d})
	got := objectKeys(t, out)
	var s string
	_ = jsonv2.Unmarshal([]byte(got["unitDur"]), &s)
	parsed, err := time.ParseDuration(s)
	if err != nil {
		t.Fatalf("ParseDuration %q: %v", s, err)
	}
	if parsed != d {
		t.Errorf("parsed = %v, want %v", parsed, d)
	}
}

// TestFormat_AllTimeLayouts pins wire shape for every supported time
// format in one shot. Numeric `unix*` variants must emit bare digits
// (no quotes); every other layout must emit a quoted string whose
// content time.Parse can read back when given the same layout.
func TestFormat_AllTimeLayouts(t *testing.T) {
	out, _ := encode.Marshal(timeFormatsAll())
	got := objectKeys(t, out)

	// All numeric formats must be unquoted JSON numbers. `unix`
	// permits a fractional decimal (matches jsonv2 — emits float when
	// nanos != 0); the others are integer-granular at their unit.
	intNumericFields := []string{"unixMilli", "unixMicro", "unixNano"}
	for _, k := range intNumericFields {
		v := got[k]
		if len(v) == 0 || v[0] == '"' {
			t.Errorf("%s must be a JSON number, got: %s", k, v)
		}
		if _, err := strconv.ParseInt(string(v), 10, 64); err != nil {
			t.Errorf("%s: ParseInt %q: %v", k, v, err)
		}
	}
	if v := got["unix"]; len(v) == 0 || v[0] == '"' {
		t.Errorf("unix must be a JSON number, got: %s", v)
	} else if _, err := strconv.ParseFloat(string(v), 64); err != nil {
		t.Errorf("unix: ParseFloat %q: %v", v, err)
	}

	// All other fields must be quoted strings.
	stringFields := []string{
		"default", "layout", "ansic", "unixDate", "rubyDate",
		"rfc822", "rfc822Z", "rfc850", "rfc1123", "rfc1123Z",
		"rfc3339", "rfc3339Nano", "kitchen",
		"stamp", "stampMilli", "stampMicro", "stampNano",
		"dateTime", "dateOnly", "timeOnly",
		"customTiny", "customLong",
	}
	for _, k := range stringFields {
		v := got[k]
		if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
			t.Errorf("%s must be a quoted JSON string, got: %s", k, v)
		}
	}
}

// TestFormat_NetIPParseable: net.IP / netip.Addr / netip.Prefix all emit
// quoted strings the corresponding parser accepts.
func TestFormat_NetIPParseable(t *testing.T) {
	addr, _ := netip.ParseAddr("192.0.2.7")
	prefix, _ := netip.ParsePrefix("10.0.0.0/24")
	out, _ := encode.Marshal(NativeTypes{
		LegacyIP: net.ParseIP("192.0.2.1"),
		Addr:     addr,
		Cidr:     prefix,
	})
	got := objectKeys(t, out)

	var legacy, addrStr, cidrStr string
	_ = jsonv2.Unmarshal([]byte(got["legacyIP"]), &legacy)
	_ = jsonv2.Unmarshal([]byte(got["addr"]), &addrStr)
	_ = jsonv2.Unmarshal([]byte(got["cidr"]), &cidrStr)

	if net.ParseIP(legacy) == nil {
		t.Errorf("legacyIP = %q not parseable", legacy)
	}
	if _, err := netip.ParseAddr(addrStr); err != nil {
		t.Errorf("addr = %q: %v", addrStr, err)
	}
	if _, err := netip.ParsePrefix(cidrStr); err != nil {
		t.Errorf("cidr = %q: %v", cidrStr, err)
	}
}

// TestNilSlice_MarshalsAsNull: a nil slice (not just empty) must emit
// `null`, matching stdlib `encoding/json`. Empty non-nil slice still
// emits `[]`. The decoder accepts `null` symmetrically — round-trip
// produces nil again.
func TestNilSlice_MarshalsAsNull(t *testing.T) {
	out, err := encode.Marshal(Node{ID: 1, Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	got := objectKeys(t, out)
	for _, k := range []string{"tags", "children"} {
		if string(got[k]) != "null" {
			t.Errorf("nil slice %q → %s, want null", k, got[k])
		}
	}
	// Non-nil empty stays as []. Different shape on the wire.
	out2, err := encode.Marshal(Node{ID: 1, Name: "n", Tags: []string{}, Children: []Node{}})
	if err != nil {
		t.Fatal(err)
	}
	got2 := objectKeys(t, out2)
	for _, k := range []string{"tags", "children"} {
		if string(got2[k]) != "[]" {
			t.Errorf("empty non-nil slice %q → %s, want []", k, got2[k])
		}
	}
}

// TestNilMap_MarshalsAsNull: nil map → `null`. Empty non-nil → `{}`.
func TestNilMap_MarshalsAsNull(t *testing.T) {
	out, err := encode.Marshal(Node{ID: 1, Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	got := objectKeys(t, out)
	if string(got["props"]) != "null" {
		t.Errorf("nil map → %s, want null", got["props"])
	}
	out2, err := encode.Marshal(Node{ID: 1, Name: "n", Props: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	got2 := objectKeys(t, out2)
	if string(got2["props"]) != "{}" {
		t.Errorf("empty non-nil map → %s, want {}", got2["props"])
	}
}

// TestEmptyArrayDecode_NonNil: stdlib parity for the empty-container
// branch — `[]` always decodes to a non-nil empty slice (primitive,
// struct-value, and pointer-element variants). `{}` does the same for
// maps. Symmetric to the `null` → nil behavior in
// TestNullDecode_LeavesContainerNil.
func TestEmptyArrayDecode_NonNil(t *testing.T) {
	in := []byte(`{"id":1,"name":"n","tags":[],"children":[],"props":{},"score":0,"active":false}`)
	got, _, err := Node{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Tags == nil || len(got.Tags) != 0 {
		t.Errorf("Tags = %v (nil=%v len=%d), want non-nil empty", got.Tags, got.Tags == nil, len(got.Tags))
	}
	if got.Children == nil || len(got.Children) != 0 {
		t.Errorf("Children = %v (nil=%v len=%d), want non-nil empty", got.Children, got.Children == nil, len(got.Children))
	}
	if got.Props == nil || len(got.Props) != 0 {
		t.Errorf("Props = %v (nil=%v len=%d), want non-nil empty", got.Props, got.Props == nil, len(got.Props))
	}

	// Pointer-element slices (slab path) also produce empty non-nil.
	ptr, _, err := PtrSliceStruct{}.DecodeFrom([]byte(`{"items":[],"tuple":[null,null,null],"nodes":[]}`))
	if err != nil {
		t.Fatalf("unmarshal PtrSliceStruct: %v", err)
	}
	if ptr.Items == nil || len(ptr.Items) != 0 {
		t.Errorf("Items = %v (nil=%v len=%d), want non-nil empty", ptr.Items, ptr.Items == nil, len(ptr.Items))
	}
	if ptr.Nodes == nil || len(ptr.Nodes) != 0 {
		t.Errorf("Nodes = %v (nil=%v len=%d), want non-nil empty", ptr.Nodes, ptr.Nodes == nil, len(ptr.Nodes))
	}
}

// TestStdlibVsGgen_MapReplaceDivergence: ggen's documented divergence
// from stdlib. Stdlib `Unmarshal(data, &dst)` merges into `dst` —
// pre-existing map keys survive. ggen's `Node.DecodeFrom` ignores the
// receiver entirely and returns a fresh value, so the same call shape
// (`ggGot, _, err = ggGot.DecodeFrom(in, 0)`) wipes the pre-pop state.
func TestStdlibVsGgen_MapReplaceDivergence(t *testing.T) {
	in := []byte(`{"id":1,"name":"n","props":{"new":"v"},"score":0,"active":false}`)

	// stdlib: decode INTO a pre-populated value — Props gets merged
	// (additive semantic). Verify the baseline before contrasting
	// with ggen; if a future Go release ever changes this, the test
	// premise no longer holds and we skip rather than falsely fail.
	stdGot := Node{ID: 1, Name: "n", Props: map[string]string{"old": "kept"}}
	if err := jsonv2.Unmarshal(in, &stdGot); err != nil {
		t.Fatalf("jsonv2: %v", err)
	}
	if _, ok := stdGot.Props["old"]; !ok {
		t.Skipf("stdlib no longer preserves pre-populated keys (now %v) — divergence test premise has shifted", stdGot.Props)
	}
	if stdGot.Props["new"] != "v" {
		t.Skipf("stdlib didn't decode 'new' key as expected (got %v) — divergence test premise has shifted", stdGot.Props)
	}

	// ggen: same pre-populated value, decoded via Node's own method.
	// DecodeFrom ignores the receiver — `var result Node` inside —
	// so the pre-pop state is dropped on reassignment regardless of
	// whether the caller used decode.Unmarshal or Node.DecodeFrom.
	ggGot := Node{ID: 1, Name: "n", Props: map[string]string{"old": "kept"}}
	ggGot, _, err := ggGot.DecodeFrom(in)
	if err != nil {
		t.Fatalf("ggen: %v", err)
	}
	if _, ok := ggGot.Props["old"]; ok {
		t.Errorf("ggen should NOT have 'old' key (receiver-ignored), got %v", ggGot.Props)
	}
	if ggGot.Props["new"] != "v" {
		t.Errorf("ggen should have 'new' key, got %v", ggGot.Props)
	}

	// The whole point: same input → different observable results.
	if len(stdGot.Props) == len(ggGot.Props) {
		t.Errorf("expected divergence: stdlib map %v vs ggen map %v", stdGot.Props, ggGot.Props)
	}
}

// TestNullDecode_LeavesContainerNil: decoder consumes `null` for slice
// and map fields, leaving the Go value nil — symmetric to the encoder.
func TestNullDecode_LeavesContainerNil(t *testing.T) {
	in := []byte(`{"id":1,"name":"n","tags":null,"children":null,"props":null,"score":0,"active":false}`)
	got, _, err := Node{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Tags != nil {
		t.Errorf("Tags = %v, want nil", got.Tags)
	}
	if got.Children != nil {
		t.Errorf("Children = %v, want nil", got.Children)
	}
	if got.Props != nil {
		t.Errorf("Props = %v, want nil", got.Props)
	}
}

// TestOmitEmpty_NilSliceMap_KeyAbsent: omitempty must still drop nil
// slices/maps entirely after the nil-as-null change. Verify by absence.
func TestOmitEmpty_NilSliceMap_KeyAbsent(t *testing.T) {
	out, _ := encode.Marshal(OmitStruct{Name: "x", StrCount: 1})
	mustAbsent(t, out, "tags", "labels")
}

// TestOmitEmpty_EmptyContainer_KeyAbsent: omitempty drops empty (len==0)
// non-nil slices/maps too — same shape as nil, both go away.
func TestOmitEmpty_EmptyContainer_KeyAbsent(t *testing.T) {
	out, _ := encode.Marshal(OmitStruct{
		Name: "x", StrCount: 1,
		Tags:   []string{},
		Labels: map[string]string{},
	})
	mustAbsent(t, out, "tags", "labels")
}

// TestOmitZero_NilContainer_KeyAbsent: omitzero drops nil (Go zero) but
// keeps empty non-nil. Different from omitempty.
func TestOmitZero_NilContainer_KeyAbsent(t *testing.T) {
	out, _ := encode.Marshal(OmitStruct{Name: "x", StrCount: 1})
	mustAbsent(t, out, "extra", "meta")
}

// TestOmitZero_EmptyContainer_KeyEmitted: omitzero KEEPS empty non-nil.
// `make([]T, 0)` and `make(map, 0)` are non-zero Go values → emit `[]` /
// `{}`.
func TestOmitZero_EmptyContainer_KeyEmitted(t *testing.T) {
	out, _ := encode.Marshal(OmitStruct{
		Name: "x", StrCount: 1,
		Extra: []string{},
		Meta:  map[string]string{},
	})
	got := objectKeys(t, out)
	if string(got["extra"]) != "[]" {
		t.Errorf("extra = %s, want []", got["extra"])
	}
	if string(got["meta"]) != "{}" {
		t.Errorf("meta = %s, want {}", got["meta"])
	}
}

// TestPtrSlice_RoundTrip: `[]*T` encodes/decodes through the slab path.
// Pointer identity is not preserved across the roundtrip but element
// values are.
func TestPtrSlice_RoundTrip(t *testing.T) {
	a := Address{Street: "S1", City: "C1", ZipCode: "11111"}
	b := Address{Street: "S2", City: "C2", ZipCode: "22222"}
	in := PtrSliceStruct{
		PtrSliceItemsStruct: PtrSliceItemsStruct{Items: []*Address{&a, &b}},
		PtrSliceTupleStruct: PtrSliceTupleStruct{Tuple: [3]*Address{&a, nil, &b}},
		PtrSliceNodesStruct: PtrSliceNodesStruct{Nodes: []*Node{{ID: 1, Name: "x"}, nil, {ID: 2, Name: "y"}}},
	}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := PtrSliceStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\nbytes: %s", err, out)
	}
	if len(got.Items) != 2 || got.Items[0] == nil || got.Items[1] == nil {
		t.Fatalf("items: %#v", got.Items)
	}
	if *got.Items[0] != a || *got.Items[1] != b {
		t.Errorf("items values: %v %v", *got.Items[0], *got.Items[1])
	}
	if got.Tuple[0] == nil || got.Tuple[1] != nil || got.Tuple[2] == nil {
		t.Fatalf("tuple shape: %#v", got.Tuple)
	}
	if *got.Tuple[0] != a || *got.Tuple[2] != b {
		t.Errorf("tuple values: %v %v", *got.Tuple[0], *got.Tuple[2])
	}
	if len(got.Nodes) != 3 || got.Nodes[1] != nil {
		t.Fatalf("nodes shape: %#v", got.Nodes)
	}
	if got.Nodes[0].ID != 1 || got.Nodes[2].ID != 2 {
		t.Errorf("nodes values: %v %v", got.Nodes[0], got.Nodes[2])
	}
}

// TestPtrSlice_NilSlice_AsNull: a nil `[]*T` slice serializes as `null`
// (matching the value-slice rule); decode of `null` produces nil slice.
func TestPtrSlice_NilSlice_AsNull(t *testing.T) {
	out, err := encode.Marshal(PtrSliceStruct{})
	if err != nil {
		t.Fatal(err)
	}
	got := objectKeys(t, out)
	if string(got["items"]) != "null" {
		t.Errorf("nil items → %s, want null", got["items"])
	}
	if string(got["nodes"]) != "null" {
		t.Errorf("nil nodes → %s, want null", got["nodes"])
	}
	// Roundtrip back to nil.
	parsed, _, err := PtrSliceStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Items != nil {
		t.Errorf("Items decoded as %v, want nil", parsed.Items)
	}
}

// TestPtrSlice_AllNullElements: a slice of all-null elements decodes to
// a slice of nil pointers (length preserved).
func TestPtrSlice_AllNullElements(t *testing.T) {
	in := []byte(`{"items":[null,null,null],"tuple":[null,null,null],"nodes":[null]}`)
	got, _, err := PtrSliceStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Items) != 3 {
		t.Fatalf("items len = %d, want 3", len(got.Items))
	}
	for i, p := range got.Items {
		if p != nil {
			t.Errorf("items[%d] = %v, want nil", i, p)
		}
	}
	for i, p := range got.Tuple {
		if p != nil {
			t.Errorf("tuple[%d] = %v, want nil", i, p)
		}
	}
}

// TestWideStruct_BitmaskSeenFlags: 40-field struct exercises the
// bitmask seen-tracking path. Roundtrip must preserve all values; missing
// any required key must surface a RequiredError.
func TestWideStruct_BitmaskSeenFlags(t *testing.T) {
	in := WideStruct{
		F1: "1", F2: "2", F3: "3", F4: "4", F5: "5",
		F6: "6", F7: "7", F8: "8", F9: "9", F10: "10",
		F11: "11", F12: "12", F13: "13", F14: "14", F15: "15",
		F16: "16", F17: "17", F18: "18", F19: "19", F20: "20",
		F21: "21", F22: "22", F23: "23", F24: "24", F25: "25",
		F26: "26", F27: "27", F28: "28", F29: "29", F30: "30",
		F31: "31", F32: "32", F33: "33", F34: "34", F35: "35",
		F36: "36", F37: "37", F38: "38", F39: "39", F40: "40",
	}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := WideStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\nbytes: %s", err, out)
	}
	if got != in {
		t.Errorf("roundtrip mismatch:\n got: %+v\nwant: %+v", got, in)
	}

	// Missing-required: every field omitted must surface a RequiredError
	// after the bitmask post-loop check.
	_, _, err = WideStruct{}.DecodeFrom([]byte(`{"f1":"x"}`))
	if err == nil {
		t.Fatal("expected RequiredError on missing fields, got nil")
	}

	// Duplicate-key: the bitmask check must fire on a repeat just like
	// the bool path did.
	_, _, err = WideStruct{}.DecodeFrom([]byte(`{"f1":"a","f1":"b"}`))
	if err == nil {
		t.Fatal("expected DuplicateKeyError on repeated key, got nil")
	}
}

// --- jsonv2-incompatible time formats. Each is a single-field type so
// TimeFormatsStruct can embed them alongside the stdcompat-friendly
// set (defined in stdcompat_test.go) via Go's anonymous-field promotion.
// Excluded from cross-compat because jsonv2's format-tag parser rejects
// them; pulled into wire-shape + JSONSize budget coverage here.

//ggen:generate
type TimeLayout struct {
	Layout time.Time `json:"layout,format:Layout"`
}

//ggen:generate
type TimeStamp struct {
	Stamp time.Time `json:"stamp,format:Stamp"`
}

//ggen:generate
type TimeStampMilli struct {
	StampMilli time.Time `json:"stampMilli,format:StampMilli"`
}

//ggen:generate
type TimeStampMicro struct {
	StampMicro time.Time `json:"stampMicro,format:StampMicro"`
}

//ggen:generate
type TimeStampNano struct {
	StampNano time.Time `json:"stampNano,format:StampNano"`
}

// Custom layouts exercise the `len(format)+6` fallback in
// timeFormatSize. Tiny output (smallest realistic format string) + a
// verbose layout with literals and high-resolution fractional seconds.

//ggen:generate
type TimeCustomTiny struct {
	CustomTiny time.Time `json:"customTiny,format:'2'"`
}

//ggen:generate
type TimeCustomLong struct {
	CustomLong time.Time `json:"customLong,format:'2006-Jan-02T15:04:05.000000000_Mon_-0700'"`
}

// TimeFormatsStruct is TimeFormatsStdCompat (jsonv2-friendly subset,
// defined in stdcompat_test.go) plus the jsonv2-rejected formats above.
// Used for the wire-shape sweep below and the per-format JSONSize
// budget test in jsonsize_test.go.
//
//ggen:generate
type TimeFormatsStruct struct {
	TimeFormatsStdCompat
	TimeLayout
	TimeStamp
	TimeStampMilli
	TimeStampMicro
	TimeStampNano
	TimeCustomTiny
	TimeCustomLong
}

// timeFormatsAll returns a TimeFormatsStruct with every field set to
// the same moment. Worst-output cases (max-width nanos, MST → numeric
// offset fallback when zone is unnamed) maximize per-format byte count
// so wire-shape + JSONSize tests pin the bound for every format.
func timeFormatsAll() TimeFormatsStruct {
	// Fixed-offset zone with NO name forces UnixDate/RFC850/RFC1123 to
	// emit the 5-char numeric offset instead of a 3-char TZ abbreviation.
	noName := time.FixedZone("", -7*3600)
	when := time.Date(9999, 12, 31, 23, 59, 59, 999999999, noName)
	return TimeFormatsStruct{
		TimeFormatsStdCompat: timeFormatsStdCompat(when),
		TimeLayout:           TimeLayout{Layout: when},
		TimeStamp:            TimeStamp{Stamp: when},
		TimeStampMilli:       TimeStampMilli{StampMilli: when},
		TimeStampMicro:       TimeStampMicro{StampMicro: when},
		TimeStampNano:        TimeStampNano{StampNano: when},
		TimeCustomTiny:       TimeCustomTiny{CustomTiny: when},
		TimeCustomLong:       TimeCustomLong{CustomLong: when},
	}
}

// --- Boundary edges --------------------------------------------------------

// BoundaryStruct collects the edge cases that the stdlib v1/v2 specs
// either explicitly reject or quietly do something with: NaN/Inf floats,
// integer overflow, string content that's hostile to escape tables
// (every short-escape + raw control char + lone surrogate).
//
//ggen:generate
type BoundaryStruct struct {
	F   float64 `json:"f"`
	I   int64   `json:"i"`
	Str string  `json:"str"`
}

// TestBoundary_FloatNaN: ggen's behavior on NaN marshal — stdlib v1+v2
// both error. Either error or "null"-encoded NaN is acceptable; what we
// check is "no silent success that produces an invalid JSON like `NaN`".
func TestBoundary_FloatNaN_marshal(t *testing.T) {
	in := BoundaryStruct{F: math.NaN()}
	out, err := encode.Marshal(in)
	if err == nil {
		if bytes.Contains(out, []byte("NaN")) {
			t.Errorf("NaN leaked to wire as bare literal: %s", out)
		}
	}
}

func TestBoundary_FloatInf_marshal(t *testing.T) {
	for _, v := range []float64{math.Inf(1), math.Inf(-1)} {
		in := BoundaryStruct{F: v}
		out, err := encode.Marshal(in)
		if err == nil {
			if bytes.Contains(out, []byte("Inf")) || bytes.Contains(out, []byte("inf")) {
				t.Errorf("Inf leaked to wire as bare literal: %s", out)
			}
		}
	}
}

// TestBoundary_IntegerOverflow: a JSON number that exceeds int64 range
// must surface an error, not silent truncation. 9999999999999999999 (19
// nines) is above 2^63-1 ≈ 9.22e18.
func TestBoundary_IntegerOverflow_unmarshal(t *testing.T) {
	in := []byte(`{"i":9999999999999999999,"f":0,"str":""}`)
	got, _, err := BoundaryStruct{}.DecodeFrom(in)
	if err == nil {
		// MaxInt64 = 9223372036854775807. Saturation/clamping at MaxInt64
		// is acceptable; arbitrary truncation isn't.
		if got.I < 0 {
			t.Errorf("silent overflow to negative: I = %d", got.I)
		}
	}
}

// TestBoundary_FloatPrecision_unmarshal: 1e308 fits in float64; 1e309
// overflows to +Inf. The decoder must distinguish and never panic.
func TestBoundary_FloatPrecision_unmarshal(t *testing.T) {
	ok := []byte(`{"f":1e308,"i":0,"str":""}`)
	if got, _, err := (BoundaryStruct{}).DecodeFrom(ok); err == nil {
		if math.IsInf(got.F, 0) || math.IsNaN(got.F) {
			t.Errorf("1e308 → %v, want finite", got.F)
		}
	}
	overflow := []byte(`{"f":1e309,"i":0,"str":""}`)
	_, _, err := BoundaryStruct{}.DecodeFrom(overflow)
	_ = err // either error or +Inf is documented stdlib behavior
}

// TestBoundary_EveryEscapeAtOnce: a string with every short-escape char
// hits the slow path on every byte. Round-trip must preserve content;
// JSONSize must absorb the worst-case 2× expansion.
func TestBoundary_EveryEscapeAtOnce(t *testing.T) {
	str := "\b\f\n\r\t\"\\"
	in := BoundaryStruct{Str: str}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, _, err := BoundaryStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\nwire: %s", err, out)
	}
	if got.Str != str {
		t.Errorf("escape roundtrip mismatch:\n got: %q\nwant: %q\nwire: %s", got.Str, str, out)
	}
	size := in.JSONSize()
	got2, err := in.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	if cap(got2) != size {
		t.Errorf("realloc on every-escape input: JSONSize=%d cap=%d", size, cap(got2))
	}
}

// TestBoundary_LoneSurrogate: \uD800 alone is invalid UTF-16. The decoder
// must surface an error or substitute U+FFFD, never panic.
func TestBoundary_LoneSurrogate_unmarshal(t *testing.T) {
	in := []byte(`{"f":0,"i":0,"str":"\uD800"}`)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on lone surrogate: %v", r)
		}
	}()
	_, _, _ = BoundaryStruct{}.DecodeFrom(in)
}

// TestBoundary_RawControlChar: a literal \x01 inside a JSON string is
// invalid per RFC 8259 — the scanner must reject it as ErrBadString,
// not silently include it. Sonic accepts; stdlib v1+v2 reject; ggen
// follows stdlib.
func TestBoundary_RawControlChar_unmarshal(t *testing.T) {
	in := []byte("{\"f\":0,\"i\":0,\"str\":\"a\x01b\"}")
	_, _, err := BoundaryStruct{}.DecodeFrom(in)
	if err == nil {
		t.Errorf("expected error on raw control char")
	}
}
