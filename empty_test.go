package ggen

import (
	"testing"
	"time"
)

func TestAnyIsEmpty(t *testing.T) {
	t.Parallel()
	var nilPtr *int
	cases := []struct {
		v    any
		want bool
	}{
		{nil, true}, {"", true}, {"x", false},
		{[]any{}, true}, {[]any{1}, false},
		{map[string]any{}, true}, {map[string]any{"a": 1}, false},
		{0, false}, {false, false}, {0.0, false}, {uint8(0), false},
		{[]int{}, true}, {[]int(nil), true}, {[]int{1}, false},
		{[]byte{}, true}, {[]byte("x"), false},
		{map[string]int{}, true}, {map[string]int{"a": 1}, false},
		{[0]int{}, true}, {[1]int{}, false},
		{nilPtr, true}, {new(""), true}, {new("x"), false}, {new(0), false}, {new(new("")), true},
		{time.Time{}, false},
	}
	for _, c := range cases {
		if got := AnyIsEmpty(c.v); got != c.want {
			t.Errorf("AnyIsEmpty(%#v) = %v, want %v", c.v, got, c.want)
		}
		// The predicate must agree with what AppendAny writes.
		out, err := AppendAny(nil, c.v)
		if err != nil {
			t.Fatalf("AppendAny(%#v): %v", c.v, err)
		}
		switch s := string(out); s {
		case "null", `""`, "[]", "{}":
			if !c.want {
				t.Errorf("AppendAny(%#v) = %s, JSON-empty, but AnyIsEmpty says false", c.v, s)
			}
		default:
			if c.want {
				t.Errorf("AppendAny(%#v) = %s, not JSON-empty, but AnyIsEmpty says true", c.v, s)
			}
		}
	}
}
