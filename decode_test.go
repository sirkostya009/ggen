package ggen

import (
	"testing"
)

// intT is a minimal bytes-path Decoder for the walker tests.
type intT int64

func (intT) DecodeFrom(data []byte) (intT, int, error) {
	v, n, err := Int64(data, 0)
	return intT(v), n, err
}

// TestUnmarshalSliceEmptyNonNil pins that [] decodes to a NON-nil empty slice
// — generated slice fields and jsonv2 agree. The Stream walker's half of this
// contract lives in scan's TestStreamMethods.
func TestUnmarshalSliceEmptyNonNil(t *testing.T) {
	got, err := UnmarshalSlice[intT]([]byte(" [ ] "))
	if err != nil {
		t.Fatalf("UnmarshalSlice: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("UnmarshalSlice([]) = %#v, want non-nil empty", got)
	}
	// Non-empty arrays are unaffected.
	if got, err := UnmarshalSlice[intT]([]byte("[1,2]")); err != nil || len(got) != 2 {
		t.Errorf("UnmarshalSlice([1,2]) = %v, %v", got, err)
	}
}
