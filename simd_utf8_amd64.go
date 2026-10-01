//go:build goexperiment.simd

// Vector UTF-8 validation, using the lookup-table algorithm from Lemire and
// simdjson. SIMD (vector) code processes 16 bytes with one CPU instruction.
// A lane is one byte slot inside a vector.
//
// UTF-8 primer. A rune (one Unicode code point) takes 1 to 4 bytes. ASCII
// bytes are below 0x80 and stand alone. A lead byte (0xC2 to 0xF4) starts a
// multi-byte rune and its top bits say how many bytes follow. Each following
// byte is a continuation byte (0x80 to 0xBF). Input is invalid when:
//   - a lead byte is not followed by enough continuation bytes,
//   - a continuation byte appears without a lead byte,
//   - a rune uses more bytes than it needs (an overlong encoding),
//   - it encodes a surrogate (U+D800 to U+DFFF, reserved for UTF-16),
//   - or it encodes a value above U+10FFFF.
//
// The vector string scanners call this only when they saw a byte >= 0x80, so
// it only runs on bodies that contain non-ASCII text. On those, the scalar
// utf8.Valid was almost all of the decode cost.
//
// It works on 16-byte blocks and uses only AVX instructions, the oldest x86
// vector set. classifyStructural is shared by all three tiers, so the
// validator must run on the lowest one.
//
// For each byte, the pair (previous byte, current byte) is looked up in three
// nibble LUTs. A nibble LUT is a 16-entry table indexed by 4 bits of a byte:
//
//	sc = lut1[prev1>>4] & lut2[prev1&0xF] & lut3[cur>>4]
//
// Every illegal pair sets an error bit in sc. One legal case also sets a bit:
// a continuation byte after a continuation byte (utf8TwoConts, 0x80). That
// pair is legal inside 3- and 4-byte runes. must23_80 has 0x80 exactly where
// a third or fourth continuation byte is required. That is where the byte two
// back is >= 0xE0 or the byte three back is >= 0xF0, found with a saturating
// subtract (one that stops at 0 instead of wrapping). XOR with must23_80
// clears the legal case and flags a missing continuation. Anything left
// after the XOR is ORed into errAcc.
//
// End of input. The last partial block is loaded zero-padded. Zeros count as
// ASCII, so a rune cut short there shows up as a lead byte followed by a
// non-continuation byte. A rune cut short at the end of a full final block
// has no byte after it. Running a whole extra block of zeros through the
// lookups to test three bytes would waste work. Instead, after the loop, one
// saturating subtract against utf8MaxIncomplete tests those three positions
// directly. simdutf calls this check_eof. A padded block has zeros there, so
// the check does nothing for it.

package ggen

import "simd/archsimd"

const (
	utf8TooShort     = 1 << 0 // lead followed by a non-continuation byte
	utf8TooLong      = 1 << 1 // ASCII or continuation followed by continuation
	utf8Overlong3    = 1 << 2 // E0 followed by 80..9F
	utf8TooLarge     = 1 << 3 // F4 followed by 90..BF (> U+10FFFF)
	utf8Surrogate    = 1 << 4 // ED followed by A0..BF (U+D800..DFFF)
	utf8Overlong2    = 1 << 5 // C0 or C1 lead (< U+0080)
	utf8TooLarge1000 = 1 << 6 // F5..FF lead, never valid
	utf8Overlong4    = 1 << 6 // F0 followed by 80..8F, same bit, never overlaps
	utf8TwoConts     = 1 << 7 // continuation after continuation, legal if required

	utf8Carry = utf8TooShort | utf8TooLong | utf8TwoConts
)

// Indexed by the high nibble of the previous byte.
var utf8Byte1High = [16]uint8{
	utf8TooLong, utf8TooLong, utf8TooLong, utf8TooLong, // 0..3: ASCII
	utf8TooLong, utf8TooLong, utf8TooLong, utf8TooLong, // 4..7: ASCII
	utf8TwoConts, utf8TwoConts, utf8TwoConts, utf8TwoConts, // 8..B: continuation
	utf8TooShort | utf8Overlong2,                 // C: 2-byte lead (C0/C1 overlong via low nibble)
	utf8TooShort,                                 // D: 2-byte lead
	utf8TooShort | utf8Overlong3 | utf8Surrogate, // E: 3-byte lead
	utf8TooShort | utf8TooLarge | utf8TooLarge1000 | utf8Overlong4, // F: 4-byte lead
}

