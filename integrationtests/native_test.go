package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/sirkostya009/ggen"
)

// NativeTypes exercises the format tag with native types: time.Time,
// time.Duration, []byte encodings, and net/netip address types.
//
//ggen:generate
type NativeTypes struct {
	CreatedAt time.Time `json:"createdAt"`
	UnixAt    time.Time `json:"unixAt,format:unix"`
	IssuedAt  time.Time `json:"issuedAt,format:RFC3339"`

	SecDur  time.Duration `json:"secDur,format:sec"`
	UnitDur time.Duration `json:"unitDur,format:units"`

	Blob      []byte `json:"blob"`
	HexBlob   []byte `json:"hexBlob,format:hex"`
	ByteArray []byte `json:"byteArray,format:array"`

	LegacyIP net.IP       `json:"legacyIP"`
	Addr     netip.Addr   `json:"addr"`
	Cidr     netip.Prefix `json:"cidr"`
}

func TestNativeTypes_roundtrip(t *testing.T) {
	t.Parallel()
	in := NativeTypes{
		CreatedAt: time.Date(2026, 4, 18, 12, 34, 56, 789000000, time.UTC),
		UnixAt:    time.Unix(1_700_000_000, 0).UTC(),
		IssuedAt:  time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
		SecDur:    90 * time.Second,
		UnitDur:   time.Hour + 30*time.Minute,
		Blob:      []byte("hello"),
		HexBlob:   []byte{0xde, 0xad, 0xbe, 0xef},
		ByteArray: []byte{1, 2, 3},
		LegacyIP:  net.ParseIP("192.0.2.1"),
		Addr:      netip.MustParseAddr("2001:db8::1"),
		Cidr:      netip.MustParsePrefix("10.0.0.0/8"),
	}

	out, _ := ggen.Marshal(in)
	t.Logf("marshaled: %s", out)

	got, _, err := NativeTypes{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}

	if !got.CreatedAt.Equal(in.CreatedAt) {
		t.Errorf("CreatedAt: got %v want %v", got.CreatedAt, in.CreatedAt)
	}
	if !got.UnixAt.Equal(in.UnixAt) {
		t.Errorf("UnixAt: got %v want %v", got.UnixAt, in.UnixAt)
	}
	if !got.IssuedAt.Equal(in.IssuedAt) {
		t.Errorf("IssuedAt: got %v want %v", got.IssuedAt, in.IssuedAt)
	}
	if got.SecDur != in.SecDur {
		t.Errorf("SecDur: got %v want %v", got.SecDur, in.SecDur)
	}
	if got.UnitDur != in.UnitDur {
		t.Errorf("UnitDur: got %v want %v", got.UnitDur, in.UnitDur)
	}
	if !bytes.Equal(got.Blob, in.Blob) {
		t.Errorf("Blob: got %q want %q", got.Blob, in.Blob)
	}
	if !bytes.Equal(got.HexBlob, in.HexBlob) {
		t.Errorf("HexBlob: got %x want %x", got.HexBlob, in.HexBlob)
	}
	if !bytes.Equal(got.ByteArray, in.ByteArray) {
		t.Errorf("ByteArray: got %v want %v", got.ByteArray, in.ByteArray)
	}
	if !got.LegacyIP.Equal(in.LegacyIP) {
		t.Errorf("LegacyIP: got %v want %v", got.LegacyIP, in.LegacyIP)
	}
	if got.Addr != in.Addr {
		t.Errorf("Addr: got %v want %v", got.Addr, in.Addr)
	}
	if got.Cidr != in.Cidr {
		t.Errorf("Cidr: got %v want %v", got.Cidr, in.Cidr)
	}
}

