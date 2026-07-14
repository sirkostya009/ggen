//go:build goexperiment.jsonv2

package integrationtests

//go:generate ../ggen $GOFILE

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/scan"
)

// PermissiveDoc opts out of decode UTF-8 validation: raw invalid bytes flow
// into string fields / keys / raw spans untouched, unpaired \uXXXX surrogates
// substitute U+FFFD (encoding/json v1 shape, minus v1's raw-byte U+FFFD
// substitution — bytes pass through verbatim here).
//
//ggen:generate allowinvalidutf8
type PermissiveDoc struct {
	Name  string            `json:"name"`
	Long  string            `json:"long"`
	Tags  []string          `json:"tags"`
	Props map[string]string `json:"props"`
	Raw   json.RawMessage   `json:"raw"`
}

// TestAllowInvalidUTF8 pins the permissive contract on every string-producing
// shape (inline window, long-span scan.String fall, escape arm, slice elem,
// map key+value, raw capture), bytes + stream, against the strict default
// (Address) as control.
func TestAllowInvalidUTF8(t *testing.T) {
	long := strings.Repeat("x", 40)
	cases := []struct {
		name    string
		payload string
		check   func(t *testing.T, v PermissiveDoc)
	}{
		{"short_raw_ff", "{\"name\":\"a\xffb\"}", func(t *testing.T, v PermissiveDoc) {
			if v.Name != "a\xffb" {
				t.Errorf("Name = %q, want raw bytes through", v.Name)
			}
		}},
		{"long_raw_ff", "{\"long\":\"" + long + "\xff\"}", func(t *testing.T, v PermissiveDoc) {
			if v.Long != long+"\xff" {
				t.Errorf("Long = %q, want raw bytes through", v.Long)
			}
		}},
		{"escape_with_invalid", "{\"name\":\"a\\n\xffz\"}", func(t *testing.T, v PermissiveDoc) {
			if v.Name != "a\n\xffz" {
				t.Errorf("Name = %q, want unescaped + raw byte", v.Name)
			}
		}},
		{"lone_surrogate_fffd", `{"name":"\uD83D"}`, func(t *testing.T, v PermissiveDoc) {
			if v.Name != "�" {
				t.Errorf("Name = %q, want U+FFFD substitution", v.Name)
			}
		}},
		{"slice_elem", "{\"tags\":[\"ok\",\"a\xffb\"]}", func(t *testing.T, v PermissiveDoc) {
			if len(v.Tags) != 2 || v.Tags[1] != "a\xffb" {
				t.Errorf("Tags = %q", v.Tags)
			}
		}},
		{"map_key_value", "{\"props\":{\"k\xff\":\"v\xfe\"}}", func(t *testing.T, v PermissiveDoc) {
			if v.Props["k\xff"] != "v\xfe" {
				t.Errorf("Props = %q", v.Props)
			}
		}},
		{"raw_span", "{\"raw\":{\"k\":\"a\xffb\"}}", func(t *testing.T, v PermissiveDoc) {
			if string(v.Raw) != "{\"k\":\"a\xffb\"}" {
				t.Errorf("Raw = %q", v.Raw)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := PermissiveDoc{}.DecodeFrom([]byte(c.payload))
			if err != nil {
				t.Fatalf("bytes: %v", err)
			}
			c.check(t, got)
			for _, bufCap := range []int{8, 512} {
				var s scan.Stream
				s.Reset(&chunkReader{data: []byte(c.payload), max: 3}, make([]byte, 0, bufCap))
				sGot, sErr := PermissiveDoc{}.DecodeFromStream(&s)
				if sErr != nil {
					t.Fatalf("stream cap=%d: %v", bufCap, sErr)
				}
				c.check(t, sGot)
			}
		})
	}
	// Grammar errors still reject in permissive mode — only UTF-8 checks are off.
	for _, bad := range []string{"{\"name\":\"a\x01b\"}", `{"name":"a\q"}`, `{"name":"unterminated`} {
		if _, _, err := (PermissiveDoc{}).DecodeFrom([]byte(bad)); err == nil {
			t.Errorf("grammar error accepted: %q", bad)
		}
	}
	// Control: the strict default still rejects the same bytes.
	if _, _, err := (Address{}).DecodeFrom([]byte("{\"street\":\"a\xffb\",\"city\":\"Y\",\"zipCode\":\"1\"}")); err == nil {
		t.Error("strict struct accepted invalid UTF-8")
	}
}
