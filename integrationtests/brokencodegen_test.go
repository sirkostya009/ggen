//go:build goexperiment.jsonv2 && ggen_brokencodegen

// Pins combinations that currently produce broken generator output. Behind
// `-tags ggen_brokencodegen` so the default test run stays green; tagging
// reproduces the codegen issues and serves as a regression suite once fixed:
//
//   - Pointer-to-exotic types (*big.Int, *uuid.UUID, *netip.Addr, …):
//     the inner decode shadows the outer `v` with a wrong-typed `var v string`.
//   - Slice/map elements of the same exotic kinds — same shadow bug.
//   - `var v big.Int` etc. emitted without the corresponding import.

package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"encoding/json"
	"math"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/sirkostya009/ggen/encode"
)

// --- Pointer-to-exotic types -----------------------------------------------

// PtrExoticStruct holds pointer-wrapped exotic types — one nil-branch test
// per exotic kind.
//
//ggen:generate
type PtrExoticStruct struct {
	Big *big.Int         `json:"big"`
	Flt *big.Float       `json:"flt"`
	Rat *big.Rat         `json:"rat"`
	UID *uuid.UUID       `json:"uid"`
	Raw *json.RawMessage `json:"raw"`
	Dur *time.Duration   `json:"dur,format:units"`
	Adr *netip.Addr      `json:"adr"`
	Pfx *netip.Prefix    `json:"pfx"`
	Url *url.URL         `json:"url"`
}

func TestPtrExotic_AllNil_marshalsAsNull(t *testing.T) {
	out, err := encode.Marshal(PtrExoticStruct{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := objectKeys(t, out)
	for _, k := range []string{"big", "flt", "rat", "uid", "raw", "dur", "adr", "pfx", "url"} {
		if string(got[k]) != "null" {
			t.Errorf("nil ptr field %q → %s, want null", k, got[k])
		}
	}
}

func TestPtrExotic_AllNilRoundtrip(t *testing.T) {
	out, _ := encode.Marshal(PtrExoticStruct{})
	got, _, err := PtrExoticStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\nwire: %s", err, out)
	}
	if got.Big != nil || got.Flt != nil || got.Rat != nil || got.UID != nil ||
		got.Raw != nil || got.Dur != nil || got.Adr != nil || got.Pfx != nil || got.Url != nil {
		t.Errorf("expected all nil after null roundtrip, got %+v", got)
	}
}

func TestPtrExotic_AllPopulated_roundtrip(t *testing.T) {
	bigI, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	bigF := big.NewFloat(3.14159)
	bigR, _ := new(big.Rat).SetString("22/7")
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	raw := json.RawMessage(`{"k":"v"}`)
	dur := 90 * time.Minute
	addr := netip.MustParseAddr("2001:db8::1")
	pfx := netip.MustParsePrefix("10.0.0.0/8")
	u, _ := url.Parse("https://example.com/path?q=1")

	in := PtrExoticStruct{
		Big: bigI, Flt: bigF, Rat: bigR, UID: &id, Raw: &raw,
		Dur: &dur, Adr: &addr, Pfx: &pfx, Url: u,
	}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, _, err := PtrExoticStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\nwire: %s", err, out)
	}
	if got.Big == nil || got.Big.Cmp(bigI) != 0 {
		t.Errorf("Big mismatch: %v", got.Big)
	}
	if got.UID == nil || *got.UID != id {
		t.Errorf("UID mismatch: %v", got.UID)
	}
	if got.Dur == nil || *got.Dur != dur {
		t.Errorf("Dur mismatch: got %v want %v", got.Dur, dur)
	}
	if got.Adr == nil || *got.Adr != addr {
		t.Errorf("Adr mismatch: %v", got.Adr)
	}
	if got.Pfx == nil || *got.Pfx != pfx {
		t.Errorf("Pfx mismatch: %v", got.Pfx)
	}
}

