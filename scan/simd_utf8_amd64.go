//go:build goexperiment.simd

// Vectorized UTF-8 validation (Lemire/simdjson range-lookup algorithm) for
// the SIMD string tiers' second pass. classifyStructural / the stream cores
// gate it on the lane-OR high-bit flag, so it only ever runs on spans that
// actually contain non-ASCII — where the scalar utf8.Valid DFA (~1 B/ns on
// 2-byte runes) was 97-98% of the decode cost (see utf8cost benches).
//
// 16-byte blocks via VPSHUFB/VPALIGNR (AVX1-safe — classifyStructural is
// shared by all three tiers, so the validator must run on the lowest one).
//
// Per block, each byte pair (prev1[i], cur[i]) is classified through three
// nibble LUTs: sc = lut1[prev1>>4] & lut2[prev1&0xF] & lut3[cur>>4]. Every
// illegal pair (lead+non-continuation, stray continuation, overlong,
// surrogate, > U+10FFFF) sets a distinct error bit. The one legal case that
// also sets a bit is continuation-after-continuation (twoConts = 0x80, needed
// for 3/4-byte runes), so it is cancelled by XOR against must23_80 — a mask
// with 0x80 exactly where a 3rd/4th continuation byte is REQUIRED (prev2 ≥
// 0xE0 or prev3 ≥ 0xF0, detected via saturating sub). Any residue after the
// XOR accumulates into errAcc.
//
// Tail: the last partial block loads zero-padded (zeros classify as ASCII,
// turning a truncated rune into a lead+non-continuation error), and one final
// all-zero block runs unconditionally so a lead in the last byte of the last
// real block still meets its "next byte" check.

package scan

import "simd/archsimd"

const (
	utf8TooShort     = 1 << 0 // lead followed by non-continuation
	utf8TooLong      = 1 << 1 // ASCII/continuation followed by continuation
	utf8Overlong3    = 1 << 2 // E0 followed by 80..9F
	utf8TooLarge     = 1 << 3 // F4 followed by 90..BF (> U+10FFFF)
	utf8Surrogate    = 1 << 4 // ED followed by A0..BF (U+D800..DFFF)
	utf8Overlong2    = 1 << 5 // C0/C1 lead (< U+0080)
	utf8TooLarge1000 = 1 << 6 // F5..FF lead / F4 with 2nd byte ≥ 90
	utf8Overlong4    = 1 << 6 // F0 followed by 80..8F (shares bit — disjoint cases)
	utf8TwoConts     = 1 << 7 // continuation after continuation (legal iff required)

	utf8Carry = utf8TooShort | utf8TooLong | utf8TwoConts
)

// Indexed by prev1's HIGH nibble.
var utf8Byte1High = [16]uint8{
	utf8TooLong, utf8TooLong, utf8TooLong, utf8TooLong, // 0..3: ASCII
	utf8TooLong, utf8TooLong, utf8TooLong, utf8TooLong, // 4..7: ASCII
	utf8TwoConts, utf8TwoConts, utf8TwoConts, utf8TwoConts, // 8..B: continuation
	utf8TooShort | utf8Overlong2, // C: 2-byte lead (C0/C1 overlong via low nibble)
	utf8TooShort,                 // D: 2-byte lead
	utf8TooShort | utf8Overlong3 | utf8Surrogate,                    // E: 3-byte lead
	utf8TooShort | utf8TooLarge | utf8TooLarge1000 | utf8Overlong4, // F: 4-byte lead
}

