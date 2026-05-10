package thirdparty

import (
	"errors"
	"strings"
)

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