func TestPtrExotic_JSONSize_NoRealloc(t *testing.T) {
	bigI, _ := new(big.Int).SetString(strings.Repeat("9", 50), 10)
	bigF := big.NewFloat(1.79e308)
	bigR, _ := new(big.Rat).SetString("987654321098765432109876543210/123456789012345678901234567890")
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	raw := json.RawMessage(`{"deeply":{"nested":[1,2,3,"abc"]}}`)
	dur := time.Duration(math.MaxInt64)
	addr := netip.MustParseAddr("2001:db8::1")
	pfx := netip.MustParsePrefix("2001:db8::/32")
	u, _ := url.Parse("https://user:pass@host.example.com:8080/a/b/c?k1=v1&k2=v2#frag")

	in := PtrExoticStruct{
		Big: bigI, Flt: bigF, Rat: bigR, UID: &id, Raw: &raw,
		Dur: &dur, Adr: &addr, Pfx: &pfx, Url: u,
	}
	size := in.JSONSize()
	got, err := in.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	if cap(got) != size {
		t.Errorf("realloc: JSONSize=%d cap=%d len=%d", size, cap(got), len(got))
	}
	if len(got) > size {
		t.Errorf("undersized: len=%d > size=%d\nout=%s", len(got), size, got)
	}
}

// --- Slice-of-exotic --------------------------------------------------------

// SliceExoticStruct exercises slice-element decoding for exotic types.
//
//ggen:generate
type SliceExoticStruct struct {
	Times []time.Time     `json:"times"`
	Durs  []time.Duration `json:"durs,format:units"`
	IPs   []net.IP        `json:"ips"`
	Addrs []netip.Addr    `json:"addrs"`
	Bigs  []big.Int       `json:"bigs"`
	UIDs  []uuid.UUID     `json:"uids"`
	Blobs [][]byte        `json:"blobs"`
}

func TestSliceExotic_roundtrip(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	t1 := time.Unix(1700001234, 5_000_000).UTC()
	bigA, _ := new(big.Int).SetString("12345678901234567890", 10)
	bigB, _ := new(big.Int).SetString("999", 10)
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")

	in := SliceExoticStruct{
		Times: []time.Time{t0, t1},
		Durs:  []time.Duration{time.Hour, time.Minute},
		IPs:   []net.IP{net.ParseIP("192.0.2.1"), net.ParseIP("2001:db8::1")},
		Addrs: []netip.Addr{netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("::1")},
		Bigs:  []big.Int{*bigA, *bigB},
		UIDs:  []uuid.UUID{id, uuid.Nil},
		Blobs: [][]byte{[]byte("hello"), []byte("world")},
	}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, _, err := SliceExoticStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\nwire: %s", err, out)
	}
	if len(got.Times) != 2 || !got.Times[0].Equal(t0) || !got.Times[1].Equal(t1) {
		t.Errorf("Times = %v", got.Times)
	}
	if len(got.Durs) != 2 || got.Durs[0] != time.Hour || got.Durs[1] != time.Minute {
		t.Errorf("Durs = %v", got.Durs)
	}
	if len(got.Bigs) != 2 || got.Bigs[0].Cmp(bigA) != 0 || got.Bigs[1].Cmp(bigB) != 0 {
		t.Errorf("Bigs = %v", got.Bigs)
	}
	if len(got.UIDs) != 2 || got.UIDs[0] != id {
		t.Errorf("UIDs = %v", got.UIDs)
	}
	if !bytes.Equal(got.Blobs[0], []byte("hello")) || !bytes.Equal(got.Blobs[1], []byte("world")) {
		t.Errorf("Blobs = %v", got.Blobs)
	}
}

func TestSliceExotic_JSONSize_NoRealloc(t *testing.T) {
	bigA, _ := new(big.Int).SetString(strings.Repeat("9", 30), 10)
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	in := SliceExoticStruct{
		Times: []time.Time{time.Now().UTC(), time.Now().UTC()},
		Durs:  []time.Duration{math.MaxInt64, time.Second},
		IPs:   []net.IP{net.ParseIP("2001:db8::1")},
		Addrs: []netip.Addr{netip.MustParseAddr("2001:db8::1")},
		Bigs:  []big.Int{*bigA},
		UIDs:  []uuid.UUID{id, id, id},
		Blobs: [][]byte{make([]byte, 96), make([]byte, 32)},
	}
	size := in.JSONSize()
	got, err := in.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	if cap(got) != size {
		t.Errorf("realloc: JSONSize=%d cap=%d len=%d\nout=%s", size, cap(got), len(got), got)
	}
	if len(got) > size {
		t.Errorf("undersized: len=%d > size=%d", len(got), size)
	}
}

