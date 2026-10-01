//go:build goexperiment.simd

// Vector string scanners. SIMD (vector) code processes 16, 32 or 64 bytes
// with one CPU instruction. AVX, AVX2 and AVX-512 are the x86 vector
// instruction sets for 16-, 32- and 64-byte vectors. A lane is one byte slot
// inside a vector. A mask is one bit per lane, set where a test matched.
//
// Each loop step tests a whole vector for the closing '"', a '\\' and a
// control byte (< 0x20) at once. The scalar String needs separate passes for
// these: bytes.IndexByte for the quote, again for the backslash, and a
// control-byte check over the body.
//
// Generated code calls these when ggen runs with the -simd flag. The flag
// fixes the tier at generate time. Nothing probes the CPU at run time, so
// calling a tier the CPU does not support crashes with an illegal
// instruction. Building this file requires GOEXPERIMENT=simd.
//
// The 16- and 32-byte tiers load the last partial vector with Load*Part.
// It zero-fills the missing lanes and is built from scalar loads, so it never
// reads past the slice. The zero padding looks like control bytes, so every
// hit is checked with k < len(rest). LoadUint8x64Part is not used: archsimd
// turns it into a full 64-byte load that can read up to 63 bytes past the
// slice. That faults when the input ends at a page edge, for example in an
// mmap'd file or a buffer from foreign code. StringAVX512 builds its own last
// vector instead.

package ggen

import (
	"bytes"
	"math/bits"
	"unsafe"

	"simd/archsimd"
)

// classifyStructural finishes a vector scan. rest[k] is the first quote,
// backslash or control byte in the string body. Results match String
// exactly:
//   - On '"' it returns a string that aliases data, meaning it points into
//     the caller's bytes with no copy. If hasHigh is set it first checks that
//     the body is valid UTF-8. hasHigh can be a false positive, because it
//     also covers bytes past k in the last vector. Validating the exact body
//     settles it.
//   - On '\\' it hands off to stringSlow, which decodes escapes and validates
//     its own output. The scratch buffer is sized from the real closing
//     quote, found with stringSpanEnd, as String does. If no quote follows,
//     stringUnterminated reports the error.
//   - On a control byte it returns ErrBadString at that byte, the same
//     position String reports.
func classifyStructural(data, rest []byte, start, k int, hasHigh, validate bool) (string, int, error) {
	switch rest[k] {
	case '"':
		if hasHigh && !validUTF8x16(rest[:k]) {
			return "", start, ErrInvalidUTF8
		}
		return unsafe.String(unsafe.SliceData(rest), k), start + k + 1, nil
	case '\\':
		closeIdx := bytes.IndexByte(rest[k:], '"')
		if closeIdx < 0 {
			return stringUnterminated(data, start, start+k, validate)
		}
		// closeIdx is the first '"', which may be an escaped `\"`. Size the
		// scratch from the real closing quote instead, so an early escaped
		// quote does not make it too small and force it to grow. String does
		// the same.
		return stringSlow(data, start, start+k, stringSpanEnd(data, start)-start, validate)
	default:
		return "", start + k, ErrBadString
	}
}

// classifyStructural64 is classifyStructural for the avx512 tier. It sends
// long bodies to the 64-byte UTF-8 validator, which is several times faster
// than the 16-byte one on long input. It is a copy because classifyStructural
// is shared by all three tiers and is linked into avx and avx2 binaries.
// Those must never execute 512-bit code. Keep the two bodies in sync. The
// only difference is the validator call.
func classifyStructural64(data, rest []byte, start, k int, hasHigh, validate bool) (string, int, error) {
	switch rest[k] {
	case '"':
		// Pick the width here instead of letting validUTF8x64 delegate.
		// Otherwise a short span pays for an extra call frame that cannot
		// be inlined just to reach validUTF8x16, which measured slower.
		if hasHigh {
			span := rest[:k]
			var ok bool
			if len(span) < utf8x64MinLen {
				ok = validUTF8x16(span)
			} else {
				ok = validUTF8x64(span)
			}
			if !ok {
				return "", start, ErrInvalidUTF8
			}
		}
		return unsafe.String(unsafe.SliceData(rest), k), start + k + 1, nil
	case '\\':
		closeIdx := bytes.IndexByte(rest[k:], '"')
		if closeIdx < 0 {
			return stringUnterminated(data, start, start+k, validate)
		}
		return stringSlow(data, start, start+k, stringSpanEnd(data, start)-start, validate)
	default:
		return "", start + k, ErrBadString
	}
}

