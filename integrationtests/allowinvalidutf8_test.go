package integrationtests

//go:generate ../ggen $GOFILE

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sirkostya009/ggen"
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
// shape (inline window, long-span ggen.String fall, escape arm, slice elem,
// map key+value, raw capture), bytes + stream, against the strict default
// (Address) as control.
func TestAllowInvalidUTF8(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 40)
	cases := []struct {
		name    string
		payload string
		check   func(t *testing.T, v PermissiveDoc)
	}{
		{"short_raw_ff", "{\"name\":\"a\xffb\"}", func(t *testing.T, v PermissiveDoc) {
			t.Helper()
			if v.Name != "a\xffb" {
				t.Errorf("Name = %q, want raw bytes through", v.Name)
			}
		}},
		{"long_raw_ff", "{\"long\":\"" + long + "\xff\"}", func(t *testing.T, v PermissiveDoc) {
			t.Helper()
			if v.Long != long+"\xff" {
				t.Errorf("Long = %q, want raw bytes through", v.Long)
			}
		}},
		{"escape_with_invalid", "{\"name\":\"a\\n\xffz\"}", func(t *testing.T, v PermissiveDoc) {
			t.Helper()
			if v.Name != "a\n\xffz" {
				t.Errorf("Name = %q, want unescaped + raw byte", v.Name)
			}
		}},
		{"lone_surrogate_fffd", `{"name":"\uD83D"}`, func(t *testing.T, v PermissiveDoc) {
			t.Helper()
			if v.Name != "�" {
				t.Errorf("Name = %q, want U+FFFD substitution", v.Name)
			}
		}},
		{"slice_elem", "{\"tags\":[\"ok\",\"a\xffb\"]}", func(t *testing.T, v PermissiveDoc) {
			t.Helper()
			if len(v.Tags) != 2 || v.Tags[1] != "a\xffb" {
				t.Errorf("Tags = %q", v.Tags)
			}
		}},
		{"map_key_value", "{\"props\":{\"k\xff\":\"v\xfe\"}}", func(t *testing.T, v PermissiveDoc) {
			t.Helper()
			if v.Props["k\xff"] != "v\xfe" {
				t.Errorf("Props = %q", v.Props)
			}
		}},
		{"raw_span", "{\"raw\":{\"k\":\"a\xffb\"}}", func(t *testing.T, v PermissiveDoc) {
			t.Helper()
			if string(v.Raw) != "{\"k\":\"a\xffb\"}" {
				t.Errorf("Raw = %q", v.Raw)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, _, err := PermissiveDoc{}.DecodeFrom([]byte(c.payload))
			if err != nil {
				t.Fatalf("bytes: %v", err)
			}
			c.check(t, got)
			for _, bufCap := range []int{8, 512} {
				var s ggen.Stream
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

// R10PermissiveAny: the opt-out reaches `any` values too — every string and
// key inside an any field, map[string]any values and the json:",embed"
// catch-all — on both the float64 and usenumber shapes. AnyStruct (strict)
// is the control.
//
//ggen:generate allowinvalidutf8
type R10PermissiveAny struct {
	Body  any            `json:"body"`
	M     map[string]any `json:"m"`
	Extra map[string]any `json:",embed"`
}

//ggen:generate allowinvalidutf8 usenumber
type R10PermissiveAnyNumber struct {
	Body any `json:"body"`
}

func TestAllowInvalidUTF8_anyValues(t *testing.T) {
	t.Parallel()
	const bad = "\xff"
	cases := []struct {
		name    string
		payload string
		check   func(v R10PermissiveAny) bool
	}{
		{"any_string", `{"body":"` + bad + `"}`, func(v R10PermissiveAny) bool { return v.Body == bad }},
		{"any_object_key", `{"body":{"k` + bad + `":1}}`, func(v R10PermissiveAny) bool {
			m, _ := v.Body.(map[string]any)
			return m["k"+bad] == 1.0
		}},
		{"any_nested", `{"body":[{"k":"` + bad + `"}]}`, func(v R10PermissiveAny) bool {
			a, _ := v.Body.([]any)
			m, _ := a[0].(map[string]any)
			return m["k"] == bad
		}},
		{"map_any_value", `{"m":{"k":"` + bad + `"}}`, func(v R10PermissiveAny) bool { return v.M["k"] == bad }},
		{"embed_any_value", `{"zz":"` + bad + `"}`, func(v R10PermissiveAny) bool { return v.Extra["zz"] == bad }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, _, err := R10PermissiveAny{}.DecodeFrom([]byte(c.payload))
			if err != nil || !c.check(got) {
				t.Errorf("bytes: %+v (%v)", got, err)
			}
			var s ggen.Stream
			s.Reset(&chunkReader{data: []byte(c.payload), max: 3}, make([]byte, 0, 16))
			sgot, err := R10PermissiveAny{}.DecodeFromStream(&s)
			if err != nil || !c.check(sgot) {
				t.Errorf("stream: %+v (%v)", sgot, err)
			}
		})
	}
	num, _, err := R10PermissiveAnyNumber{}.DecodeFrom([]byte(`{"body":"` + bad + `"}`))
	if err != nil || num.Body != bad {
		t.Errorf("usenumber: %+v (%v)", num, err)
	}
	// Strict control: the same bytes into a validating any field still reject.
	if _, _, err := (AnyStruct{}).DecodeFrom([]byte(`{"body":"` + bad + `"}`)); !errors.Is(err, ggen.ErrInvalidUTF8) {
		t.Errorf("strict any accepted invalid UTF-8: %v", err)
	}
}