// --- Map-of-exotic ----------------------------------------------------------

// MapExoticStruct exercises map-value decoding for exotic types.
//
//ggen:generate
type MapExoticStruct struct {
	TimeMap map[string]time.Time       `json:"timeMap"`
	BigMap  map[string]big.Int         `json:"bigMap"`
	UIDMap  map[string]uuid.UUID       `json:"uidMap"`
	RawMap  map[string]json.RawMessage `json:"rawMap"`
	AddrMap map[string]*Address        `json:"addrMap"`
}

func TestMapExotic_roundtrip(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	bigA, _ := new(big.Int).SetString("123456789012345678901234567890", 10)
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	addr := &Address{Street: "Main 1", City: "Lviv", ZipCode: "79000"}

	in := MapExoticStruct{
		TimeMap: map[string]time.Time{"a": t0},
		BigMap:  map[string]big.Int{"x": *bigA},
		UIDMap:  map[string]uuid.UUID{"id1": id},
		RawMap:  map[string]json.RawMessage{"r": json.RawMessage(`{"v":1}`)},
		AddrMap: map[string]*Address{"home": addr},
	}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, _, err := MapExoticStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\nwire: %s", err, out)
	}
	if v, ok := got.TimeMap["a"]; !ok || !v.Equal(t0) {
		t.Errorf("TimeMap[a] = (%v, %v)", v, ok)
	}
	if v, ok := got.BigMap["x"]; !ok || v.Cmp(bigA) != 0 {
		t.Errorf("BigMap[x] = (%v, %v)", v.String(), ok)
	}
	if v, ok := got.UIDMap["id1"]; !ok || v != id {
		t.Errorf("UIDMap[id1] = (%v, %v)", v, ok)
	}
	if v, ok := got.AddrMap["home"]; !ok || v == nil || v.City != "Lviv" {
		t.Errorf("AddrMap[home] = (%+v, %v)", v, ok)
	}
}

func TestMapExotic_JSONSize_NoRealloc(t *testing.T) {
	bigA, _ := new(big.Int).SetString(strings.Repeat("7", 30), 10)
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	in := MapExoticStruct{
		TimeMap: map[string]time.Time{"keyA": time.Now().UTC()},
		BigMap:  map[string]big.Int{"keyB": *bigA},
		UIDMap:  map[string]uuid.UUID{"keyC": id},
		RawMap:  map[string]json.RawMessage{"keyD": json.RawMessage(`{"x":42}`)},
		AddrMap: map[string]*Address{"keyE": {Street: "X", City: "Y", ZipCode: "12345"}},
	}
	size := in.JSONSize()
	got, err := in.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	if cap(got) != size {
		t.Errorf("realloc: JSONSize=%d cap=%d len=%d", size, cap(got), len(got))
	}
	if len(got) > size {
		t.Errorf("undersized: len=%d > size=%d", len(got), size)
	}
}

// --- Fixed-array of exotic --------------------------------------------------

// TupleExoticStruct exercises the strict [N]T tuple emitter for exotic
// element kinds (non-trivial per-element decode).
//
//ggen:generate
type TupleExoticStruct struct {
	TimePair   [2]time.Time `json:"timePair"`
	UIDTriple  [3]uuid.UUID `json:"uidTriple"`
	BigPair    [2]big.Int   `json:"bigPair"`
	PtrStrPair [2]*string   `json:"ptrStrPair"`
}

func TestTupleExotic_roundtrip(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	t1 := time.Unix(1700001234, 0).UTC()
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	bigA, _ := new(big.Int).SetString("12345", 10)
	bigB, _ := new(big.Int).SetString("67890", 10)
	a, b := "hello", "world"

	in := TupleExoticStruct{
		TimePair:   [2]time.Time{t0, t1},
		UIDTriple:  [3]uuid.UUID{id, uuid.Nil, id},
		BigPair:    [2]big.Int{*bigA, *bigB},
		PtrStrPair: [2]*string{&a, nil},
	}
	out, err := encode.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, _, err := TupleExoticStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\nwire: %s", err, out)
	}
	if !got.TimePair[0].Equal(t0) || !got.TimePair[1].Equal(t1) {
		t.Errorf("TimePair = %v", got.TimePair)
	}
	if got.UIDTriple[0] != id || got.UIDTriple[2] != id {
		t.Errorf("UIDTriple = %v", got.UIDTriple)
	}
	if got.BigPair[0].Cmp(bigA) != 0 || got.BigPair[1].Cmp(bigB) != 0 {
		t.Errorf("BigPair = %v", got.BigPair)
	}
	if got.PtrStrPair[0] == nil || *got.PtrStrPair[0] != a {
		t.Errorf("PtrStrPair[0] = %v", got.PtrStrPair[0])
	}
	if got.PtrStrPair[1] != nil {
		t.Errorf("PtrStrPair[1] = %v, want nil", got.PtrStrPair[1])
	}
	_ = b
}

