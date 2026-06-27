package integrationtests

import (
	"reflect"
	"strings"
	"testing"
)

// wsify inserts assorted whitespace (space/tab/newline/CR) around every
// structural token ({ } [ ] : ,) that sits OUTSIDE a string literal, so a
// decode of the result exercises every whitespace-skip site the generated
// decoders emit (after `:`, before/after `[`/`{`, around `,`, before `]`/`}`).
func wsify(s string) string {
	const ws = " \t\n\r "
	var b strings.Builder
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			b.WriteByte(c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
			b.WriteByte(c)
		case '{', '[', ',', ':', '}', ']':
			b.WriteString(ws)
			b.WriteByte(c)
			b.WriteString(ws)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// TestWhitespace_Tolerance: decoding a whitespace-laden payload must yield the
// same value as the compact form, across every container shape (slice, map,
// nested slice, fixed array, slice-of-array, array-of-slice, nested struct).
// Pins the whitespace-skip behaviour so a codegen change to those skips can't
// silently regress (the rest of the suite uses compact JSON).
func TestWhitespace_Tolerance(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		compact string
		decode  func([]byte) (any, error)
	}{
		{
			"extra",
			`{"hintedTags":["a","b"],"clampedScore":50,"keyedMap":{"key":1,"abc":2},"nestedInts":[[1,2],[3]],"triple":[[["x"]],[["y","z"]]]}`,
			func(p []byte) (any, error) { v, _, e := ExtraStruct{}.DecodeFrom(p); return v, e },
		},
		{
			"tuple",
			`{"point":[1.5,2.5],"rgb":[10,20,30],"segments":[[1,2],[3,4]],"pair":[["a"],["b","c"]]}`,
			func(p []byte) (any, error) { v, _, e := TupleStruct{}.DecodeFrom(p); return v, e },
		},
		{
			"node",
			`{"id":1,"name":"n","score":9.5,"active":true,"tags":["a","b"],"props":{"k":"v"},"children":[{"id":2,"name":"c","score":0,"active":false,"tags":[],"props":{},"children":[]}]}`,
			func(p []byte) (any, error) { v, _, e := Node{}.DecodeFrom(p); return v, e },
		},
		// Top-level container aliases — exercise the alias decode path's leading
		// whitespace handling (opt [18] moved the skip there).
		{
			"alias_slice",
			`["a","b","c"]`,
			func(p []byte) (any, error) { v, _, e := AliasTags{}.DecodeFrom(p); return v, e },
		},
		{
			"alias_map",
			`{"x":1,"y":2}`,
			func(p []byte) (any, error) { v, _, e := AliasLookup{}.DecodeFrom(p); return v, e },
		},
		{
			"alias_array",
			`[1,2,3]`,
			func(p []byte) (any, error) { v, _, e := AliasTuple{}.DecodeFrom(p); return v, e },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want, err := c.decode([]byte(c.compact))
			if err != nil {
				t.Fatalf("compact decode: %v", err)
			}
			padded := wsify(c.compact)
			got, err := c.decode([]byte(padded))
			if err != nil {
				t.Fatalf("whitespace decode: %v\npayload: %s", err, padded)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("whitespace decode mismatch:\n got:  %#v\n want: %#v", got, want)
			}
		})
	}
}
