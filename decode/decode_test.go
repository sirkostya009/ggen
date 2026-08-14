package decode

import (
	"strings"
	"testing"

	"github.com/sirkostya009/ggen/scan"
)

// intT is a minimal Decoder for walker tests.
type intT int64

func (intT) DecodeFrom(data []byte) (intT, int, error) {
	v, n, err := scan.Int64(data, 0)
	return intT(v), n, err
}

func (intT) DecodeFromStream(s *scan.Stream) (intT, error) {
	v, err := s.Int64()
	return intT(v), err
}

// TestUnmarshalSliceEmptyNonNil pins that [] decodes to a NON-nil empty
// slice on both walkers — generated slice fields and jsonv2 agree.
func TestUnmarshalSliceEmptyNonNil(t *testing.T) {
	got, err := UnmarshalSlice[intT]([]byte(" [ ] "))
	if err != nil {
		t.Fatalf("UnmarshalSlice: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("UnmarshalSlice([]) = %#v, want non-nil empty", got)
	}
	sgot, _, err := UnmarshalSliceStream[intT](strings.NewReader(" [ ] "), nil)
	if err != nil {
		t.Fatalf("UnmarshalSliceStream: %v", err)
	}
	if sgot == nil || len(sgot) != 0 {
		t.Errorf("UnmarshalSliceStream([]) = %#v, want non-nil empty", sgot)
	}
	// Non-empty arrays are unaffected.
	if got, err := UnmarshalSlice[intT]([]byte("[1,2]")); err != nil || len(got) != 2 {
		t.Errorf("UnmarshalSlice([1,2]) = %v, %v", got, err)
	}
}
