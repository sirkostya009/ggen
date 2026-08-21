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
