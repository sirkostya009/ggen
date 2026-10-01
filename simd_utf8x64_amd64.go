//go:build goexperiment.simd

// 64-byte version of validUTF8x16, used only by the avx512 tier. AVX-512 is
// the x86 vector instruction set that processes 64 bytes with one
// instruction. A lane is one byte slot inside a vector. The algorithm and the
// set of accepted inputs are the same as validUTF8x16, so see that file for
// how UTF-8 and the lookup tables work. Only two things differ: the width,
// and how the vectors of the previous 1, 2 and 3 bytes (prevN) are built.
//
// simdutf builds prevN by shuffling bytes across registers, because its input
// is already in a register. ggen always validates a contiguous []byte, so
// prevN is just a load from b[i-N:]. Three loads from cache replace a shuffle
// plus three byte shifts and give a shorter chain of dependent instructions.
// It also avoids a gap in archsimd, which has no byte shift across a whole
// vector wider than 128 bits.
//
// The 512-bit table lookup (VPSHUFB) works on each 128-bit quarter
// separately. That suits a 16-entry nibble table, so the tables are just
// repeated 4 times.
//
// The first block has no bytes before it. So the first 16 bytes run through
// the 16-byte lookup with zero as the previous bytes. There is no end check
// there, because a rune may continue into the next block. The wide loop then
// starts at 16, where every prevN load is in bounds. The final block starts at
// len(b)-64 instead of loading zero-padded past the end. archsimd's
// LoadUint8x64Part is a full 64-byte load that reads past the slice and
// faults at a page edge. Checking the overlapping lanes twice is harmless.
// Each lane depends only on its own four bytes and is ORed into errAcc.
// Because the final block ends exactly at len(b), a rune cut short there is
// caught by the end check (check_eof) after the loop.

package ggen

import "simd/archsimd"

// rep4 repeats a 16-entry nibble table across the four 128-bit quarters of a
// 512-bit register. The lookup indexes only within its own quarter.
func rep4(t [16]uint8) (o [64]uint8) {
	for i := range o {
		o[i] = t[i%16]
	}
	return
}

var (
	utf8Byte1High64 = rep4(utf8Byte1High)
	utf8Byte1Low64  = rep4(utf8Byte1Low)
	utf8Byte2High64 = rep4(utf8Byte2High)
)

// utf8MaxIncomplete64 is utf8MaxIncomplete at 64 lanes. Only the last three
// positions have a limit, so a saturating subtract is non-zero exactly when
// the last block ends in the middle of a rune.
var utf8MaxIncomplete64 = func() (o [64]uint8) {
	for i := range o {
		o[i] = 0xFF
	}
	o[61], o[62], o[63] = 0xF0-1, 0xE0-1, 0xC0-1
	return
}()

// utf8x64MinLen is the shortest input validUTF8x64 handles itself. Below it
// the extra setup costs more than the fewer loop steps save. That setup is
// three 64-byte table loads on top of the 16-byte head's. Shorter input stays
// on validUTF8x16.
const utf8x64MinLen = 128

// validUTF8x64 reports whether b is valid UTF-8. It accepts exactly what
// unicode/utf8.Valid and validUTF8x16 accept, which TestValidUTF8x64_Parity
// checks.
func validUTF8x64(b []byte) bool {
	if len(b) < utf8x64MinLen {
		return validUTF8x16(b)
	}

	// Head: the first 16 bytes, zero as the previous bytes, no end check.
	lo1 := archsimd.LoadUint8x16(utf8Byte1High[:])
	lo2 := archsimd.LoadUint8x16(utf8Byte1Low[:])
	lo3 := archsimd.LoadUint8x16(utf8Byte2High[:])
	nib16 := archsimd.BroadcastUint8x16(0x0F)
	zero16 := archsimd.BroadcastUint8x16(0)
	h := archsimd.LoadUint8x16(b)
	hPrev1 := h.ConcatShiftBytesRight(zero16, 15)
	hPrev1Hi := hPrev1.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(nib16)
	hCurHi := h.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(nib16)
	hsc := lo1.PermuteOrZero(hPrev1Hi.BitsToInt8()).
		And(lo2.PermuteOrZero(hPrev1.And(nib16).BitsToInt8())).
		And(lo3.PermuteOrZero(hCurHi.BitsToInt8()))
	hMust := h.ConcatShiftBytesRight(zero16, 14).SubSaturated(archsimd.BroadcastUint8x16(0xE0 - 0x80)).
		Or(h.ConcatShiftBytesRight(zero16, 13).SubSaturated(archsimd.BroadcastUint8x16(0xF0 - 0x80))).
		And(archsimd.BroadcastUint8x16(0x80))
	if !hMust.Xor(hsc).IsZero() {
		return false
	}

	lut1 := archsimd.LoadUint8x64(utf8Byte1High64[:])
	lut2 := archsimd.LoadUint8x64(utf8Byte1Low64[:])
	lut3 := archsimd.LoadUint8x64(utf8Byte2High64[:])
	nib := archsimd.BroadcastUint8x64(0x0F)
	sub3 := archsimd.BroadcastUint8x64(0xE0 - 0x80)
	sub4 := archsimd.BroadcastUint8x64(0xF0 - 0x80)
	high := archsimd.BroadcastUint8x64(0x80)
	zero := archsimd.BroadcastUint8x64(0)
	errAcc := zero
	prev := zero

	// Full blocks only. Every load is a full 64-byte load that stays in
	// bounds, since b[i-3:] holds at least 64 bytes. The last block is moved
	// back so it ends at len(b).
	for i := 16; ; i += 64 {
		if i+64 > len(b) {
			if i >= len(b) {
				break
			}
			i = len(b) - 64
		}
		c := archsimd.LoadUint8x64(b[i:])
		p1 := archsimd.LoadUint8x64(b[i-1:])
		p2 := archsimd.LoadUint8x64(b[i-2:])
		p3 := archsimd.LoadUint8x64(b[i-3:])
		prev1Hi := p1.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(nib)
		curHi := c.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(nib)
		sc := lut1.PermuteOrZeroGrouped(prev1Hi.BitsToInt8()).
			And(lut2.PermuteOrZeroGrouped(p1.And(nib).BitsToInt8())).
			And(lut3.PermuteOrZeroGrouped(curHi.BitsToInt8()))
		must := p2.SubSaturated(sub3).Or(p3.SubSaturated(sub4)).And(high)
		errAcc = errAcc.Or(must.Xor(sc))
		prev = c
	}
	// End check on the final block. See validUTF8x16.
	maxInc := archsimd.LoadUint8x64(utf8MaxIncomplete64[:])
	errAcc = errAcc.Or(prev.SubSaturated(maxInc))
	return errAcc.Equal(zero).ToBits() == ^uint64(0)
}