// Indexed by prev1's LOW nibble.
var utf8Byte1Low = [16]uint8{
	utf8Carry | utf8Overlong3 | utf8Overlong2 | utf8Overlong4, // 0: E0/C0/F0
	utf8Carry | utf8Overlong2,                                 // 1: C1
	utf8Carry, utf8Carry,                                      // 2,3
	utf8Carry | utf8TooLarge, // 4: F4
	utf8Carry | utf8TooLarge | utf8TooLarge1000, // 5: F5
	utf8Carry | utf8TooLarge | utf8TooLarge1000, // 6
	utf8Carry | utf8TooLarge | utf8TooLarge1000, // 7
	utf8Carry | utf8TooLarge | utf8TooLarge1000, // 8
	utf8Carry | utf8TooLarge | utf8TooLarge1000, // 9
	utf8Carry | utf8TooLarge | utf8TooLarge1000, // A
	utf8Carry | utf8TooLarge | utf8TooLarge1000, // B
	utf8Carry | utf8TooLarge | utf8TooLarge1000,                  // C
	utf8Carry | utf8TooLarge | utf8TooLarge1000 | utf8Surrogate, // D: ED lead
	utf8Carry | utf8TooLarge | utf8TooLarge1000,                  // E
	utf8Carry | utf8TooLarge | utf8TooLarge1000, // F
}

// Indexed by cur's HIGH nibble.
var utf8Byte2High = [16]uint8{
	utf8TooShort, utf8TooShort, utf8TooShort, utf8TooShort, // 0..3: ASCII
	utf8TooShort, utf8TooShort, utf8TooShort, utf8TooShort, // 4..7: ASCII
	utf8TooLong | utf8Overlong2 | utf8TwoConts | utf8Overlong3 | utf8TooLarge1000 | utf8Overlong4, // 8
	utf8TooLong | utf8Overlong2 | utf8TwoConts | utf8Overlong3 | utf8TooLarge,                     // 9
	utf8TooLong | utf8Overlong2 | utf8TwoConts | utf8Surrogate | utf8TooLarge,                     // A
	utf8TooLong | utf8Overlong2 | utf8TwoConts | utf8Surrogate | utf8TooLarge,                     // B
	utf8TooShort, utf8TooShort, utf8TooShort, utf8TooShort, // C..F: lead
}

// validUTF8x16 reports whether b is well-formed UTF-8 (same accept set as
// unicode/utf8.Valid — verified by TestValidUTF8SIMD_Parity).
func validUTF8x16(b []byte) bool {
	lut1 := archsimd.LoadUint8x16Slice(utf8Byte1High[:])
	lut2 := archsimd.LoadUint8x16Slice(utf8Byte1Low[:])
	lut3 := archsimd.LoadUint8x16Slice(utf8Byte2High[:])
	nib := archsimd.BroadcastUint8x16(0x0F)
	// Saturating-sub thresholds: result has the high bit set iff the byte is
	// ≥ 0xE0 (3+-byte lead two back) / ≥ 0xF0 (4-byte lead three back).
	sub3 := archsimd.BroadcastUint8x16(0xE0 - 0x80)
	sub4 := archsimd.BroadcastUint8x16(0xF0 - 0x80)
	high := archsimd.BroadcastUint8x16(0x80)
	zero := archsimd.BroadcastUint8x16(0)
	prev := zero
	errAcc := zero
	// One iteration past the end runs the all-zero epilogue block.
	for i := 0; i < len(b)+16; i += 16 {
		var c archsimd.Uint8x16
		switch {
		case i+16 <= len(b):
			c = archsimd.LoadUint8x16Slice(b[i:])
		case i < len(b):
			c = archsimd.LoadUint8x16SlicePart(b[i:]) // zero-padded tail
		default:
			c = zero
		}
		prev1 := c.ConcatShiftBytesRight(15, prev)
		prev1Hi := prev1.AsUint16x8().ShiftAllRight(4).AsUint8x16().And(nib)
		curHi := c.AsUint16x8().ShiftAllRight(4).AsUint8x16().And(nib)
		sc := lut1.PermuteOrZero(prev1Hi.AsInt8x16()).
			And(lut2.PermuteOrZero(prev1.And(nib).AsInt8x16())).
			And(lut3.PermuteOrZero(curHi.AsInt8x16()))
		prev2 := c.ConcatShiftBytesRight(14, prev)
		prev3 := c.ConcatShiftBytesRight(13, prev)
		must23_80 := prev2.SubSaturated(sub3).Or(prev3.SubSaturated(sub4)).And(high)
		errAcc = errAcc.Or(must23_80.Xor(sc))
		prev = c
	}
	return errAcc.IsZero()
}