func TestNativeTypes_format(t *testing.T) {
	t.Parallel()
	// Spot-check that format tags change the wire encoding.
	in := NativeTypes{
		UnixAt:    time.Unix(1_700_000_000, 0),
		SecDur:    2 * time.Second,
		HexBlob:   []byte{0xab, 0xcd},
		ByteArray: []byte{7, 8, 9},
	}
	bs, _ := ggen.Marshal(in)
	out := string(bs)
	// unix → number
	if !strings.Contains(out, `"unixAt":1700000000`) {
		t.Errorf("unix format missing from: %s", out)
	}
	// sec duration → number of seconds
	if !strings.Contains(out, `"secDur":2`) {
		t.Errorf("sec duration missing from: %s", out)
	}
	// hex blob
	if !strings.Contains(out, `"hexBlob":"abcd"`) {
		t.Errorf("hex format missing from: %s", out)
	}
	// array blob
	if !strings.Contains(out, `"byteArray":[7,8,9]`) {
		t.Errorf("array format missing from: %s", out)
	}
}

// BareDuration lives outside NativeTypes: ggen's bare-duration default is the
// units string (documented), while jsonv2's is int64 nanos — it would fail the
// crossCompat fixture.
//
//ggen:generate
type BareDuration struct {
	D time.Duration `json:"d"` // no format: → units is the default
}

// Bare duration marshals as a QUOTED units string — the opening quote used to
// be dropped (`{"d":1h30m0s"}`, invalid JSON with a nil error).
func TestDuration_BareIsQuotedUnits(t *testing.T) {
	t.Parallel()
	in := BareDuration{D: time.Hour + 30*time.Minute}
	bs, err := ggen.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"d":"1h30m0s"}`; string(bs) != want {
		t.Errorf("marshal = %s, want %s", bs, want)
	}
	if !json.Valid(bs) {
		t.Errorf("invalid JSON: %s", bs)
	}
	got, _, err := BareDuration{}.DecodeFrom(bs)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.D != in.D {
		t.Errorf("roundtrip: got %v want %v", got.D, in.D)
	}
}

// A netip zone is arbitrary bytes — ParseAddr accepts `%q"z` — and used to
// drop raw between the JSON quotes: a value ggen itself decoded re-marshaled
// to invalid JSON with a nil error.
func TestNetipAddr_ZoneEscaped(t *testing.T) {
	t.Parallel()
	got, _, err := NativeTypes{}.DecodeFrom([]byte(`{"addr":"fe80::1%q\"z"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Addr.Zone() != `q"z` {
		t.Fatalf("zone = %q", got.Addr.Zone())
	}
	out, err := ggen.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out) {
		t.Errorf("invalid JSON: %s", out)
	}
	if !strings.Contains(string(out), `"addr":"fe80::1%q\"z"`) {
		t.Errorf("zone not escaped: %s", out)
	}
	// Zone-free addrs keep the raw fast path byte-identical.
	got2, _, _ := NativeTypes{}.DecodeFrom([]byte(`{"addr":"2001:db8::1"}`))
	out2, _ := ggen.Marshal(got2)
	if !strings.Contains(string(out2), `"addr":"2001:db8::1"`) {
		t.Errorf("zone-free addr changed: %s", out2)
	}
}

