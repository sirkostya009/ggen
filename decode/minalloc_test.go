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

func (testElem) DecodeFromStream(s *scan.Stream) (testElem, error) {
	v, err := s.String(true)
	return testElem(v), err
}

// The runtime excludes objects below 16 bytes from span inline mark bits
// (gcUsesSpanInlineMarkBits: heapBitsInSpan(size) && size >= 16). Check no
// element width makes the ladder emit an allocation under that.
func TestPreallocCap_NeverBelow16Bytes(t *testing.T) {
	min, minSize := 1<<30, uintptr(0)
	for size := uintptr(1); size <= 4096; size++ {
		if b := PreallocCap(size) * int(size); b < min {
			min, minSize = b, size
		}
	}
	t.Logf("smallest allocation the ladder can emit: %d bytes (element %d)", min, minSize)
	if min < 16 {
		t.Errorf("ladder emits a %d-byte allocation (element %d) — below the runtime's 16-byte span-mark floor", min, minSize)
	}
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