func TestTupleExotic_JSONSize_NoRealloc(t *testing.T) {
	t0 := time.Now().UTC()
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	bigA, _ := new(big.Int).SetString(strings.Repeat("9", 60), 10)
	a := strings.Repeat("x", 32)
	in := TupleExoticStruct{
		TimePair:   [2]time.Time{t0, t0},
		UIDTriple:  [3]uuid.UUID{id, id, id},
		BigPair:    [2]big.Int{*bigA, *bigA},
		PtrStrPair: [2]*string{&a, &a},
	}
	size := in.JSONSize()
	got, err := in.AppendJSON(make([]byte, 0, size))
	if err != nil {
		t.Fatalf("AppendJSON: %v", err)
	}
	if cap(got) != size {
		t.Errorf("realloc: JSONSize=%d cap=%d len=%d", size, cap(got), len(got))
	}
	if len(got) > size {
		t.Errorf("undersized: len=%d > size=%d", len(got), size)
	}
}

// --- Pointer-to-container --------------------------------------------------

// PtrContainerStruct exercises *[]T / *map[K]V combos — codegen emits the
// container reset against the pointer instead of dereferencing it.
//
//ggen:generate
type PtrContainerStruct struct {
	OptInts *[]int             `json:"optInts"`
	OptMap  *map[string]string `json:"optMap"`
	OptOmit *[]int             `json:"optOmit,omitempty"`
	OptStrs *[]string          `json:"optStrs"`
}

func TestPtrContainer_NilEmitsNull(t *testing.T) {
	out, err := encode.Marshal(PtrContainerStruct{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	gotMap := objectKeys(t, out)
	for _, k := range []string{"optInts", "optMap", "optStrs"} {
		if string(gotMap[k]) != "null" {
			t.Errorf("nil %q → %s, want null", k, gotMap[k])
		}
	}
	if _, present := gotMap["optOmit"]; present {
		t.Errorf("omitempty key must be absent when nil, wire: %s", out)
	}
}

func TestPtrContainer_PopulatedRoundtrip(t *testing.T) {
	ints := []int{1, 2, 3}
	m := map[string]string{"a": "b"}
	strs := []string{"x", "y"}
	in := PtrContainerStruct{
		OptInts: &ints, OptMap: &m, OptStrs: &strs,
	}
	out, _ := encode.Marshal(in)
	got, _, err := PtrContainerStruct{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\nwire: %s", err, out)
	}
	if got.OptInts == nil || !reflect.DeepEqual(*got.OptInts, ints) {
		t.Errorf("OptInts = %v", got.OptInts)
	}
	if got.OptMap == nil || (*got.OptMap)["a"] != "b" {
		t.Errorf("OptMap = %v", got.OptMap)
	}
	if got.OptStrs == nil || !reflect.DeepEqual(*got.OptStrs, strs) {
		t.Errorf("OptStrs = %v", got.OptStrs)
	}
}

// --- *T + json:",string" combo ---------------------------------------------

// PtrStringTagStruct exercises *int with ,string — codegen emits
// `result.PtrI = *int(n)` (the deref was meant to be a cast).
//
//ggen:generate
type PtrStringTagStruct struct {
	PtrI *int `json:"ptrI,omitempty,string"`
}

func TestPtrStringTag_roundtrip(t *testing.T) {
	pi := 42
	in := PtrStringTagStruct{PtrI: &pi}
	out, err := encode.MarshalString(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want := `"ptrI":"42"`; !strings.Contains(out, want) {
		t.Errorf("missing %s in %s", want, out)
	}
	got, _, err := PtrStringTagStruct{}.DecodeFrom([]byte(out))
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.PtrI == nil || *got.PtrI != 42 {
		t.Errorf("PtrI = %v", got.PtrI)
	}
}

// FuzzPtrExoticNoPanic: random bytes through PtrExoticStruct stay panic-free.
func FuzzPtrExoticNoPanic(f *testing.F) {
	for _, s := range [][]byte{
		[]byte(`{"big":null,"flt":null,"rat":null,"uid":null,"raw":null,"dur":null,"adr":null,"pfx":null,"url":null}`),
		[]byte(`{"big":1}`),
		[]byte(`{"uid":"550e8400-e29b-41d4-a716-446655440000"}`),
		[]byte(`{"raw":{"k":"v"}}`),
		[]byte(`{}`),
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic on PtrExoticStruct input %q: %v", data, r)
			}
		}()
		_, _, _ = PtrExoticStruct{}.DecodeFrom(data)
	})
}

