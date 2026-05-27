package thirdparty

import (
	"errors"
	"fmt"
	"strings"
)

// ValidateUpper enforces all-uppercase ASCII. Reachable from the main
// integrationtests package via `ggen:"@thirdparty.ValidateUpper"`, so
// the @pkg.Func resolver walks the source file's imports, picks up the
// non-aliased thirdparty package, and emits a direct call.
func ValidateUpper(s string) error {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			return fmt.Errorf("must be uppercase: %q", s)
		}
	}
	return nil
}

// PrefixHash is a pure mod (T → T) — same cross-package resolution path
// as ValidateUpper, but used via `mod:"@thirdparty.PrefixHash"`.
func PrefixHash(s string) string { return "#" + s }

// ParseNonEmpty is a fallible mod (T → (T, error)) — exercises the
// cross-package fallible-mod path. Empty input is rejected as a parse
// error (not a validation error), matching same-package fallible-mod
// semantics in mods_test.go.
func ParseNonEmpty(s string) (string, error) {
	if s == "" {
		return "", errors.New("empty value")
	}
	return s, nil
}

// External is a plain Go struct without any generated AppendJSON/DecodeFrom
// methods. ggen-generated code should fall back to json for encoding and
// decoding it.
type External struct {
	Key   string `json:"key"`
	Value int    `json:"value"`
}

// Tagged is text-encoded as `"name#tag"`. Implements TextMarshaler /
// TextUnmarshaler so the static analyzer routes encode/decode through
// these methods instead of the slower json.Marshal / json.Unmarshal path.
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
