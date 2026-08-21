//go:build goexperiment.simd

// 64-byte-lane sibling of validUTF8x16, for the avx512 tier only. Same
// Lemire/simdjson lookup4 algorithm and the same accept set — the only
// differences are width and how the prev1/2/3 vectors are obtained.
//
// simdutf builds those with cross-lane shuffles (VPERMI2D/VPERM2I128) because
// its input is a register it already holds. ggen always validates a CONTIGUOUS
// []byte span, so prevN is just an unaligned load from b[i-N:] — three L1-hot
// loads replace a permute plus three VPALIGNRs and shorten the dependency
// chain. That also sidesteps archsimd having no non-grouped byte concat-shift
// above 128 bits.
//
// VPSHUFB at 512 bits operates per 128-bit lane, which is exactly what a
// nibble LUT wants, so the 16-entry tables are simply repeated 4×.
//
// The first block has no preceding bytes, so it runs one 16-lane classify with
// prev = zero (no EOF check — the rune may continue into the wide region); the
// wide loop then starts at 16, where every prevN load is in bounds.

package ggen

import "simd/archsimd"

// rep4 repeats a 16-entry nibble LUT across the four 128-bit lanes of a
// 512-bit register (VPSHUFB indexes within its own lane).
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

// utf8MaxIncomplete64 is utf8MaxIncomplete at 64 lanes: only the final three
// positions are constrained, so a saturating sub is non-zero exactly when the
// last block ends mid-rune.
var utf8MaxIncomplete64 = func() (o [64]uint8) {
	for i := range o {
		o[i] = 0xFF
	}
	o[61], o[62], o[63] = 0xF0-1, 0xE0-1, 0xC0-1
	return
}()

// utf8x64MinLen gates validUTF8x64: below it the wider setup (three 64-byte
// LUT loads on top of the 16-lane head's) costs more than the block count it
// saves, so short spans stay on validUTF8x16.
const utf8x64MinLen = 128

// validUTF8x64 reports whether b is well-formed UTF-8 — same accept set as
// unicode/utf8.Valid and validUTF8x16 (pinned by TestValidUTF8x64_Parity).
func validUTF8x64(b []byte) bool {
	if len(b) < utf8x64MinLen {
		return validUTF8x16(b)
	}

	// Head: first 16 bytes with prev = zero, 16-lane classify, no EOF check.
	lo1 := archsimd.LoadUint8x16(utf8Byte1High[:])
	lo2 := archsimd.LoadUint8x16(utf8Byte1Low[:])
	lo3 := archsimd.LoadUint8x16(utf8Byte2High[:])
	nib16 := archsimd.BroadcastUint8x16(0x0F)
	zero16 := archsimd.BroadcastUint8x16(0)
	h := archsimd.LoadUint8x16(b)
	hPrev1 := h.ConcatShiftBytesRight(zero16, 15)
	hPrev1Hi := hPrev1.AsUint16x8().ShiftAllRight(4).AsUint8x16().And(nib16)
	hCurHi := h.AsUint16x8().ShiftAllRight(4).AsUint8x16().And(nib16)
	hsc := lo1.PermuteOrZero(hPrev1Hi.AsInt8x16()).
		And(lo2.PermuteOrZero(hPrev1.And(nib16).AsInt8x16())).
		And(lo3.PermuteOrZero(hCurHi.AsInt8x16()))
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

	i := 16
	// Full blocks: b[i-3:] always holds ≥ 64 bytes here, so every prevN load
	// is a plain full load.
	for ; i+64 <= len(b); i += 64 {
		c := archsimd.LoadUint8x64(b[i:])
		p1 := archsimd.LoadUint8x64(b[i-1:])
		p2 := archsimd.LoadUint8x64(b[i-2:])
		p3 := archsimd.LoadUint8x64(b[i-3:])
		prev1Hi := p1.AsUint16x32().ShiftAllRight(4).AsUint8x64().And(nib)
		curHi := c.AsUint16x32().ShiftAllRight(4).AsUint8x64().And(nib)
		sc := lut1.PermuteOrZeroGrouped(prev1Hi.AsInt8x64()).
			And(lut2.PermuteOrZeroGrouped(p1.And(nib).AsInt8x64())).
			And(lut3.PermuteOrZeroGrouped(curHi.AsInt8x64()))
		must := p2.SubSaturated(sub3).Or(p3.SubSaturated(sub4)).And(high)
		errAcc = errAcc.Or(must.Xor(sc))
		prev = c
	}
	// Tail: zero-padded, so a rune truncated inside it fails its successor
	// check in-block. prevN load the same way (their pad lanes only ever pair
	// with padded cur lanes).
	if i < len(b) {
		c, _ := archsimd.LoadUint8x64Part(b[i:])
		p1, _ := archsimd.LoadUint8x64Part(b[i-1:])
		p2, _ := archsimd.LoadUint8x64Part(b[i-2:])
		p3, _ := archsimd.LoadUint8x64Part(b[i-3:])
		prev1Hi := p1.AsUint16x32().ShiftAllRight(4).AsUint8x64().And(nib)
		curHi := c.AsUint16x32().ShiftAllRight(4).AsUint8x64().And(nib)
		sc := lut1.PermuteOrZeroGrouped(prev1Hi.AsInt8x64()).
			And(lut2.PermuteOrZeroGrouped(p1.And(nib).AsInt8x64())).
			And(lut3.PermuteOrZeroGrouped(curHi.AsInt8x64()))
		must := p2.SubSaturated(sub3).Or(p3.SubSaturated(sub4)).And(high)
		errAcc = errAcc.Or(must.Xor(sc))
		prev = c
	}
	// check_eof on the final block (see validUTF8x16).
	maxInc := archsimd.LoadUint8x64(utf8MaxIncomplete64[:])
	errAcc = errAcc.Or(prev.SubSaturated(maxInc))
	return errAcc.Equal(zero).ToBits() == ^uint64(0)
}