// Indexed by the low nibble of the previous byte.
var utf8Byte1Low = [16]uint8{
	utf8Carry | utf8Overlong3 | utf8Overlong2 | utf8Overlong4, // 0: E0/C0/F0
	utf8Carry | utf8Overlong2,                                 // 1: C1
	utf8Carry, utf8Carry,                                      // 2,3
	utf8Carry | utf8TooLarge,                                    // 4: F4
	utf8Carry | utf8TooLarge | utf8TooLarge1000,                 // 5: F5
	utf8Carry | utf8TooLarge | utf8TooLarge1000,                 // 6
	utf8Carry | utf8TooLarge | utf8TooLarge1000,                 // 7
	utf8Carry | utf8TooLarge | utf8TooLarge1000,                 // 8
	utf8Carry | utf8TooLarge | utf8TooLarge1000,                 // 9
	utf8Carry | utf8TooLarge | utf8TooLarge1000,                 // A
	utf8Carry | utf8TooLarge | utf8TooLarge1000,                 // B
	utf8Carry | utf8TooLarge | utf8TooLarge1000,                 // C
	utf8Carry | utf8TooLarge | utf8TooLarge1000 | utf8Surrogate, // D: ED lead
	utf8Carry | utf8TooLarge | utf8TooLarge1000,                 // E
	utf8Carry | utf8TooLarge | utf8TooLarge1000,                 // F
}

// Indexed by the high nibble of the current byte.
var utf8Byte2High = [16]uint8{
	utf8TooShort, utf8TooShort, utf8TooShort, utf8TooShort, // 0..3: ASCII
	utf8TooShort, utf8TooShort, utf8TooShort, utf8TooShort, // 4..7: ASCII
	utf8TooLong | utf8Overlong2 | utf8TwoConts | utf8Overlong3 | utf8TooLarge1000 | utf8Overlong4, // 8
	utf8TooLong | utf8Overlong2 | utf8TwoConts | utf8Overlong3 | utf8TooLarge,                     // 9
	utf8TooLong | utf8Overlong2 | utf8TwoConts | utf8Surrogate | utf8TooLarge,                     // A
	utf8TooLong | utf8Overlong2 | utf8TwoConts | utf8Surrogate | utf8TooLarge,                     // B
	utf8TooShort, utf8TooShort, utf8TooShort, utf8TooShort, // C..F: lead
}

// utf8MaxIncomplete holds, per lane, the largest byte that does not need more
// bytes after the end of the block. Only the last three lanes have a limit:
// below 0xF0 three from the end, below 0xE0 two from the end, below 0xC0 at
// the end. A saturating subtract against it is non-zero exactly when the
// block ends in the middle of a rune.
var utf8MaxIncomplete = [16]uint8{
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
	0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
	0xF0 - 1, 0xE0 - 1, 0xC0 - 1,
}

// validUTF8x16 reports whether b is valid UTF-8. It accepts exactly what
// unicode/utf8.Valid accepts, which TestValidUTF8SIMD_Parity checks.
func validUTF8x16(b []byte) bool {
	lut1 := archsimd.LoadUint8x16(utf8Byte1High[:])
	lut2 := archsimd.LoadUint8x16(utf8Byte1Low[:])
	lut3 := archsimd.LoadUint8x16(utf8Byte2High[:])
	nib := archsimd.BroadcastUint8x16(0x0F)
	// Saturating subtract thresholds. The result has its high bit set only
	// when the byte is >= 0xE0 (a 3- or 4-byte lead two back) or >= 0xF0 (a
	// 4-byte lead three back).
	sub3 := archsimd.BroadcastUint8x16(0xE0 - 0x80)
	sub4 := archsimd.BroadcastUint8x16(0xF0 - 0x80)
	high := archsimd.BroadcastUint8x16(0x80)
	zero := archsimd.BroadcastUint8x16(0)
	prev := zero
	errAcc := zero
	for i := 0; i < len(b); i += 16 {
		var c archsimd.Uint8x16
		if i+16 <= len(b) {
			c = archsimd.LoadUint8x16(b[i:])
		} else {
			c, _ = archsimd.LoadUint8x16Part(b[i:]) // zero-padded last block
		}
		prev1 := c.ConcatShiftBytesRight(prev, 15)
		prev1Hi := prev1.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(nib)
		curHi := c.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(nib)
		sc := lut1.PermuteOrZero(prev1Hi.BitsToInt8()).
			And(lut2.PermuteOrZero(prev1.And(nib).BitsToInt8())).
			And(lut3.PermuteOrZero(curHi.BitsToInt8()))
		prev2 := c.ConcatShiftBytesRight(prev, 14)
		prev3 := c.ConcatShiftBytesRight(prev, 13)
		must23_80 := prev2.SubSaturated(sub3).Or(prev3.SubSaturated(sub4)).And(high)
		errAcc = errAcc.Or(must23_80.Xor(sc))
		prev = c
	}
	// prev is the final block, or zero when b is empty. Reject a rune cut
	// short at the end of the input.
	maxInc := archsimd.LoadUint8x16(utf8MaxIncomplete[:])
	return errAcc.Or(prev.SubSaturated(maxInc)).IsZero()
}
