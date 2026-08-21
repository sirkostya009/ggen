//go:build goexperiment.simd

package ggen

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
		prevA[i] = uint8(i)      // 0..15
		curA[i] = uint8(100 + i) // 100..115
	}
	prev := archsimd.LoadUint8x16(prevA[:])
	cur := archsimd.LoadUint8x16(curA[:])

	// The law validUTF8x16's prev1/prev2/prev3 lanes build on:
	// recv.ConcatShiftBytesRight(N, arg)[i] == (arg ++ recv)[i+N].
	check := func(name string, got [16]uint8, arg, recv [16]uint8, n int) {
		t.Helper()
		concat := append(append([]uint8{}, arg[:]...), recv[:]...)
		for i := range 16 {
			if got[i] != concat[i+n] {
				t.Fatalf("%s: out[%d] = %d, want (arg++recv)[%d] = %d\nout = %v",
					name, i, got[i], i+n, concat[i+n], got)
			}
		}
	}

	var out [16]uint8
	cur.ConcatShiftBytesRight(prev, 15).Store(out[:])
	check("cur.ConcatShiftBytesRight(prev, 15)", out, prevA, curA, 15)
	prev.ConcatShiftBytesRight(cur, 15).Store(out[:])
	check("prev.ConcatShiftBytesRight(cur, 15)", out, curA, prevA, 15)
	cur.ConcatShiftBytesRight(prev, 14).Store(out[:])
	check("cur.ConcatShiftBytesRight(prev, 14)", out, prevA, curA, 14)
	cur.ConcatShiftBytesRight(prev, 13).Store(out[:])
	check("cur.ConcatShiftBytesRight(prev, 13)", out, prevA, curA, 13)
}
