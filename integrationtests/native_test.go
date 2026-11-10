package integrationtests

//go:generate ../ggen $GOFILE

import (
	"bytes"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"net"
	"net/netip"
	"reflect"
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
	// unix format encodes a number.
	if !strings.Contains(out, `"unixAt":1700000000`) {
		t.Errorf("unix format missing from: %s", out)
	}
	// sec format encodes the duration as a number of seconds.
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

// BareDuration is kept out of NativeTypes because it would fail the
// crossCompat fixture. Without a format tag, ggen encodes a duration as a
// units string such as "1h30m0s" (documented). jsonv2 encodes int64
// nanoseconds.
//
//ggen:generate
type BareDuration struct {
	D time.Duration `json:"d"` // no format tag, so the units string is used
}

// A duration without a format tag marshals as a quoted units string. Both
// quotes must be present, or the output is invalid JSON with a nil error.
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

// A netip zone can hold any bytes. ParseAddr accepts `%q"z`. The zone must be
// JSON-escaped on marshal. Written raw between the quotes, a value ggen
// itself decoded would marshal to invalid JSON with a nil error.
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
	// An address without a zone takes the fast path that writes it raw. The
	// output must stay byte-identical.
	got2, _, _ := NativeTypes{}.DecodeFrom([]byte(`{"addr":"2001:db8::1"}`))
	out2, _ := ggen.Marshal(got2)
	if !strings.Contains(string(out2), `"addr":"2001:db8::1"`) {
		t.Errorf("zone-free addr changed: %s", out2)
	}
}

// An empty wire value (not null) decodes to an empty non-nil slice, like every
// other container. Left alone, the decoders would produce nil here:
// AppendDecode(nil, "") returns nil, and an immediate `]` appends nothing. A
// nil slice marshals as null, so decode then marshal would not give back the
// input.
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

// A null []byte decodes to nil, and a nil []byte marshals as null. An empty
// non-nil one keeps its empty form: "" or [].
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

// ByteArrays checks that [N]byte encodes as a base64 string with a strict
// decoded length, as jsonv2 does. encoding/json v1 emits a number array
// instead and rejects the string form. format:array selects the v1 number
// array. Every other []byte format applies to [N]byte too.
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

	// The length is strict on decode and encode, on both the bytes and the
	// stream path.
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

// R10ByteSliceTuple is a fixed-length array of variable-length byte slices.
// Each element is a []byte, not a [N]byte. It must not inherit the array
// length, or every element that is not exactly N bytes would be rejected,
// including ggen's own marshal output.
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

// R10BPtrByteArray checks pointers to fixed byte arrays, at any pointer depth.
// The pointer adds null handling, and the value is still base64 or the tagged
// encoding. The `[N]byte` treatment is recorded in the field's kind. The
// pointer's leaf must keep that kind and not treat the type as an array of
// numbers.
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
	// null sets the pointer to nil. A payload with the wrong decoded length is
	// rejected.
	nulled, _, err := got.DecodeFrom([]byte(`{"p":null,"q":null}`))
	if err != nil || nulled.P != nil || nulled.Q != nil {
		t.Errorf("null: %v %v %v", nulled.P, nulled.Q, err)
	}
	var le *ggen.LenError
	if _, _, err := (R10BPtrByteArray{}).DecodeFrom([]byte(`{"p":"AQID"}`)); !errors.As(err, &le) {
		t.Errorf("short base64 into *[8]byte: want LenError, got %v", err)
	}
	// omitempty drops a nil pointer.
	if out, err := ggen.Marshal(R10BPtrByteArray{}); err != nil || strings.Contains(string(out), `"o"`) {
		t.Errorf("omitempty: %s, %v", out, err)
	}
}

// R10NetZero checks that a zero netip.Addr, a zero netip.Prefix and a nil
// net.IP marshal as "", as jsonv2 does. "" decodes back to the zero value, the
// same way the types' own UnmarshalText methods do, so ggen accepts its own
// output for an address that was never set. net.IP is a byte slice, so null
// sets it to nil like a []byte.
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
		// A receiver with a value set also takes the zero. "" must
		// overwrite the old value, not skip it.
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
	// Malformed text, and null on the non-pointer types, are still rejected
	// on both paths.
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

