package prealloc

import "testing"

// The Go runtime gives objects smaller than 16 bytes a slower GC marking
// path. It keeps inline mark bits only for spans where size >= 16 (see
// gcUsesSpanInlineMarkBits). Check that no element width makes Cap pick a
// total allocation below 16 bytes.
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
