package encode

import (
	"bytes"
	"encoding/json"
	"testing"
)

// Named map/func types with value-receiver marshal hooks: their nil values
// box a nil interface data word — exactly like a typed-nil pointer — but
// calling the method on them is safe Go that stdlib performs.
type nilMapJSON map[string]int

func (m nilMapJSON) MarshalJSON() ([]byte, error) {
	if m == nil {
		return []byte(`"nil-map"`), nil
	}
	return []byte(`"map"`), nil
}

type nilFuncText func()

func (nilFuncText) MarshalText() ([]byte, error) { return []byte("fn"), nil }

// TestAppendAny_NilMapFuncNotNull pins that a nil named map/func with a
// value-receiver hook has its method CALLED (stdlib parity) — isNilPtr must
// not read their nil data word as a typed-nil pointer — while a typed-nil
// pointer still emits null.
func TestAppendAny_NilMapFuncNotNull(t *testing.T) {
	cases := []any{nilMapJSON(nil), nilFuncText(nil), (*nilMapJSON)(nil)}
	for _, v := range cases {
		want, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("stdlib json.Marshal(%T): %v", v, err)
		}
		got, err := AppendAny(nil, v)
		if err != nil {
			t.Fatalf("AppendAny(%T): %v", v, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("AppendAny(%T) = %s, stdlib %s", v, got, want)
		}
	}
}
