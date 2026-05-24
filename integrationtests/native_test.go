package integrationtests

import (
	"bytes"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/sirkostya009/ggen/encode"
)

// NativeTypes exercises the jsonv2 format tag with native Go types:
// time.Time (various layouts), time.Duration (various units), []byte (various
// encodings), and net/netip address types.
//
//ggen:generate
type NativeTypes struct {
	// time.Time in RFC3339Nano (default when format unset)
	CreatedAt time.Time `json:"createdAt"`
	// time.Time as a unix timestamp (number)
	UnixAt time.Time `json:"unixAt,format:unix"`
	// time.Time as RFC3339 (named layout)
	IssuedAt time.Time `json:"issuedAt,format:RFC3339"`

	// time.Duration encoded as seconds float
	SecDur time.Duration `json:"secDur,format:sec"`
	// time.Duration encoded as "1h30m" string (time.Duration.String)
	UnitDur time.Duration `json:"unitDur,format:units"`

	// []byte encoded as standard base64 (default)
	Blob []byte `json:"blob"`
	// []byte encoded as lowercase hex
	HexBlob []byte `json:"hexBlob,format:hex"`
	// []byte encoded as a JSON array of numbers
	ByteArray []byte `json:"byteArray,format:array"`

	// net.IP encoded as "192.0.2.1" or "::1"
	LegacyIP net.IP `json:"legacyIP"`
	// netip.Addr (modern value type)
	Addr netip.Addr `json:"addr"`
	// netip.Prefix ("10.0.0.0/8")
	Cidr netip.Prefix `json:"cidr"`
}

func TestNativeTypes_roundtrip(t *testing.T) {
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

	out, _ := encode.Marshal(in)
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
	// Spot-check that the format tag actually changes the wire encoding.
	in := NativeTypes{
		UnixAt:    time.Unix(1_700_000_000, 0),
		SecDur:    2 * time.Second,
		HexBlob:   []byte{0xab, 0xcd},
		ByteArray: []byte{7, 8, 9},
	}
	bs, _ := encode.Marshal(in)
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
