package integrationtests

//go:generate ../ggen $GOFILE

import (
	"errors"
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
// Slice (`[]**T`) and map (`map[string]**T`) variants are intentionally
// absent: the current codegen has gaps in those paths (slice slab emits
// `*int{}` zero literal; map JSONSize emits `for k, v` with `v` unused
// in the generic struct fallback). See the n-pointer backlog entry in
// CLAUDE.md.
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