// A netip parse failure keeps the bad input in its error message. The stream
// scan returns a string that aliases the Stream's buffer (points into it, no
// copy). So the error must copy that text before the buffer is reused for
// the next payload.
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

// NetIPErrs is decoded without -copy, so the bytes path aliases the input.
//
//ggen:generate
type NetIPErrs struct {
	IP  net.IP            `json:"ip"`
	IPs []net.IP          `json:"ips"`
	M   map[string]net.IP `json:"m"`
}

// AddrError.Text is a copy on every path, so it survives the input being
// reused even where decoded strings alias it.
func TestNetIP_AddrErrorTextDetachedOnBytes(t *testing.T) {
	t.Parallel()
	const bad = "not-an-ip-at-all"
	for _, in := range []string{`{"ip":"` + bad + `"}`, `{"ips":["10.0.0.1","` + bad + `"]}`, `{"m":{"k":"` + bad + `"}}`} {
		src := []byte(in)
		_, _, err := NetIPErrs{}.DecodeFrom(src)
		ae, ok := errors.AsType[*ggen.AddrError](err)
		if !ok {
			t.Fatalf("%s: %v, want a *ggen.AddrError", in, err)
		}
		for i := range src {
			src[i] = 'Z'
		}
		if ae.Text != bad {
			t.Errorf("%s: after scribble Text = %q, want %q", in, ae.Text, bad)
		}
	}
}

type DurAlias = time.Duration

// DurErrs holds a Duration in each position that parses one from text: a
// field, an `=` alias field and a catch-all value.
//
//ggen:generate
type DurErrs struct {
	D     time.Duration            `json:"d"`
	A     DurAlias                 `json:"a"`
	Extra map[string]time.Duration `json:",embed"`
}

// time.ParseDuration keeps the text it was given in its error, and the stream
// scan aliases the Stream's buffer, so the error must hold a copy.
func TestDuration_ErrorDetachedFromBuffer(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 0, 64)
	var s ggen.Stream
	for _, key := range []string{"d", "a", "k"} {
		s.Reset(strings.NewReader(`{"`+key+`":"badbadbad"}`), buf)
		_, err := s.Value[DurErrs]()
		if err == nil {
			t.Fatalf("%s: accepted", key)
		}
		before := err.Error()
		s.Reset(strings.NewReader(`{"`+key+`":"ZZZZZZZZZ"}`), buf)
		s.Value[DurErrs]()
		if after := err.Error(); after != before {
			t.Errorf("%s: error text changed after buffer reuse:\n before %q\n after  %q", key, before, after)
		}
	}
}

// R10Time pins the RFC 3339 checks that jsonv2 applies and time.Parse does
// not, on both RFC 3339 layouts. Encode refuses a year outside [0,9999] and a
// zone hour of 24 or more, because no RFC 3339 parser could read the result
// back. Decode refuses a one-digit hour, a `,` fraction separator and
// out-of-range zone digits.
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

// The RFC 3339 checks quote the text in their errors, and the stream scan
// aliases the Stream's buffer, so the error must hold a copy.
func TestTime_ErrorDetachedFromBuffer(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 0, 64)
	var s ggen.Stream
	for _, c := range [][2]string{
		{"t", "2020-01-01T1:04:05Z"},
		{"t", "2020-01-01T00:00:00+24:00"},
		{"s", "2020-01-01T00:00:00,123Z"},
		{"s", "2020-01-01T00:00:00+23:60"},
	} {
		s.Reset(strings.NewReader(`{"`+c[0]+`":"`+c[1]+`"}`), buf)
		_, err := s.Value[R10Time]()
		if err == nil {
			t.Fatalf("%s %q: accepted", c[0], c[1])
		}
		before := err.Error()
		s.Reset(strings.NewReader(`{"`+c[0]+`":"`+strings.Repeat("Z", len(c[1]))+`"}`), buf)
		s.Value[R10Time]()
		if after := err.Error(); after != before {
			t.Errorf("%s %q: error text changed after buffer reuse:\n before %q\n after  %q", c[0], c[1], before, after)
		}
	}
}

