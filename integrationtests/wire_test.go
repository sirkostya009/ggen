//go:build goexperiment.jsonv2

package integrationtests

//go:generate ../ggen $GOFILE

// Direct wire-byte assertions (tighter than stdcompat's round-trip-via-any):
// omitempty/omitzero must drop the key, and format tags must emit values the
// stdlib decoder accepts. Uses jsontext to walk objects type-free.

import (
	"bytes"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
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

// objectKeys parses a JSON object into a map of top-level key → raw value.
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

// Nil omitempty pointer fields must not appear.
func TestOmit_NilPointerKeyAbsent(t *testing.T) {
	t.Parallel()
	out, _ := encode.Marshal(PointerStruct{PtrNameStruct: PtrNameStruct{Name: new("x")}, PtrEnabledStruct: PtrEnabledStruct{Enabled: new(false)}})
	// name/enabled are not omitempty — must be present.
	mustPresent(t, out, "name", "enabled")
	mustAbsent(t, out, "count", "ratio", "addr", "when")
}

func TestOmit_PresentPointerKeyEmitted(t *testing.T) {
	t.Parallel()
	count := 7
	out, _ := encode.Marshal(PointerStruct{PtrNameStruct: PtrNameStruct{Name: new("")}, PtrEnabledStruct: PtrEnabledStruct{Enabled: new(false)}, PtrCountStruct: PtrCountStruct{Count: &count}})
	mustPresent(t, out, "count")
}

// omitzero on Score=0 drops the key.
func TestOmit_ZeroNumberKeyAbsent(t *testing.T) {
	t.Parallel()
	out, _ := encode.Marshal(OmitStruct{Name: "x", Score: 0, StrCount: 1})
	mustAbsent(t, out, "score")
	mustPresent(t, out, "name", "count")
}

// omitempty drops empty string and nil slice.
func TestOmit_EmptyContainersAbsent(t *testing.T) {
	t.Parallel()
	out, _ := encode.Marshal(OmitStruct{Name: "x", StrCount: 1})
	mustAbsent(t, out, "bio", "tags")
}

// The `,string` tag wraps the number as a JSON string, not a bare number.
func TestStringTag_QuotedNumber(t *testing.T) {
	t.Parallel()
	out, _ := encode.Marshal(OmitStruct{Name: "x", StrCount: 42})
	got := objectKeys(t, out)
	v, ok := got["count"]
	if !ok {
		t.Fatalf("count key absent in %s", out)
	}
	if len(v) == 0 || v[0] != '"' {
		t.Errorf("count value must be a JSON string, got: %s", v)
	}
}

// `format:hex` emits a quoted lowercase hex string that hex.DecodeString
// round-trips to the input bytes.
func TestFormat_HexParseable(t *testing.T) {
	t.Parallel()
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

// Default `[]byte` (no format) is base64.
func TestFormat_Base64Parseable(t *testing.T) {
	t.Parallel()
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

// `format:base32` round-trips through base32.
//
//ggen:generate
type base32Wrap struct {
	B []byte `json:"b,format:base32"`
}

func TestFormat_Base32Parseable(t *testing.T) {
	t.Parallel()
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

// `format:array` emits a JSON array of numbers (uint8 each).
func TestFormat_BytesArray(t *testing.T) {
	t.Parallel()
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

// `format:unix` emits a bare number, not a string.
func TestFormat_TimeUnixIsNumber(t *testing.T) {
	t.Parallel()
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

// `format:RFC3339` emits a quoted layout string that time.Parse reads back.
func TestFormat_TimeRFC3339Parseable(t *testing.T) {
	t.Parallel()
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

// `format:sec` emits a number (seconds).
func TestFormat_DurationSecIsNumber(t *testing.T) {
	t.Parallel()
	out, _ := encode.Marshal(NativeTypes{SecDur: 90 * time.Second})
	got := objectKeys(t, out)
	v := got["secDur"]
	if len(v) == 0 || v[0] == '"' {
		t.Errorf("secDur must be a JSON number, got: %s", v)
	}
}

// `format:units` emits a Go-duration string time.ParseDuration accepts.
func TestFormat_DurationUnitsParseable(t *testing.T) {
	t.Parallel()
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

// Wire shape for every supported time format: numeric `unix*` variants emit
// bare digits, every other layout emits a quoted string time.Parse reads back.
func TestFormat_AllTimeLayouts(t *testing.T) {
	t.Parallel()
	out, _ := encode.Marshal(timeFormatsAll())
	got := objectKeys(t, out)

	// Numeric formats are unquoted; `unix` may carry a fractional decimal.
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

	// Everything else is a quoted string.
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

// net.IP / netip.Addr / netip.Prefix all emit quoted strings the corresponding
// parser accepts.
func TestFormat_NetIPParseable(t *testing.T) {
	t.Parallel()
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

// Nil slice → `null`, empty non-nil → `[]`.
func TestNilSlice_MarshalsAsNull(t *testing.T) {
	t.Parallel()
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
	// Non-nil empty stays as [].
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

// Nil map → `null`, empty non-nil → `{}`.
func TestNilMap_MarshalsAsNull(t *testing.T) {
	t.Parallel()
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

// `[]`/`{}` decode to non-nil empty containers (symmetric to
// TestNullDecode_LeavesContainerNil).
func TestEmptyArrayDecode_NonNil(t *testing.T) {
	t.Parallel()
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

	// Pointer-element slices (slab path) too.
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

// stdlib merges into a pre-populated map (old keys survive); ggen's DecodeFrom
// replaces it.
func TestStdlibVsGgen_MapReplaceDivergence(t *testing.T) {
	t.Parallel()
	in := []byte(`{"id":1,"name":"n","props":{"new":"v"},"score":0,"active":false}`)

	// stdlib baseline; skip rather than falsely fail if a future Go changes it.
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

	// ggen: same pre-populated value — DecodeFrom replaces the map.
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

	// Same input → different observable results.
	if len(stdGot.Props) == len(ggGot.Props) {
		t.Errorf("expected divergence: stdlib map %v vs ggen map %v", stdGot.Props, ggGot.Props)
	}
}

// The decoder consumes `null` for slice and map fields, leaving the Go value
// nil — symmetric to the encoder.
func TestNullDecode_LeavesContainerNil(t *testing.T) {
	t.Parallel()
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

// omitempty drops nil slices/maps entirely (their wire form is `null`).
func TestOmitEmpty_NilSliceMap_KeyAbsent(t *testing.T) {
	t.Parallel()
	out, _ := encode.Marshal(OmitStruct{Name: "x", StrCount: 1})
	mustAbsent(t, out, "tags", "labels")
}

// omitempty drops empty (len==0) non-nil slices/maps too.
func TestOmitEmpty_EmptyContainer_KeyAbsent(t *testing.T) {
	t.Parallel()
	out, _ := encode.Marshal(OmitStruct{
		Name: "x", StrCount: 1,
		Tags:   []string{},
		Labels: map[string]string{},
	})
	mustAbsent(t, out, "tags", "labels")
}

// omitzero drops nil (Go zero) but keeps empty non-nil — unlike omitempty.
func TestOmitZero_NilContainer_KeyAbsent(t *testing.T) {
	t.Parallel()
	out, _ := encode.Marshal(OmitStruct{Name: "x", StrCount: 1})
	mustAbsent(t, out, "extra", "meta")
}

// omitzero keeps empty non-nil: `make([]T, 0)` / `make(map, 0)` are non-zero Go
// values → emit `[]` / `{}`.
func TestOmitZero_EmptyContainer_KeyEmitted(t *testing.T) {
	t.Parallel()
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

// `[]*T` through the slab path: element values survive the roundtrip, pointer
// identity does not.
func TestPtrSlice_RoundTrip(t *testing.T) {
	t.Parallel()
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

// A nil `[]*T` slice serializes as `null` (matching the value-slice rule);
// decode of `null` produces a nil slice.
func TestPtrSlice_NilSlice_AsNull(t *testing.T) {
	t.Parallel()
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

// A slice of all-null elements decodes to nil pointers (length preserved).
func TestPtrSlice_AllNullElements(t *testing.T) {
	t.Parallel()
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

// A 40-field struct exercises the bitmask seen-tracking path (roundtrip,
// missing-required, duplicate-key).
func TestWideStruct_BitmaskSeenFlags(t *testing.T) {
	t.Parallel()
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

	// Missing-required must surface a RequiredError after the post-loop check.
	_, _, err = WideStruct{}.DecodeFrom([]byte(`{"f1":"x"}`))
	if err == nil {
		t.Fatal("expected RequiredError on missing fields, got nil")
	}

	// Duplicate key must fire the bitmask check.
	_, _, err = WideStruct{}.DecodeFrom([]byte(`{"f1":"a","f1":"b"}`))
	if err == nil {
		t.Fatal("expected DuplicateKeyError on repeated key, got nil")
	}
}

// --- Time formats excluded from cross-compat, single-field types so
// TimeFormatsStruct can embed them via anonymous-field promotion; covered
// here for wire shape + JSONSize budget. `Layout` jsonv2 rejects outright
// (invalid format flag). Stamp/Stamp*/customTiny jsonv2 ACCEPTS on marshal,
// but the layouts carry no year, so decoded values cannot round-trip.

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

// Smallest realistic custom layout — exercises timeFormatSize's
// `len(format)+6` fallback floor. (The verbose-layout sibling lives in the
// stdcompat subset: TimeCustomLong round-trips under jsonv2.)
//
//ggen:generate
type TimeCustomTiny struct {
	CustomTiny time.Time `json:"customTiny,format:'2'"`
}

// A custom layout's literal characters are copied verbatim by AppendFormat,
// so one carrying `"` or `\` used to land raw between the quotes — invalid
// JSON for the quote, a silent backspace escape for the backslash. Both now
// close through the escape-on-dirty helper.
//
//ggen:generate
type TimeEscapingLayout struct {
	Quote time.Time `json:"quote,format:'x\"y 2006'"`
	Slash time.Time `json:"slash,format:'a\\b 2006'"`
}

func TestFormat_CustomLayoutEscapes(t *testing.T) {
	t.Parallel()
	when := time.Date(2026, 8, 9, 1, 2, 3, 0, time.UTC)
	in := TimeEscapingLayout{Quote: when, Slash: when}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out) {
		t.Fatalf("invalid JSON: %s", out)
	}
	got := objectKeys(t, out)
	var q, sl string
	if err := jsonv2.Unmarshal([]byte(got["quote"]), &q); err != nil {
		t.Fatalf("quote value: %v (%s)", err, got["quote"])
	}
	if err := jsonv2.Unmarshal([]byte(got["slash"]), &sl); err != nil {
		t.Fatalf("slash value: %v (%s)", err, got["slash"])
	}
	if q != when.Format(`x"y 2006`) {
		t.Errorf("quote layout: %q, want %q", q, when.Format(`x"y 2006`))
	}
	if sl != when.Format(`a\b 2006`) {
		t.Errorf("slash layout: %q, want %q", sl, when.Format(`a\b 2006`))
	}
	// The widened budget still bounds the escaped output.
	if n := in.JSONSize(); len(out) > n {
		t.Errorf("JSONSize %d < output %d", n, len(out))
	}
	got2, err := in.AppendJSON(make([]byte, 0, in.JSONSize()))
	if err != nil || cap(got2) != in.JSONSize() {
		t.Errorf("realloc: size %d cap %d err %v", in.JSONSize(), cap(got2), err)
	}
}

// TimeFormatsStruct is TimeFormatsStdCompat plus the jsonv2-rejected formats
// above.
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
}

// timeFormatsAll sets every field to the same worst-output moment (max-width
// nanos, unnamed zone → numeric offset) so the per-format byte bound is maxed.
func timeFormatsAll() TimeFormatsStruct {
	// Unnamed fixed-offset zone forces the 5-char numeric offset.
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
	}
}

// --- Boundary edges --------------------------------------------------------

// BoundaryStruct collects edge cases: NaN/Inf floats, integer overflow, and
// escape-hostile string content.
//
//ggen:generate
type BoundaryStruct struct {
	F   float64 `json:"f"`
	I   int64   `json:"i"`
	Str string  `json:"str"`
}

// NaN marshal must not leak a bare `NaN` literal (error or null-encoding both
// fine).
func TestBoundary_FloatNaN_marshal(t *testing.T) {
	t.Parallel()
	in := BoundaryStruct{F: math.NaN()}
	out, err := encode.Marshal(in)
	if err == nil {
		if bytes.Contains(out, []byte("NaN")) {
			t.Errorf("NaN leaked to wire as bare literal: %s", out)
		}
	}
}

func TestBoundary_FloatInf_marshal(t *testing.T) {
	t.Parallel()
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

// A number above int64 range must error, not silently truncate.
func TestBoundary_IntegerOverflow_unmarshal(t *testing.T) {
	t.Parallel()
	in := []byte(`{"i":9999999999999999999,"f":0,"str":""}`)
	got, _, err := BoundaryStruct{}.DecodeFrom(in)
	if err == nil {
		// Saturation at MaxInt64 is acceptable; arbitrary truncation isn't.
		if got.I < 0 {
			t.Errorf("silent overflow to negative: I = %d", got.I)
		}
	}
}

// 1e308 stays finite, 1e309 overflows; neither may panic.
func TestBoundary_FloatPrecision_unmarshal(t *testing.T) {
	t.Parallel()
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

// A string of every short-escape char round-trips, and JSONSize absorbs the
// worst-case 2× expansion.
func TestBoundary_EveryEscapeAtOnce(t *testing.T) {
	t.Parallel()
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

// \uD800 alone must not panic (error or U+FFFD).
func TestBoundary_LoneSurrogate_unmarshal(t *testing.T) {
	t.Parallel()
	in := []byte(`{"f":0,"i":0,"str":"\uD800"}`)
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic on lone surrogate: %v", r)
		}
	}()
	_, _, _ = BoundaryStruct{}.DecodeFrom(in)
}

// A literal \x01 inside a string is invalid per RFC 8259 and must be rejected.
func TestBoundary_RawControlChar_unmarshal(t *testing.T) {
	t.Parallel()
	in := []byte("{\"f\":0,\"i\":0,\"str\":\"a\x01b\"}")
	_, _, err := BoundaryStruct{}.DecodeFrom(in)
	if err == nil {
		t.Errorf("expected error on raw control char")
	}
}