// StringAVX is String scanning 16 bytes per step with 128-bit vectors.
func StringAVX(data []byte, i int, validate bool) (string, int, error) {
	if i >= len(data) || data[i] != '"' {
		return "", i, ErrExpectString
	}
	start := i + 1
	rest := data[start:]
	quote := archsimd.BroadcastUint8x16('"')
	bslash := archsimd.BroadcastUint8x16('\\')
	// v <= 0x1F is the same as min(v, 0x1F) == v, which is two instructions.
	// Below 512 bits the CPU has no unsigned byte compare, so Less would be
	// emulated with extra work on every step.
	ctrl := archsimd.BroadcastUint8x16(0x1F)
	ascii := archsimd.BroadcastUint8x16(0x7F)
	// acc ORs together every vector scanned. min(acc, 0x7F) == acc only when
	// no byte was >= 0x80. That costs one OR per step and lets pure ASCII
	// bodies skip UTF-8 validation.
	acc := archsimd.BroadcastUint8x16(0)
	j := 0
	for ; j+16 <= len(rest); j += 16 {
		v := archsimd.LoadUint8x16(rest[j:])
		acc = acc.Or(v)
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			hi := validate && acc.Min(ascii).Equal(acc).ToBits() != 0xFFFF
			return classifyStructural(data, rest, start, j+bits.TrailingZeros16(m), hi, validate)
		}
	}
	if j < len(rest) {
		v, _ := archsimd.LoadUint8x16Part(rest[j:])
		acc = acc.Or(v)
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			if k := j + bits.TrailingZeros16(m); k < len(rest) {
				hi := validate && acc.Min(ascii).Equal(acc).ToBits() != 0xFFFF
				return classifyStructural(data, rest, start, k, hi, validate)
			}
		}
	}
	return "", len(data), ErrUnterminated
}

// StringAVX2 is String scanning 32 bytes per step with 256-bit vectors.
func StringAVX2(data []byte, i int, validate bool) (string, int, error) {
	if i >= len(data) || data[i] != '"' {
		return "", i, ErrExpectString
	}
	start := i + 1
	rest := data[start:]
	quote := archsimd.BroadcastUint8x32('"')
	bslash := archsimd.BroadcastUint8x32('\\')
	// Min and Equal instead of an emulated unsigned Less. See StringAVX.
	ctrl := archsimd.BroadcastUint8x32(0x1F)
	ascii := archsimd.BroadcastUint8x32(0x7F)
	acc := archsimd.BroadcastUint8x32(0) // OR of all bytes, see StringAVX
	j := 0
	for ; j+32 <= len(rest); j += 32 {
		v := archsimd.LoadUint8x32(rest[j:])
		acc = acc.Or(v)
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			hi := validate && acc.Min(ascii).Equal(acc).ToBits() != 0xFFFFFFFF
			return classifyStructural(data, rest, start, j+bits.TrailingZeros32(m), hi, validate)
		}
	}
	if j < len(rest) {
		v, _ := archsimd.LoadUint8x32Part(rest[j:])
		acc = acc.Or(v)
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			if k := j + bits.TrailingZeros32(m); k < len(rest) {
				hi := validate && acc.Min(ascii).Equal(acc).ToBits() != 0xFFFFFFFF
				return classifyStructural(data, rest, start, k, hi, validate)
			}
		}
	}
	return "", len(data), ErrUnterminated
}

// StringAVX512 is String scanning 64 bytes per step with 512-bit vectors.
func StringAVX512(data []byte, i int, validate bool) (string, int, error) {
	if i >= len(data) || data[i] != '"' {
		return "", i, ErrExpectString
	}
	start := i + 1
	rest := data[start:]
	quote := archsimd.BroadcastUint8x64('"')
	bslash := archsimd.BroadcastUint8x64('\\')
	space := archsimd.BroadcastUint8x64(0x20)
	high := archsimd.BroadcastUint8x64(0x80)
	acc := archsimd.BroadcastUint8x64(0) // OR of all bytes, see StringAVX
	j := 0
	// At 512 bits, Mask.Or moves the masks back into vector registers and out
	// again, which takes three instructions. Converting each mask to a uint64
	// with ToBits and ORing those is cheaper.
	for ; j+64 <= len(rest); j += 64 {
		v := archsimd.LoadUint8x64(rest[j:])
		acc = acc.Or(v)
		m := v.Equal(quote).ToBits() | v.Equal(bslash).ToBits() | v.Less(space).ToBits()
		if m != 0 {
			hi := validate && acc.Less(high).ToBits() != ^uint64(0)
			return classifyStructural64(data, rest, start, j+bits.TrailingZeros64(m), hi, validate)
		}
	}
	if j < len(rest) {
		// Load the last partial vector without reading past len(rest). If
		// rest has at least 64 bytes, reload its last 64 bytes. The overlap
		// with the previous step was already found clean, so it adds no mask
		// bits. A shorter rest is copied into a zeroed array on the stack.
		// Its zero padding looks like control bytes, so the k < len(rest)
		// check filters it out.
		var v archsimd.Uint8x64
		if len(rest) >= 64 {
			j = len(rest) - 64
			v = archsimd.LoadUint8x64(rest[j:])
		} else {
			var lane [64]byte
			copy(lane[:], rest[j:])
			v = archsimd.LoadUint8x64(lane[:])
		}
		acc = acc.Or(v)
		m := v.Equal(quote).ToBits() | v.Equal(bslash).ToBits() | v.Less(space).ToBits()
		if m != 0 {
			if k := j + bits.TrailingZeros64(m); k < len(rest) {
				hi := validate && acc.Less(high).ToBits() != ^uint64(0)
				return classifyStructural64(data, rest, start, k, hi, validate)
			}
		}
	}
	return "", len(data), ErrUnterminated
}
