package thirdparty

import (
	"errors"
	"fmt"
	"strings"
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
