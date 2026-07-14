//go:build goexperiment.simd

package scan

import (
	"simd/archsimd"
	"testing"
)

// Semantic probe for ConcatShiftBytesRight — pins which operand is "low" and
// the byte order, before the utf8 validator builds on it. Want: prev1[i] =
// bytes of (prev ++ cur) at offset i+15, i.e. prev1[0] = prev[15], prev1[i>0]
// = cur[i-1].
func TestConcatShiftSemantics(t *testing.T) {
	var prevA, curA [16]uint8
	for i := range 16 {
		prevA[i] = uint8(i)          // 0..15
		curA[i] = uint8(100 + i)     // 100..115
	}
	prev := archsimd.LoadUint8x16Slice(prevA[:])
	cur := archsimd.LoadUint8x16Slice(curA[:])
	var out [16]uint8
	cur.ConcatShiftBytesRight(15, prev).StoreSlice(out[:])
	t.Logf("cur.ConcatShiftBytesRight(15, prev) = %v", out)
	prev.ConcatShiftBytesRight(15, cur).StoreSlice(out[:])
	t.Logf("prev.ConcatShiftBytesRight(15, cur) = %v", out)
	cur.ConcatShiftBytesRight(14, prev).StoreSlice(out[:])
	t.Logf("cur.ConcatShiftBytesRight(14, prev) = %v", out)
	cur.ConcatShiftBytesRight(13, prev).StoreSlice(out[:])
	t.Logf("cur.ConcatShiftBytesRight(13, prev) = %v", out)
}
