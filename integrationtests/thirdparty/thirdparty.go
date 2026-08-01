package thirdparty

import (
	"errors"
	"fmt"
	"strings"
)

// ValidateUpper enforces all-uppercase ASCII. Used cross-package via
// `pipe:"@thirdparty.ValidateUpper"` to exercise the @pkg.Func resolver.
func ValidateUpper(s string) error {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			return fmt.Errorf("must be uppercase: %q", s)
		}
	}
	return nil
}

// PrefixHash is a pure cross-package mod (T → T).
func PrefixHash(s string) string { return "#" + s }

// ParseNonEmpty is a fallible cross-package mod (T → (T, error)); empty input
// is a parse error, not a validation error.
func ParseNonEmpty(s string) (string, error) {
	if s == "" {
		return "", errors.New("empty value")
	}
	return s, nil
}

// External is a plain struct with no generated methods — forces the
// encoding/json fallback.
type External struct {
	Key   string `json:"key"`
	Value int    `json:"value"`
}

// Tagged is text-encoded as `"name#tag"` via TextMarshaler/TextUnmarshaler so
// the analyzer routes through those instead of json.Marshal/Unmarshal.
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

// QuotedText's text form is caller-controlled bytes (may carry `"` / `\`) —
// exercises the TextAppender marshal arm's escape-on-close.
type QuotedText struct {
	V string
}

func (q QuotedText) AppendText(b []byte) ([]byte, error) { return append(b, q.V...), nil }
func (q QuotedText) MarshalText() ([]byte, error)        { return []byte(q.V), nil }
func (q *QuotedText) UnmarshalText(b []byte) error       { q.V = string(b); return nil }