// --- Cross-compat tests for the broken-codegen structs --------------------

func TestStdCompat_PtrExoticStruct(t *testing.T) {
	bigI, _ := new(big.Int).SetString("12345678901234567890", 10)
	bigF := big.NewFloat(2.5)
	bigR, _ := new(big.Rat).SetString("22/7")
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	raw := json.RawMessage(`{"k":"v"}`)
	dur := time.Hour
	addr := netip.MustParseAddr("10.0.0.1")
	pfx := netip.MustParsePrefix("10.0.0.0/8")
	// *url.URL diverges from jsonv2; tested in TestPtrExotic_AllPopulated_roundtrip.
	crossCompat(t, PtrExoticStruct{
		Big: bigI, Flt: bigF, Rat: bigR, UID: &id, Raw: &raw,
		Dur: &dur, Adr: &addr, Pfx: &pfx,
	})
	crossCompat(t, PtrExoticStruct{})
}

func TestStdCompat_SliceExoticStruct(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	bigA, _ := new(big.Int).SetString("99999", 10)
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	crossCompat(t, SliceExoticStruct{
		Times: []time.Time{t0},
		Durs:  []time.Duration{time.Hour},
		IPs:   []net.IP{net.ParseIP("192.0.2.1")},
		Addrs: []netip.Addr{netip.MustParseAddr("10.0.0.1")},
		Bigs:  []big.Int{*bigA},
		UIDs:  []uuid.UUID{id},
		Blobs: [][]byte{[]byte("hello")},
	})
	crossCompat(t, SliceExoticStruct{
		Times: []time.Time{},
		Durs:  []time.Duration{},
		IPs:   []net.IP{},
		Addrs: []netip.Addr{},
		Bigs:  []big.Int{},
		UIDs:  []uuid.UUID{},
		Blobs: [][]byte{},
	})
}

func TestStdCompat_MapExoticStruct(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	bigA, _ := new(big.Int).SetString("987654321", 10)
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	crossCompat(t, MapExoticStruct{
		TimeMap: map[string]time.Time{"a": t0},
		BigMap:  map[string]big.Int{"b": *bigA},
		UIDMap:  map[string]uuid.UUID{"c": id},
		RawMap:  map[string]json.RawMessage{"d": json.RawMessage(`{"v":1}`)},
		AddrMap: map[string]*Address{"home": {Street: "S", City: "C", ZipCode: "12345"}},
	})
}

func TestStdCompat_TupleExoticStruct(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	id := uuid.MustParse("550e8400-e29b-41d4-a716-446655440000")
	bigA, _ := new(big.Int).SetString("100", 10)
	a, b := "hello", "world"
	crossCompat(t, TupleExoticStruct{
		TimePair:   [2]time.Time{t0, t0},
		UIDTriple:  [3]uuid.UUID{id, id, id},
		BigPair:    [2]big.Int{*bigA, *bigA},
		PtrStrPair: [2]*string{&a, &b},
	})
}

func TestStdCompat_PtrContainerStruct(t *testing.T) {
	ints := []int{1, 2, 3}
	m := map[string]string{"a": "b"}
	strs := []string{"x", "y"}
	crossCompat(t, PtrContainerStruct{
		OptInts: &ints, OptMap: &m, OptStrs: &strs,
	})
	crossCompat(t, PtrContainerStruct{})
}
