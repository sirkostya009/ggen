package prealloc

import "testing"

// The runtime excludes objects below 16 bytes from span inline mark bits
// (gcUsesSpanInlineMarkBits: heapBitsInSpan(size) && size >= 16). Check no
// element width makes the ladder emit an allocation under that.
func TestPreallocCap_NeverBelow16Bytes(t *testing.T) {
	t.Parallel()
	smallest, minSize := 1<<30, uintptr(0)
	for size := uintptr(1); size <= 4096; size++ {
		if b := Cap(size) * int(size); b < smallest {
			smallest, minSize = b, size
		}
	}
	t.Logf("smallest allocation the ladder can emit: %d bytes (element %d)", smallest, minSize)
	if smallest < 16 {
		t.Errorf("ladder emits a %d-byte allocation (element %d) — below the runtime's 16-byte span-mark floor", smallest, minSize)
	}
}
