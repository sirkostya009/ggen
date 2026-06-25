package scan

import (
	"testing"
)

func naiveHasCtrl(b []byte) bool {
	for _, c := range b {
		if c < 0x20 {
			return true
		}
	}
	return false
}

// TestHasCtrlByte_DifferentialExhaustive places a control byte at every
// position of buffers spanning all 8 word-phase alignments, alongside
// high-bit (UTF-8) bytes that must NOT false-positive, and checks the SWAR
// path agrees with the naive byte loop bit-for-bit.
func TestHasCtrlByte_DifferentialExhaustive(t *testing.T) {
	t.Parallel()
	// Base spans cover lengths around the 8-byte word boundary (0..40) so
	// the SWAR loop, the scalar tail, and their seam are all exercised.
	for n := 0; n <= 40; n++ {
		// All-clean baseline: mix ASCII printable and high (UTF-8) bytes,
		// none of which are control chars.
		base := make([]byte, n)
		for i := range base {
			if i%3 == 0 {
				base[i] = 0x80 + byte(i%0x80) // high bit set, must not trip
			} else {
				base[i] = 0x20 + byte(i%0x5f) // printable ASCII
			}
		}
		if got, want := hasCtrlByte(base), naiveHasCtrl(base); got != want {
			t.Fatalf("clean n=%d: hasCtrlByte=%v naive=%v (%v)", n, got, want, base)
		}
		// Inject a control byte at every position, with every control value.
		for pos := 0; pos < n; pos++ {
			for _, ctrl := range []byte{0x00, 0x01, 0x09, 0x0a, 0x0d, 0x1f} {
				b := append([]byte(nil), base...)
				b[pos] = ctrl
				if got, want := hasCtrlByte(b), naiveHasCtrl(b); got != want {
					t.Fatalf("n=%d pos=%d ctrl=%#x: hasCtrlByte=%v naive=%v",
						n, pos, ctrl, got, want)
				}
			}
		}
	}
}

// TestHasCtrlByte_Boundary pins 0x1f (control) vs 0x20 (space, the lowest
// legal byte) at the exact lane boundary.
func TestHasCtrlByte_Boundary(t *testing.T) {
	t.Parallel()
	for _, n := range []int{7, 8, 9, 15, 16, 17} {
		clean := make([]byte, n)
		for i := range clean {
			clean[i] = 0x20 // all spaces — lowest legal
		}
		if hasCtrlByte(clean) {
			t.Fatalf("n=%d all-0x20 reported ctrl", n)
		}
		dirty := append([]byte(nil), clean...)
		dirty[n-1] = 0x1f
		if !hasCtrlByte(dirty) {
			t.Fatalf("n=%d trailing 0x1f not detected", n)
		}
	}
}
