package integrationtests

//go:generate ../ggen $GOFILE

import (
	"strings"
	"testing"
	"time"

	"github.com/sirkostya009/ggen/encode"
)

// PointerStruct exercises optional fields via Go pointers. Each nil pointer
// encodes as JSON null and decodes back to nil.
//
//ggen:generate
type PointerStruct struct {
	Name    *string    `json:"name"`
	Count   *int       `json:"count,omitempty"`
	Ratio   *float64   `json:"ratio,omitempty"`
	Addr    *Address   `json:"addr,omitempty"`
	When    *time.Time `json:"when,omitempty,format:unix"`
	Enabled *bool      `json:"enabled"`
}

func TestPointer_roundtripAllSet(t *testing.T) {
	in := PointerStruct{
		Name:    new("alice"),
		Count:   new(7),
		Ratio:   new(0.5),
		Addr:    &Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"},
		When:    new(time.Unix(1_700_000_000, 0).UTC()),
		Enabled: new(true),
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
	in := PointerStruct{Name: new("bob"), Enabled: new(false)}
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
// Scalar fields route through the encoding/json fallback per-element, so
// the depth doesn't change the codegen shape — verifies parity with `*T`.
//
// Slice (`[]**T`) and map (`map[string]**T`) variants are intentionally
// absent: the current codegen has gaps in those paths (slice slab emits
// `*int{}` zero literal; map JSONSize emits `for k, v` with `v` unused
// in the generic struct fallback). See the n-pointer backlog entry in
// CLAUDE.md.
//
//ggen:generate
type NPtrStruct struct {
	PP   **int      `json:"pp"`
	PPP  ***int     `json:"ppp"`
	PPPP ****string `json:"pppp"`
	Addr **Address  `json:"addr"`
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
		PP:   ppv1,
		PPP:  pppv2,
		PPPP: &ppps,
		Addr: new(&Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"}),
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