// NetAddrs holds net.IPNet, net.TCPAddr and net.UDPAddr at every position.
// Each is a JSON string: CIDR text for an IPNet, "ip:port" for an address,
// "" for the zero value.
//
//ggen:generate
type NetAddrs struct {
	Net    net.IPNet               `json:"net"`
	TCP    net.TCPAddr             `json:"tcp"`
	UDP    net.UDPAddr             `json:"udp"`
	PNet   *net.IPNet              `json:"pnet"`
	PTCP   *net.TCPAddr            `json:"ptcp"`
	Nets   []net.IPNet             `json:"nets"`
	PAddrs []*net.UDPAddr          `json:"paddrs"`
	ByName map[string]net.TCPAddr  `json:"byname"`
	PByKey map[string]*net.IPNet   `json:"pbykey"`
	Tuple  [2]net.UDPAddr          `json:"tuple"`
	OTCP   net.TCPAddr             `json:"otcp,omitzero"`
	OPUDP  *net.UDPAddr            `json:"opudp,omitempty"`
	ONet   *net.IPNet              `json:"onet,omitempty"`
	ZNet   net.IPNet               `json:"znet,omitzero"`
	EUDP   *net.UDPAddr            `json:"eudp,omitempty"`
	Deep   map[string][]net.IPNet  `json:"deep"`
	Wide   map[string]*net.UDPAddr `json:"wide"`
}

// NetAddrsHTML checks that a zone holding <, > or & is escaped under
// htmlescape.
//
//ggen:generate htmlescape
type NetAddrsHTML struct {
	TCP net.TCPAddr `json:"tcp"`
}

func mustCIDR(s string) net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return *n
}

