package integrationtests

//go:generate ../ggen $GOFILE

import (
	"errors"
	"slices"
	"testing"

	"github.com/sirkostya009/ggen"
)

//ggen:generate
type KeysLeaf struct {
	N int `json:"n" pipe:"gte=0"`
}

//ggen:generate
type KeysHost struct {
	Leaf KeysLeaf `json:"leaf"`
	M    int      `json:"m"    pipe:"gte=0"`
}

func keysOf(t *testing.T, err error) []string {
	t.Helper()
	ve, ok := errors.AsType[ggen.ValidationError](err)
	if !ok {
		t.Fatalf("err = %v, want a ValidationError", err)
	}
	return ve.Path()
}

// A one-key error path is a shared package-level slice, and putting an outer
// key in front of it never writes to it.
func TestErrPath_oneKeyPathShared(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{`{"m":-1}`, `{"m":1,"m":2}`} {
		a1, a2 := decodeBothPaths[KeysHost](payload)
		b1, b2 := decodeBothPaths[KeysHost](payload)
		ka, kb := keysOf(t, a1), keysOf(t, b1)
		if !slices.Equal(ka, []string{"m"}) || !slices.Equal(keysOf(t, a2), ka) {
			t.Fatalf("%s: paths %v, %v", payload, ka, keysOf(t, a2))
		}
		if &ka[0] != &kb[0] || &keysOf(t, a2)[0] != &keysOf(t, b2)[0] {
			t.Errorf("%s: each error allocates its own path", payload)
		}
	}

	for range 3 {
		e1, e2 := decodeBothPaths[KeysHost](`{"leaf":{"n":-1}}`)
		for _, err := range []error{e1, e2} {
			if p := keysOf(t, err); !slices.Equal(p, []string{"leaf", "n"}) {
				t.Errorf("nested path = %v", p)
			}
		}
	}
	e1, e2 := decodeBothPaths[KeysLeaf](`{"n":-1}`)
	for _, err := range []error{e1, e2} {
		if p := keysOf(t, err); !slices.Equal(p, []string{"n"}) {
			t.Errorf("leaf path = %v after nested failures", p)
		}
	}
}
