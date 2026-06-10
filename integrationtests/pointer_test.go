package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sirkostya009/ggen/decode"
	"github.com/sirkostya009/ggen/encode"
	"github.com/sirkostya009/ggen/scan"
)

// TestPointer_parseErrorChainsThroughPointee: when a wrong-type value
// trips inside *Address.DecodeFrom, the outer PointerStruct.DecodeFrom
// prefixes its own field name onto the inner ParseError's Field path —
// chaining "addr.street" rather than reporting only the deepest field.
func TestPointer_parseErrorChainsThroughPointee(t *testing.T) {
	_, _, err := (PointerStruct{}).DecodeFrom([]byte(
		`{"name":"x","enabled":true,"addr":{"street":123,"city":"C","zipCode":"12345"}}`))
	if err == nil {
		t.Fatal("expected error")
	}
	var pe *decode.ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("err = %T %v, want *decode.ParseError", err, err)
	}
	if strings.Join(pe.Path, ".") != "addr.street" {
		t.Fatalf("Path = %v; want [addr street]", pe.Path)
	}
	if !errors.Is(err, scan.ErrExpectString) {
		t.Fatalf("errors.Is sentinel mismatch: %v", err)
	}
}

// The single-level `*T` fields split into one struct per pointee kind so a
// regression in (say) the `*time.Time format:unix` size path surfaces at that
// kind rather than buried in the composite. Each carries the same json tag it
// has in the composite (omitempty / format) so the per-field JSONSize cap-guard
// (jsonsize_test.go) exercises the exact emit. Pointee kinds: string, int
// (omitempty), float64 (omitempty), ggen-struct (omitempty), time (omitempty,
// unix), bool (non-omit).

//ggen:generate
type PtrNameStruct struct {
	Name *string `json:"name"`
}

//ggen:generate
type PtrCountStruct struct {
	Count *int `json:"count,omitempty"`
}

//ggen:generate
type PtrRatioStruct struct {
	Ratio *float64 `json:"ratio,omitempty"`
}

//ggen:generate
type PtrAddrStruct struct {
	Addr *Address `json:"addr,omitempty"`
}

//ggen:generate
type PtrWhenStruct struct {
	When *time.Time `json:"when,omitempty,format:unix"`
}

//ggen:generate
type PtrEnabledStruct struct {
	Enabled *bool `json:"enabled"`
}

// PointerStruct exercises optional fields via Go pointers. Each nil pointer
// encodes as JSON null and decodes back to nil. Keeps the composite shape (via
// embedding) so the existing roundtrip/null tests still exercise the full mix
// together, mirroring PtrSliceStruct.
//
//ggen:generate
type PointerStruct struct {
	PtrNameStruct
	PtrCountStruct
	PtrRatioStruct
	PtrAddrStruct
	PtrWhenStruct
	PtrEnabledStruct
}

