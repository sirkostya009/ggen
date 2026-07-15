package scan

import (
	"errors"
	"strings"
	"testing"
)

// TestMaxDepth pins the nesting cap on every recursive runtime path: SkipValue
// and the Any families, bytes + stream. Without the cap a few MB of "[[[["
// is a fatal (unrecoverable) goroutine stack overflow — jsonv2 errors at the
// same class of limit.
func TestMaxDepth(t *testing.T) {
	t.Parallel()
	deep := func(n int) []byte {
		return []byte(strings.Repeat("[", n) + strings.Repeat("]", n))
	}
	deepObj := func(n int) []byte {
		return []byte(strings.Repeat(`{"k":`, n) + "1" + strings.Repeat("}", n))
	}
	over := deep(MaxDepth + 1)
	overObj := deepObj(MaxDepth + 1)
	under := deep(MaxDepth)

	if _, err := SkipValue(over, 0); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("SkipValue deep array: want ErrMaxDepth, got %v", err)
	}
	if _, err := SkipValue(overObj, 0); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("SkipValue deep object: want ErrMaxDepth, got %v", err)
	}
	if j, err := SkipValue(under, 0); err != nil || j != len(under) {
		t.Errorf("SkipValue at cap: err=%v j=%d", err, j)
	}
	if _, _, err := Any(over, 0); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("Any deep array: want ErrMaxDepth, got %v", err)
	}
	if _, _, err := AnyNumber(overObj, 0); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("AnyNumber deep object: want ErrMaxDepth, got %v", err)
	}
	if _, _, err := AnyCopy(over, 0); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("AnyCopy deep array: want ErrMaxDepth, got %v", err)
	}
	if _, _, err := AnyNumberCopy(over, 0); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("AnyNumberCopy deep array: want ErrMaxDepth, got %v", err)
	}
	if v, _, err := Any(under, 0); err != nil || v == nil {
		t.Errorf("Any at cap: err=%v", err)
	}

	var s Stream
	s.Reset(strings.NewReader(string(over)), make([]byte, 0, 1024))
	if err := s.SkipValue(); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("stream SkipValue: want ErrMaxDepth, got %v", err)
	}
	s.Reset(strings.NewReader(string(over)), make([]byte, 0, 1024))
	if _, err := s.Any(); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("stream Any: want ErrMaxDepth, got %v", err)
	}
	s.Reset(strings.NewReader(string(overObj)), make([]byte, 0, 1024))
	if _, err := s.AnyNumber(); !errors.Is(err, ErrMaxDepth) {
		t.Errorf("stream AnyNumber: want ErrMaxDepth, got %v", err)
	}
	s.Reset(strings.NewReader(string(under)), make([]byte, 0, 1024))
	if err := s.SkipValue(); err != nil {
		t.Errorf("stream SkipValue at cap: %v", err)
	}
}
