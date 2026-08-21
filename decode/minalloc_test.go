package decode

import (
	"errors"
	"testing"

	"github.com/sirkostya009/ggen/scan"
)

// testElem is a minimal Decoder[T] for walker tests.
type testElem string

func (testElem) DecodeFrom(data []byte) (testElem, int, error) {
	v, n, err := scan.String(data, 0, true)
	return testElem(v), n, err
}

// UnmarshalSlice used to accept `[1,2]]]` / `[{}]{"junk":` cleanly with no
// way for the caller to detect the remainder (jsonv2 whole-input parity),
// and started from a nil slice ignoring the package's own PreallocCap.
func TestUnmarshalSlice_TrailingGarbage(t *testing.T) {
	for _, in := range []string{`[]x`, `[] ]`, `["a"]]]`, `["a"] {"junk":1}`} {
		if _, err := UnmarshalSlice[testElem]([]byte(in)); !errors.Is(err, scan.ErrTrailingData) {
			t.Errorf("%q: got %v, want ErrTrailingData", in, err)
		}
	}
	for _, in := range []string{`[]`, ` [ ] `, `["a","b"]`, `["a"] `} {
		if _, err := UnmarshalSlice[testElem]([]byte(in)); err != nil {
			t.Errorf("%q: unexpected error %v", in, err)
		}
	}
}
