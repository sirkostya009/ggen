package thirdparty

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/sirkostya009/ggen/integrationtests/thirdparty/internal/hidden"
)

// ValidateUpper rejects strings that contain lowercase ASCII letters. Other
// packages use it as `pipe:"@thirdparty.ValidateUpper"` to test @pkg.Func
// references.
func ValidateUpper(s string) error {
	for i := range len(s) {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			return fmt.Errorf("must be uppercase: %q", s)
		}
	}
	return nil
}

// IsShort is a cross-package predicate validator.
func IsShort(s string) bool { return len(s) <= 3 }

// NoBang is a cross-package mod of the (T, bool) form: a '!' fails it.
func NoBang(s string) (string, bool) { return s, !strings.Contains(s, "!") }

// PrefixHash is a cross-package mod that cannot fail. It maps T to T.
func PrefixHash(s string) string { return "#" + s }

// ParseNonEmpty is a cross-package mod that can fail. It maps T to (T, error).
// Empty input is a parse error, not a validation error.
func ParseNonEmpty(s string) (string, error) {
	if s == "" {
		return "", errors.New("empty value")
	}
	return s, nil
}

// External is a plain struct with no generated methods, so ggen must fall back
// to encoding/json for it.
type External struct {
	Key   string `json:"key"`
	Value int    `json:"value"`
}

// Tagged encodes as the text `"name#tag"` through TextMarshaler and
// TextUnmarshaler. So generated code calls those methods instead of
// json.Marshal and json.Unmarshal.
type Tagged struct {
	Name string
	Tag  string
}

func (t Tagged) MarshalText() ([]byte, error) {
	return []byte(t.Name + "#" + t.Tag), nil
}

func (t *Tagged) UnmarshalText(b []byte) error {
	s := string(b)
	before, after, ok := strings.Cut(s, "#")
	if !ok {
		return errors.New("tagged: missing '#' separator")
	}
	t.Name = before
	t.Tag = after
	return nil
}

// QuotedText's text form is whatever bytes the caller set, and may contain `"`
// or `\`. It tests that marshal through TextAppender JSON-escapes the text.
type QuotedText struct {
	V string
}

func (q QuotedText) AppendText(b []byte) ([]byte, error) { return append(b, q.V...), nil }
func (q QuotedText) MarshalText() ([]byte, error)        { return []byte(q.V), nil }
func (q *QuotedText) UnmarshalText(b []byte) error       { q.V = string(b); return nil }

// UDPAddr and Duration are `=` aliases of standard types. A field in another
// package written through them is the standard type.
type (
	UDPAddr  = net.UDPAddr
	Duration = time.Duration
)

// Level is a named string with no methods.
type Level string

// Digest is a [4]byte whose text form is hex.
type Digest [4]byte

func (d Digest) MarshalText() ([]byte, error) { return hex.AppendEncode(nil, d[:]), nil }

func (d *Digest) UnmarshalText(b []byte) error {
	if hex.DecodedLen(len(b)) != len(d) {
		return errors.New("digest: want 8 hex digits")
	}
	_, err := hex.Decode(d[:], b)
	return err
}

// Host has fields of named types: text ones (net.IP, Digest), a []byte one
// with no codec (net.IPMask), a raw span and a named string. Its fields are
// declared in JSON-name order, so encoding/json writes them in the order ggen
// sorts them into.
type Host struct {
	Digest Digest
	Grid   [][2]int
	IP     net.IP
	Level  Level
	Mask   net.IPMask
	Raw    json.RawMessage
}

// Endpoint is a plain struct whose fields ggen writes as strings where
// encoding/json writes an object and a number.
type Endpoint struct {
	Addr    net.TCPAddr
	Timeout time.Duration
}

// tier and level are unexported, so other packages cannot name them.
type (
	tier  int
	level string
)

// Ranked has exported fields whose types other packages cannot name: named
// types that are unexported or internal, and a pointer and a slice of one.
// Its fields are declared in JSON-name order.
type Ranked struct {
	Code  hidden.Code
	Level level
	PTier *tier
	Tiers []tier
	Tier  tier `json:"tier,omitzero"`
}

// NewRanked builds a Ranked. The first tier, if any, also sets Tier and
// PTier.
func NewRanked(code, lv string, tiers ...int) Ranked {
	r := Ranked{Code: hidden.Code(code), Level: level(lv)}
	for _, t := range tiers {
		r.Tiers = append(r.Tiers, tier(t))
	}
	if len(tiers) > 0 {
		t := tier(tiers[0])
		r.Tier, r.PTier = t, &t
	}
	return r
}