func TestPointer_roundtripAllSet(t *testing.T) {
	in := PointerStruct{
		PtrNameStruct:    PtrNameStruct{Name: new("alice")},
		PtrCountStruct:   PtrCountStruct{Count: new(7)},
		PtrRatioStruct:   PtrRatioStruct{Ratio: new(0.5)},
		PtrAddrStruct:    PtrAddrStruct{Addr: &Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"}},
		PtrWhenStruct:    PtrWhenStruct{When: new(time.Unix(1_700_000_000, 0).UTC())},
		PtrEnabledStruct: PtrEnabledStruct{Enabled: new(true)},
	}
	out, _ := encode.MarshalString(in)
	got, _, err := PointerStruct{}.DecodeFrom([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if *got.Name != "alice" || *got.Count != 7 || *got.Ratio != 0.5 ||
		got.Addr.City != "Lviv" || !got.When.Equal(*in.When) || *got.Enabled != true {
		t.Errorf("roundtrip mismatch: %+v", got)
	}
}

func TestPointer_nilOmitted(t *testing.T) {
	in := PointerStruct{PtrNameStruct: PtrNameStruct{Name: new("bob")}, PtrEnabledStruct: PtrEnabledStruct{Enabled: new(false)}}
	out, _ := encode.MarshalString(in)
	// omitempty fields (Count/Ratio/Addr/When) are nil → absent.
	for _, absent := range []string{"count", "ratio", "addr", "when"} {
		if strings.Contains(out, `"`+absent+`"`) {
			t.Errorf("expected %q omitted, got %q", absent, out)
		}
	}
	// Non-omit fields (Name, Enabled) present even when non-nil.
	if !strings.Contains(out, `"name":"bob"`) {
		t.Errorf("name missing: %q", out)
	}
	if !strings.Contains(out, `"enabled":false`) {
		t.Errorf("enabled missing: %q", out)
	}
}

func TestPointer_nullRoundtrip(t *testing.T) {
	// Non-omit pointer explicitly null should decode to nil.
	in := []byte(`{"name":null,"enabled":null}`)
	got, _, err := PointerStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Name != nil {
		t.Errorf("expected Name nil, got %v", *got.Name)
	}
	if got.Enabled != nil {
		t.Errorf("expected Enabled nil, got %v", *got.Enabled)
	}
}

// NPtrStruct exercises multi-level pointer indirection (`**T`, `***T`, …).
// Decode parses the leaf into a temp FIRST (no allocation for a value that
// fails to parse), then reuses the receiver's existing pointer prefix and
// allocates only the nil tail with Go 1.26 `new(expr)` (`new(new(v))`);
// encode/JSONSize deref level-by-level so an intermediate nil marshals as
// `null` (stdlib parity). No reflective encoding/json fallback — primitive
// and ggen-struct leaves decode natively. Chain-reuse + parse-failure-leaves-
// -receiver-untouched pinned by `TestMerge_multiLevel*` in merge_test.go.
//
// Container variants (`[]**T`, `[N]**T`, `map[string]*T`, `map[string]**T`)
// decode each element/value through the same cascade — see
// NPtrContainersStruct below. Depth-1 `[]*T` keeps the slab fast path.
//
// Each multi-level depth/leaf split into its own struct so the per-field
// JSONSize cap-guard (jsonsize_test.go) exercises the `else if` nil-ladder at
// that exact depth in isolation: `**int` (2-deep primitive), `***int` (3-deep),
// `****string` (4-deep, string leaf), `**Address` (2-deep ggen-struct leaf).

//ggen:generate
type PtrPPStruct struct {
	PP **int `json:"pp"`
}

//ggen:generate
type PtrPPPStruct struct {
	PPP ***int `json:"ppp"`
}

//ggen:generate
type PtrPPPPStruct struct {
	PPPP ****string `json:"pppp"`
}

//ggen:generate
type PtrAddr2Struct struct {
	Addr **Address `json:"addr"`
}

//ggen:generate
type NPtrStruct struct {
	PtrPPStruct
	PtrPPPStruct
	PtrPPPPStruct
	PtrAddr2Struct
}

func TestNPtr_scalarRoundtrip(t *testing.T) {
	v1 := 42
	pv1 := &v1
	ppv1 := &pv1
	v2 := 7
	pv2 := &v2
	ppv2 := &pv2
	pppv2 := &ppv2
	s := "hello"
	ps := &s
	pps := &ps
	ppps := &pps
	in := NPtrStruct{
		PtrPPStruct:    PtrPPStruct{PP: ppv1},
		PtrPPPStruct:   PtrPPPStruct{PPP: pppv2},
		PtrPPPPStruct:  PtrPPPPStruct{PPPP: &ppps},
		PtrAddr2Struct: PtrAddr2Struct{Addr: new(&Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"})},
	}
	out, _ := encode.MarshalString(in)
	got, _, err := NPtrStruct{}.DecodeFrom([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if got.PP == nil || *got.PP == nil || **got.PP != 42 {
		t.Errorf("PP roundtrip: got %v", got.PP)
	}
	if got.PPP == nil || *got.PPP == nil || **got.PPP == nil || ***got.PPP != 7 {
		t.Errorf("PPP roundtrip: got %v", got.PPP)
	}
	if got.PPPP == nil || *got.PPPP == nil || **got.PPPP == nil || ***got.PPPP == nil || ****got.PPPP != "hello" {
		t.Errorf("PPPP roundtrip: got %v", got.PPPP)
	}
	if got.Addr == nil || *got.Addr == nil || (*got.Addr).City != "Lviv" {
		t.Errorf("Addr roundtrip: got %+v", got.Addr)
	}
}

func TestNPtr_allNull(t *testing.T) {
	in := []byte(`{"pp":null,"ppp":null,"pppp":null,"addr":null}`)
	got, _, err := NPtrStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.PP != nil || got.PPP != nil || got.PPPP != nil || got.Addr != nil {
		t.Errorf("expected all nil, got %+v", got)
	}
}

// TestNPtr_intermediateNilMarshalsNull: a non-nil outer pointer whose inner
// pointer is nil marshals that field as `null` — the encode path derefs
// level-by-level and short-circuits at the first nil, matching stdlib. Decode
// can't produce this shape (it allocates the whole chain), but a hand-built
// value must still encode correctly.
func TestNPtr_intermediateNilMarshalsNull(t *testing.T) {
	in := NPtrStruct{PtrPPStruct: PtrPPStruct{PP: new((*int)(nil))}} // **int → non-nil outer, nil inner
	out, err := encode.MarshalString(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(out, `"pp":null`) {
		t.Errorf("expected \"pp\":null for intermediate-nil pointer, got %q", out)
	}
}

// The slab-allocated `[]*T` and `[N]*T` paths split into one struct per
// container shape so a regression in (say) the array-of-pointer emit
// surfaces at that shape rather than buried in a composite. Mirrors the
// per-format Time* split in stdcompat_test.go.
//
// Slab semantics: element pointers come from a single backing slab
// (`make([]T, 0, cap)` for slices, `make([]T, N)` for arrays) so N
// allocs collapse to ~log(N). nil elements decode to nil pointers (no
// slab slot used).

//ggen:generate
type PtrSliceItemsStruct struct {
	Items []*Address `json:"items"`
}

//ggen:generate
type PtrSliceTupleStruct struct {
	Tuple [3]*Address `json:"tuple"`
}

//ggen:generate
type PtrSliceNodesStruct struct {
	Nodes []*Node `json:"nodes"`
}

// PtrSliceStruct keeps the composite shape so the existing
// per-feature tests still exercise the full mix together.
//
//ggen:generate
type PtrSliceStruct struct {
	PtrSliceItemsStruct
	PtrSliceTupleStruct
	PtrSliceNodesStruct
}

// NPtrContainersStruct pins multi-level pointers INSIDE containers: slice /
// fixed-array elements and map values route each element through the same
// parse-first pointer cascade scalar `**T` fields use (no slab, no
// encoding/json fallback). Map values cover depth 1 and 2 — `map[string]*T`
// previously took a reflective fallback that didn't even compile its
// JSONSize loop.
//
//ggen:generate
type NPtrContainersStruct struct {
	SPP  []**int             `json:"spp"`
	APP  [3]**int            `json:"app"`
	NSPP [][]**int           `json:"nspp"`
	MP   map[string]*int     `json:"mp"`
	MPP  map[string]**int    `json:"mpp"`
	MPA  map[string]*Address `json:"mpa"`
}

func TestNPtr_containersRoundtrip(t *testing.T) {
	in := []byte(`{"spp":[1,null,3],"app":[null,5,null],"nspp":[[7,null]],"mp":{"a":1,"b":null},"mpp":{"x":7,"y":null},"mpa":{"k":{"street":"Main 1","city":"Lviv","zipCode":"79000"},"n":null}}`)
	got, n, err := NPtrContainersStruct{}.DecodeFrom(in)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n != len(in) {
		t.Fatalf("consumed %d of %d", n, len(in))
	}
	if len(got.SPP) != 3 || **got.SPP[0] != 1 || got.SPP[1] != nil || **got.SPP[2] != 3 {
		t.Errorf("SPP = %v", got.SPP)
	}
	if got.APP[0] != nil || **got.APP[1] != 5 || got.APP[2] != nil {
		t.Errorf("APP = %v", got.APP)
	}
	if len(got.NSPP) != 1 || **got.NSPP[0][0] != 7 || got.NSPP[0][1] != nil {
		t.Errorf("NSPP = %v", got.NSPP)
	}
	if *got.MP["a"] != 1 || got.MP["b"] != nil {
		t.Errorf("MP = %v", got.MP)
	}
	if **got.MPP["x"] != 7 || got.MPP["y"] != nil {
		t.Errorf("MPP = %v", got.MPP)
	}
	if got.MPA["k"] == nil || got.MPA["k"].City != "Lviv" || got.MPA["n"] != nil {
		t.Errorf("MPA = %v", got.MPA)
	}

	// Marshal → decode fixed point, within the JSONSize budget.
	size := got.JSONSize()
	out, err := got.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(out) > size {
		t.Errorf("JSONSize under-budget: emitted %d, reserved %d", len(out), size)
	}
	again, _, err := NPtrContainersStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("re-decode: %v\n%s", err, out)
	}
	// Value equality, not byte equality — map fields marshal in random
	// range order, so two independent marshals of the same value differ.
	if !reflect.DeepEqual(got, again) {
		t.Errorf("roundtrip not a fixed point:\n%#v\n%#v", got, again)
	}
}

func TestNPtr_containersStream(t *testing.T) {
	in := []byte(`{"spp":[1,null],"app":[9,null,2],"mp":{"a":4},"mpp":{"z":null}}`)
	var s scan.Stream
	s.Reset(bytes.NewReader(in), make([]byte, 16))
	got, err := NPtrContainersStruct{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("stream decode: %v", err)
	}
	if len(got.SPP) != 2 || **got.SPP[0] != 1 || got.SPP[1] != nil {
		t.Errorf("SPP = %v", got.SPP)
	}
	if **got.APP[0] != 9 || got.APP[1] != nil || **got.APP[2] != 2 {
		t.Errorf("APP = %v", got.APP)
	}
	if *got.MP["a"] != 4 {
		t.Errorf("MP = %v", got.MP)
	}
	if v, ok := got.MPP["z"]; !ok || v != nil {
		t.Errorf("MPP = %v", got.MPP)
	}
}