// An EMPTY (not null) wire value decodes to an empty non-nil slice, like
// every other container — the decoders naturally produce nil there
// (AppendDecode(nil, "") is nil, an immediate `]` appends nothing), which
// would re-marshal as null and break the round-trip fixed point.
func TestBytes_emptyDecodesNonNil(t *testing.T) {
	t.Parallel()
	payload := []byte(`{"blob":"","hexBlob":"","byteArray":[]}`)
	got, _, err := NativeTypes{}.DecodeFrom(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, c := range []struct {
		name string
		v    []byte
	}{{"Blob", got.Blob}, {"HexBlob", got.HexBlob}, {"ByteArray", got.ByteArray}} {
		if c.v == nil {
			t.Errorf("%s: empty wire decoded to nil, want empty non-nil", c.name)
		}
		if len(c.v) != 0 {
			t.Errorf("%s: len = %d, want 0", c.name, len(c.v))
		}
	}
	out, err := ggen.MarshalString(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"blob":""`, `"hexBlob":""`, `"byteArray":[]`} {
		if !strings.Contains(out, want) {
			t.Errorf("re-marshal lost the empty form: want %s in %s", want, out)
		}
	}

	var st ggen.Stream
	st.Reset(bytes.NewReader(payload), nil)
	sgot, err := NativeTypes{}.DecodeFromStream(&st)
	if err != nil {
		t.Fatalf("stream decode: %v", err)
	}
	if sgot.Blob == nil || sgot.HexBlob == nil || sgot.ByteArray == nil {
		t.Errorf("stream: empty wire decoded to nil: blob=%v hex=%v arr=%v",
			sgot.Blob, sgot.HexBlob, sgot.ByteArray)
	}
}

// null []byte decodes to nil and a nil []byte marshals as null; empty
// non-nil keeps the empty-string / empty-array form.
func TestBytes_nullRoundtrip(t *testing.T) {
	t.Parallel()
	got, _, err := NativeTypes{}.DecodeFrom([]byte(`{"blob":null,"hexBlob":null,"byteArray":null}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Blob != nil || got.HexBlob != nil || got.ByteArray != nil {
		t.Errorf("null should decode to nil: blob=%v hex=%v arr=%v", got.Blob, got.HexBlob, got.ByteArray)
	}

	out, err := ggen.MarshalString(NativeTypes{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"blob":null`, `"hexBlob":null`, `"byteArray":null`} {
		if !strings.Contains(out, want) {
			t.Errorf("nil []byte wire missing %s: %s", want, out)
		}
	}

	out, err = ggen.MarshalString(NativeTypes{Blob: []byte{}, HexBlob: []byte{}, ByteArray: []byte{}})
	if err != nil {
		t.Fatalf("marshal empty: %v", err)
	}
	for _, want := range []string{`"blob":""`, `"hexBlob":""`, `"byteArray":[]`} {
		if !strings.Contains(out, want) {
			t.Errorf("empty []byte wire missing %s: %s", want, out)
		}
	}
}

// [N]byte is a base64 STRING with a strict decoded length — jsonv2 parity
// (encoding/json v1 emits a number array and rejects the string form, so
// ggen's old array shape was unreadable by v2). format:array opts back into
// the v1 shape; every other []byte format applies too.
//
//ggen:generate
type ByteArrays struct {
	B   [4]byte `json:"b"`
	Hex [3]byte `json:"hex,format:hex"`
	Arr [4]byte `json:"arr,format:array"`
}

func TestByteArray_Base64StrictLen(t *testing.T) {
	t.Parallel()
	in := ByteArrays{B: [4]byte{1, 2, 3, 255}, Hex: [3]byte{0xde, 0xad, 0xbe}, Arr: [4]byte{9, 8, 7, 6}}
	out, err := ggen.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"b":"AQID/w=="`)) ||
		!bytes.Contains(out, []byte(`"hex":"deadbe"`)) ||
		!bytes.Contains(out, []byte(`"arr":[9,8,7,6]`)) {
		t.Fatalf("wire: %s", out)
	}

	// jsonv2 reads ggen's default form back.
	var v2 struct {
		B [4]byte `json:"b"`
	}
	if err := jsonv2.Unmarshal(out, &v2); err != nil || v2.B != in.B {
		t.Errorf("jsonv2 cannot read ggen wire: %v %v", v2.B, err)
	}

	back, _, err := ByteArrays{}.DecodeFrom(out)
	if err != nil || back != in {
		t.Fatalf("roundtrip: %+v %v", back, err)
	}
	var st ggen.Stream
	st.Reset(bytes.NewReader(out), make([]byte, 0, 4))
	sb, err := ByteArrays{}.DecodeFromStream(&st)
	if err != nil || sb != in {
		t.Fatalf("stream roundtrip: %+v %v", sb, err)
	}

	// Strict length, both directions, both paths.
	for _, bad := range []string{`{"b":"AQID"}`, `{"b":"AQIDBAU="}`} {
		var le *ggen.LenError
		if _, _, err := (ByteArrays{}).DecodeFrom([]byte(bad)); !errors.As(err, &le) {
			t.Errorf("%s bytes: want LenError, got %v", bad, err)
		}
		var s2 ggen.Stream
		s2.Reset(bytes.NewReader([]byte(bad)), make([]byte, 0, 4))
		if _, err := (ByteArrays{}).DecodeFromStream(&s2); !errors.As(err, &le) {
			t.Errorf("%s stream: want LenError, got %v", bad, err)
		}
	}
}

// R10ByteSliceTuple: a fixed-length tuple of variable-length byte slices.
// The element used to inherit the tuple length and fold onto the `[N]byte`
// base64 path, rejecting every element that was not exactly N bytes —
// ggen's own marshal output included.
//
//ggen:generate
type R10ByteSliceTuple struct {
	AB [2][]byte `json:"ab"`
}

func TestByteArray_TupleOfByteSlices(t *testing.T) {
	t.Parallel()
	in := R10ByteSliceTuple{AB: [2][]byte{[]byte("hello"), []byte("world")}}
	out, err := ggen.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if want, _ := jsonv2.Marshal(in); !bytes.Equal(out, want) {
		t.Fatalf("marshal: ggen %s, jsonv2 %s", out, want)
	}
	got, _, err := R10ByteSliceTuple{}.DecodeFrom(out)
	if err != nil {
		t.Fatalf("bytes: decode of own output %s: %v", out, err)
	}
	var s ggen.Stream
	s.Reset(&chunkReader{data: out, max: 3}, make([]byte, 0, 16))
	sgot, err := R10ByteSliceTuple{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("stream: decode of own output %s: %v", out, err)
	}
	for path, v := range map[string]R10ByteSliceTuple{"bytes": got, "stream": sgot} {
		if !bytes.Equal(v.AB[0], in.AB[0]) || !bytes.Equal(v.AB[1], in.AB[1]) {
			t.Errorf("%s: %q, want %q", path, v.AB, in.AB)
		}
	}
}

// R10BPtrByteArray: a POINTER to a fixed byte array is base64 (or the tagged
// encoding) behind a nullable rung, at any depth — the `[N]byte` fold lives in
// the field's kind, which the pointer leaf must keep instead of re-reading the
// type string as a tuple of numbers.
//
//ggen:generate
type R10BPtrByteArray struct {
	P *[8]byte  `json:"p"`
	Q **[4]byte `json:"q"`
	H *[4]byte  `json:"h,format:hex"`
	A *[3]byte  `json:"a,format:array"`
	O *[2]byte  `json:"o,omitempty"`
}

func TestByteArray_Pointer(t *testing.T) {
	t.Parallel()
	in := `{"a":[1,2,3],"h":"01020304","p":"AQIDBAUGBwg=","q":"AQIDBA=="}`
	got, _, err := R10BPtrByteArray{}.DecodeFrom([]byte(in))
	if err != nil {
		t.Fatalf("bytes decode: %v", err)
	}
	if got.P == nil || *got.P != [8]byte{1, 2, 3, 4, 5, 6, 7, 8} {
		t.Errorf("p = %v", got.P)
	}
	if got.Q == nil || *got.Q == nil || **got.Q != [4]byte{1, 2, 3, 4} {
		t.Errorf("q = %v", got.Q)
	}
	if got.H == nil || *got.H != [4]byte{1, 2, 3, 4} || got.A == nil || *got.A != [3]byte{1, 2, 3} {
		t.Errorf("h = %v, a = %v", got.H, got.A)
	}
	out, err := ggen.Marshal(got)
	if err != nil || string(out) != in {
		t.Fatalf("marshal: %s, %v", out, err)
	}
	var s ggen.Stream
	s.Reset(&chunkReader{data: []byte(in), max: 3}, make([]byte, 0, 16))
	sgot, err := R10BPtrByteArray{}.DecodeFromStream(&s)
	if err != nil {
		t.Fatalf("stream decode: %v", err)
	}
	if sout, err := ggen.Marshal(sgot); err != nil || string(sout) != in {
		t.Fatalf("stream marshal: %s, %v", sout, err)
	}
	// null nils the pointer; a payload of the wrong decoded length is refused.
	nulled, _, err := got.DecodeFrom([]byte(`{"p":null,"q":null}`))
	if err != nil || nulled.P != nil || nulled.Q != nil {
		t.Errorf("null: %v %v %v", nulled.P, nulled.Q, err)
	}
	var le *ggen.LenError
	if _, _, err := (R10BPtrByteArray{}).DecodeFrom([]byte(`{"p":"AQID"}`)); !errors.As(err, &le) {
		t.Errorf("short base64 into *[8]byte: want LenError, got %v", err)
	}
	// omitempty on a nil pointer, and the pointee's base64 when set.
	if out, err := ggen.Marshal(R10BPtrByteArray{}); err != nil || strings.Contains(string(out), `"o"`) {
		t.Errorf("omitempty: %s, %v", out, err)
	}
}

// R10NetZero: the zero netip.Addr / netip.Prefix and a nil net.IP marshal
// as "" (jsonv2's shape), and "" decodes back to the zero value the way the
// types' own UnmarshalText do — ggen used to reject its own output for a
// never-set address. net.IP is a byte slice, so null nils it like []byte.
//
//ggen:generate
type R10NetZero struct {
	Addr netip.Addr   `json:"addr"`
	IP   net.IP       `json:"ip"`
	Pfx  netip.Prefix `json:"pfx"`
}

func TestNetTypes_emptyIsZero(t *testing.T) {
	t.Parallel()
	out, err := ggen.Marshal(R10NetZero{})
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := jsonv2.Marshal(R10NetZero{}); !bytes.Equal(out, want) {
		t.Fatalf("marshal: ggen %s, jsonv2 %s", out, want)
	}
	same := func(a, b R10NetZero) bool {
		return a.Addr == b.Addr && a.Pfx == b.Pfx && bytes.Equal(a.IP, b.IP) && //nolint:staticcheck // byte-exact, not net.IP.Equal
			(a.IP == nil) == (b.IP == nil)
	}
	carried := R10NetZero{Addr: netip.MustParseAddr("10.0.0.1"), IP: net.ParseIP("10.0.0.2"), Pfx: netip.MustParsePrefix("10.0.0.0/8")}
	for _, p := range []string{string(out), `{"addr":""}`, `{"ip":""}`, `{"pfx":""}`, `{"ip":null}`, `{"addr":"::1","ip":"127.0.0.1","pfx":"10.0.0.0/8"}`} {
		var want R10NetZero
		if err := jsonv2.Unmarshal([]byte(p), &want); err != nil {
			t.Fatalf("jsonv2 rejects %s: %v", p, err)
		}
		// A carried receiver takes the zero too — "" must overwrite, not skip.
		got, _, err := carried.DecodeFrom([]byte(p))
		if err != nil || !same(got, want) {
			t.Errorf("bytes %s: %+v (%v), want %+v", p, got, err, want)
		}
		var s ggen.Stream
		s.Reset(&chunkReader{data: []byte(p), max: 3}, make([]byte, 0, 16))
		sgot, err := carried.DecodeFromStream(&s)
		if err != nil || !same(sgot, want) {
			t.Errorf("stream %s: %+v (%v), want %+v", p, sgot, err, want)
		}
	}
	// Malformed text and null on the value types still reject, both paths.
	for _, p := range []string{`{"addr":"x"}`, `{"ip":"x"}`, `{"pfx":"x"}`, `{"pfx":"10.0.0.1"}`, `{"addr":null}`, `{"pfx":null}`} {
		if _, _, err := (R10NetZero{}).DecodeFrom([]byte(p)); err == nil {
			t.Errorf("bytes %s: accepted", p)
		}
		var s ggen.Stream
		s.Reset(&chunkReader{data: []byte(p), max: 3}, make([]byte, 0, 16))
		if _, err := (R10NetZero{}).DecodeFromStream(&s); err == nil {
			t.Errorf("stream %s: accepted", p)
		}
	}
}

// A netip parse failure keeps the offending input in its message; the stream
// scan aliases the buffer, so the retained text must be detached before the
// buffer is recycled for the next payload.
func TestNetip_ErrorDetachedFromBuffer(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 0, 64)
	var s ggen.Stream
	for _, c := range [][2]string{
		{`{"addr":"300.1.1.1"}`, `{"addr":"XXX.9.9.9"}`},
		{`{"cidr":"10.0.0.0/99"}`, `{"cidr":"ZZ.0.0.0/77"}`},
	} {
		s.Reset(strings.NewReader(c[0]), buf)
		_, err := s.Value[NativeTypes]()
		if err == nil {
			t.Fatalf("%s accepted", c[0])
		}
		before := err.Error()
		s.Reset(strings.NewReader(c[1]), buf)
		s.Value[NativeTypes]()
		if after := err.Error(); after != before {
			t.Errorf("error text changed after buffer reuse:\n before %q\n after  %q", before, after)
		}
	}
}

// R10Time pins the RFC 3339 checks jsonv2 applies and time.Parse does not, on
// both RFC 3339 layouts: encode refuses a year outside [0,9999] and a zone
// hour of 24 or more (a string no RFC 3339 parser reads back); decode refuses
// a one-digit hour, a `,` fraction separator and out-of-range zone digits.
//
//ggen:generate
type R10Time struct {
	T time.Time `json:"t"`
	S time.Time `json:"s,format:RFC3339"`
}

func TestTime_RFC3339StrictParity(t *testing.T) {
	t.Parallel()
	for name, tm := range map[string]time.Time{
		"year_10000": time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		"year_-1":    time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC),
		"zone_+24h":  time.Date(2020, 1, 1, 0, 0, 0, 0, time.FixedZone("W", 24*3600)),
	} {
		_, sErr := jsonv2.Marshal(struct {
			T time.Time `json:"t"`
		}{tm})
		if sErr == nil {
			t.Errorf("%s: jsonv2 accepts it; the oracle moved", name)
		}
		for _, v := range []R10Time{{T: tm}, {S: tm}} {
			if out, err := ggen.Marshal(v); err == nil {
				t.Errorf("%s: ggen wrote %s", name, out)
			}
		}
	}
	for _, s := range []string{
		"2020-01-01T00:00:00+24:00",
		"2020-01-01T00:00:00+23:60",
		"2020-01-01T00:00:00,123Z",
		"2020-01-01T1:04:05Z",
	} {
		payload := []byte(`{"t":"` + s + `","s":"` + s + `"}`)
		var std struct {
			T time.Time `json:"t"`
		}
		if err := jsonv2.Unmarshal(payload, &std); err == nil {
			t.Errorf("jsonv2 accepts %q; the oracle moved", s)
		}
		for _, p := range [][]byte{payload, []byte(`{"t":"2020-01-01T00:00:00Z","s":"` + s + `"}`)} {
			if got, _, err := (R10Time{}).DecodeFrom(p); err == nil {
				t.Errorf("bytes: accepted %s as %v", p, got)
			}
			var st ggen.Stream
			st.Reset(bytes.NewReader(p), nil)
			if got, err := (R10Time{}).DecodeFromStream(&st); err == nil {
				t.Errorf("stream: accepted %s as %v", p, got)
			}
		}
	}
	in := R10Time{
		T: time.Date(2026, 9, 7, 1, 2, 3, 456000000, time.FixedZone("", 5*3600+30*60)),
		S: time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC),
	}
	out, err := ggen.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"s":"2026-09-07T01:02:03Z","t":"2026-09-07T01:02:03.456+05:30"}`; string(out) != want {
		t.Errorf("marshal: got %s want %s", out, want)
	}
	got, _, err := R10Time{}.DecodeFrom(out)
	if err != nil {
		t.Fatal(err)
	}
	if !got.T.Equal(in.T) || !got.S.Equal(in.S) {
		t.Errorf("roundtrip: got %v want %v", got, in)
	}
}