func TestNetAddrs_wire(t *testing.T) {
	t.Parallel()
	pn := mustCIDR("2001:db8::/32")
	v := NetAddrs{
		Net:    mustCIDR("10.0.0.0/8"),
		TCP:    net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Port: 80},
		UDP:    net.UDPAddr{IP: net.ParseIP("fe80::1"), Port: 53, Zone: "eth0"},
		PNet:   &pn,
		PTCP:   &net.TCPAddr{Port: 8080},
		Nets:   []net.IPNet{mustCIDR("192.168.0.0/16"), mustCIDR("::/0")},
		PAddrs: []*net.UDPAddr{{IP: net.ParseIP("::1"), Port: 1}, nil},
		ByName: map[string]net.TCPAddr{"a": {IP: net.ParseIP("127.0.0.1"), Port: 65535}},
		PByKey: map[string]*net.IPNet{"n": nil},
		Tuple:  [2]net.UDPAddr{{IP: net.ParseIP("10.0.0.1"), Port: 0}, {}},
		Deep:   map[string][]net.IPNet{"d": {mustCIDR("172.16.0.0/12")}},
		Wide:   map[string]*net.UDPAddr{"w": {IP: net.ParseIP("2001:db8::1"), Port: 443}},
	}
	out, err := ggen.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"byname":{"a":"127.0.0.1:65535"},"deep":{"d":["172.16.0.0/12"]},"net":"10.0.0.0/8",` +
		`"nets":["192.168.0.0/16","::/0"],"paddrs":["[::1]:1",null],"pbykey":{"n":null},` +
		`"pnet":"2001:db8::/32","ptcp":":8080","tcp":"1.2.3.4:80","tuple":["10.0.0.1:0",""],` +
		`"udp":"[fe80::1%eth0]:53","wide":{"w":"[2001:db8::1]:443"}}`
	if string(out) != want {
		t.Fatalf("marshal:\n got  %s\n want %s", out, want)
	}
	if n := v.JSONSize(); n < len(out) {
		t.Errorf("JSONSize %d < %d written", n, len(out))
	}
	// The text is what the types' String methods print.
	for _, s := range []string{v.Net.String(), v.TCP.String(), v.UDP.String(), v.PTCP.String()} {
		if !strings.Contains(string(out), `"`+s+`"`) {
			t.Errorf("%q missing from output", s)
		}
	}
	got, _, err := (NetAddrs{}).DecodeFrom(out)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ggen.Marshal(got)
	if err != nil || !bytes.Equal(again, out) {
		t.Fatalf("round trip:\n got  %s (%v)\n want %s", again, err, out)
	}
	for _, chunk := range []int{1, 3, 64} {
		var s ggen.Stream
		s.Reset(&chunkReader{data: out, max: chunk}, make([]byte, 0, 16))
		sgot, err := s.Value[NetAddrs]()
		if err != nil {
			t.Fatalf("stream chunk %d: %v", chunk, err)
		}
		if sout, _ := ggen.Marshal(sgot); !bytes.Equal(sout, out) {
			t.Errorf("stream chunk %d:\n got  %s\n want %s", chunk, sout, out)
		}
	}
}

func TestNetAddrs_zeroAndOmit(t *testing.T) {
	t.Parallel()
	out, err := ggen.Marshal(NetAddrs{})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"byname":null,"deep":null,"net":"","nets":null,"paddrs":null,"pbykey":null,"pnet":null,` +
		`"ptcp":null,"tcp":"","tuple":["",""],"udp":"","wide":null}`
	if string(out) != want {
		t.Fatalf("zero:\n got  %s\n want %s", out, want)
	}
	carried := NetAddrs{Net: mustCIDR("10.0.0.0/8"), TCP: net.TCPAddr{IP: net.ParseIP("1.1.1.1"), Port: 1}}
	got, _, err := carried.DecodeFrom([]byte(`{"net":"","tcp":"","udp":":0"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Net, net.IPNet{}) || !reflect.DeepEqual(got.TCP, net.TCPAddr{}) || !reflect.DeepEqual(got.UDP, net.UDPAddr{}) {
		t.Errorf("\"\" must decode to the zero value: %+v %+v %+v", got.Net, got.TCP, got.UDP)
	}
	// omitempty drops what is written as "": a pointer to a zero value, or an
	// IP that is set but empty. omitzero keeps the latter, which is not Go's
	// zero value.
	set, err := ggen.Marshal(NetAddrs{
		OTCP:  net.TCPAddr{Port: 1},
		OPUDP: &net.UDPAddr{},
		ONet:  &net.IPNet{IP: net.IP{}, Mask: net.IPMask{}},
		ZNet:  net.IPNet{IP: net.IP{}},
		EUDP:  &net.UDPAddr{IP: net.IP{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`"otcp":":1"`, `"znet":""`} {
		if !strings.Contains(string(set), w) {
			t.Errorf("%s missing from %s", w, set)
		}
	}
	for _, k := range []string{`"opudp"`, `"onet"`, `"eudp"`} {
		if strings.Contains(string(set), k) {
			t.Errorf("omitempty kept %s: %s", k, set)
		}
	}
	set, err = ggen.Marshal(NetAddrs{ONet: new(mustCIDR("10.0.0.0/8")), EUDP: &net.UDPAddr{Port: 53}})
	if err != nil || !strings.Contains(string(set), `"onet":"10.0.0.0/8"`) || !strings.Contains(string(set), `"eudp":":53"`) {
		t.Errorf("omitempty dropped a set value: %s (%v)", set, err)
	}
}

func TestNetAddrs_decodeRejects(t *testing.T) {
	t.Parallel()
	for _, p := range []string{
		`{"net":"10.0.0.0"}`,
		`{"net":"10.0.0.0/33"}`,
		`{"net":"10.0.0.0/08"}`,
		`{"net":"fe80::/10%eth0"}`,
		`{"net":null}`,
		`{"net":8}`,
		`{"tcp":"localhost:80"}`,
		`{"tcp":"1.2.3.4"}`,
		`{"tcp":"1.2.3.4:"}`,
		`{"tcp":"1.2.3.4:http"}`,
		`{"tcp":"1.2.3.4:65536"}`,
		`{"tcp":"1.2.3.4:-1"}`,
		`{"tcp":"::1:80"}`,
		`{"tcp":"[1.2.3.4]:80"}`,
		`{"tcp":"1.2.3.4%eth0:80"}`,
		`{"tcp":":"}`,
		`{"tcp":null}`,
		`{"udp":"[::1]"}`,
	} {
		bErr, sErr := decodeBothChunked[NetAddrs](p, 3)
		if bErr == nil || sErr == nil {
			t.Errorf("%s: bytes %v, stream %v", p, bErr, sErr)
		}
	}
	// What the parser accepts beyond the obvious forms.
	for p, want := range map[string]NetAddrs{
		`{"net":"10.1.2.3/8"}`:           {Net: mustCIDR("10.0.0.0/8")},
		`{"tcp":"[::ffff:1.2.3.4]:080"}`: {TCP: net.TCPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 80}},
		`{"udp":":65535"}`:               {UDP: net.UDPAddr{Port: 65535}},
	} {
		got, _, err := (NetAddrs{}).DecodeFrom([]byte(p))
		if err != nil {
			t.Errorf("%s: %v", p, err)
			continue
		}
		if !got.Net.IP.Equal(want.Net.IP) || got.Net.String() != want.Net.String() ||
			got.TCP.String() != want.TCP.String() || got.UDP.String() != want.UDP.String() {
			t.Errorf("%s: got %+v", p, got)
		}
	}
}

func TestNetAddrs_marshalRejects(t *testing.T) {
	t.Parallel()
	for name, v := range map[string]NetAddrs{
		"port_high":     {TCP: net.TCPAddr{Port: 65536}},
		"port_negative": {UDP: net.UDPAddr{IP: net.ParseIP("::1"), Port: -1}},
		"ip_length":     {TCP: net.TCPAddr{IP: net.IP{1, 2, 3}, Port: 1}},
		"zone_on_ipv4":  {TCP: net.TCPAddr{IP: net.ParseIP("1.2.3.4"), Zone: "eth0"}},
		"zone_no_ip":    {UDP: net.UDPAddr{Zone: "eth0"}},
		"mask_not_cidr": {Net: net.IPNet{IP: net.IPv4(10, 0, 0, 0), Mask: net.IPv4Mask(255, 0, 255, 0)}},
		"mask_missing":  {Net: net.IPNet{IP: net.IPv4(10, 0, 0, 0)}},
		"ip_missing":    {Net: net.IPNet{Mask: net.CIDRMask(8, 32)}},
	} {
		if out, err := ggen.Marshal(v); err == nil {
			t.Errorf("%s: wrote %s", name, out)
		}
	}
}

func TestNetAddrs_zoneEscaped(t *testing.T) {
	t.Parallel()
	a := net.TCPAddr{IP: net.ParseIP("fe80::1"), Port: 1, Zone: `a"<b`}
	out, err := ggen.Marshal(NetAddrs{TCP: a})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"tcp":"[fe80::1%a\"<b]:1"`) {
		t.Fatalf("zone not escaped: %s", out)
	}
	got, _, err := (NetAddrs{}).DecodeFrom(out)
	if err != nil || got.TCP.Zone != a.Zone {
		t.Fatalf("round trip: %+v, %v", got.TCP, err)
	}
	h, err := ggen.Marshal(NetAddrsHTML{TCP: a})
	if err != nil || string(h) != `{"tcp":"[fe80::1%a\"\u003cb]:1"}` {
		t.Fatalf("htmlescape: %s, %v", h, err)
	}
	if n := (NetAddrsHTML{TCP: a}).JSONSize(); n < len(h) {
		t.Errorf("JSONSize %d < %d written", n, len(h))
	}
}

// The parse error keeps the bad text. The stream scan aliases its buffer, so
// the error must hold a copy that survives the buffer being reused.
func TestNetAddrs_errorDetachedFromBuffer(t *testing.T) {
	t.Parallel()
	buf := make([]byte, 0, 64)
	var s ggen.Stream
	for _, c := range [][2]string{
		{`{"net":"10.0.0.0/99"}`, `{"net":"ZZ.0.0.0/77"}`},
		{`{"net":"fe80::%eth0/10"}`, `{"net":"ZZZZ::%YYYY/77"}`},
		{`{"tcp":"1.2.3.4:99999"}`, `{"tcp":"X.Y.Z.W:00000"}`},
		{`{"udp":":99999"}`, `{"udp":":77777"}`},
	} {
		s.Reset(strings.NewReader(c[0]), buf)
		_, err := s.Value[NetAddrs]()
		if _, ok := errors.AsType[*ggen.AddrError](err); !ok {
			t.Fatalf("%s: %v, want a *ggen.AddrError", c[0], err)
		}
		before := err.Error()
		s.Reset(strings.NewReader(c[1]), buf)
		s.Value[NetAddrs]()
		if after := err.Error(); after != before {
			t.Errorf("error text changed after buffer reuse:\n before %q\n after  %q", before, after)
		}
	}
}
