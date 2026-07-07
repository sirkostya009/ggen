//go:build goexperiment.simd

// Fused single-pass string scanners over AVX / AVX2 / AVX-512. One vector
// pass per iteration classifies the closing '"', '\\', and control bytes
// (< 0x20) simultaneously — replacing scan.String's three passes (two
// bytes.IndexByte + the SWAR ctrl pass).
//
// Emitted by ggen when invoked under GOEXPERIMENT=simd; the tier is fixed at
// generate time (-simd flag). No runtime feature probing — calling a tier the
// CPU lacks faults. Building this file requires GOEXPERIMENT=simd.
//
// Tail loads use Load*SlicePart (zero-filled); padding zeroes register as
// control bytes, filtered by the k < len(rest) position check.

package scan

import (
	"bytes"
	"math/bits"
	"simd/archsimd"
	"unsafe"
)

// classifyStructural finishes a fused scan: rest[k] is the first structural
// byte in the string body. Mirrors scan.String's semantics exactly — alias
// return on '"', stringSlow handoff on '\\' (same capHint), and on a control
// byte the scalar error split: ErrUnterminated when the tail carries neither
// a closing quote nor a backslash (scalar returns it without the ctrl
// check), ErrBadString otherwise.
func classifyStructural(data, rest []byte, start, k int) (string, int, error) {
	switch rest[k] {
	case '"':
		return unsafe.String(unsafe.SliceData(rest), k), start + k + 1, nil
	case '\\':
		closeIdx := bytes.IndexByte(rest[k:], '"')
		if closeIdx < 0 {
			return stringSlow(data, start, start+k, k+16)
		}
		return stringSlow(data, start, start+k, k+closeIdx)
	default:
		return "", 0, ctrlHitErr(rest[k:])
	}
}

// ctrlHitErr picks the scalar-identical error for a ctrl-byte hit: the
// scalar scanners return ErrUnterminated for a truncated string with no
// backslash before checking ctrl bytes. Cold path — malformed input only.
func ctrlHitErr(rest []byte) error {
	if bytes.IndexByte(rest, '"') < 0 && bytes.IndexByte(rest, '\\') < 0 {
		return ErrUnterminated
	}
	return ErrBadString
}

// StringAVX is scan.String scanning 16 bytes/iteration (128-bit vectors).
func StringAVX(data []byte, i int) (string, int, error) {
	if i >= len(data) || data[i] != '"' {
		return "", 0, ErrExpectString
	}
	start := i + 1
	rest := data[start:]
	quote := archsimd.BroadcastUint8x16('"')
	bslash := archsimd.BroadcastUint8x16('\\')
	// v <= 0x1F ⇔ min(v, 0x1F) == v — VPMINUB+VPCMPEQB. Unsigned Less is
	// emulated below 512-bit (xor-0x80 re-broadcast every iteration).
	ctrl := archsimd.BroadcastUint8x16(0x1F)
	j := 0
	for ; j+16 <= len(rest); j += 16 {
		v := archsimd.LoadUint8x16Slice(rest[j:])
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			return classifyStructural(data, rest, start, j+bits.TrailingZeros16(m))
		}
	}
	if j < len(rest) {
		v := archsimd.LoadUint8x16SlicePart(rest[j:])
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			if k := j + bits.TrailingZeros16(m); k < len(rest) {
				return classifyStructural(data, rest, start, k)
			}
		}
	}
	return "", 0, ErrUnterminated
}

// StringAVX2 is scan.String scanning 32 bytes/iteration (256-bit vectors).
func StringAVX2(data []byte, i int) (string, int, error) {
	if i >= len(data) || data[i] != '"' {
		return "", 0, ErrExpectString
	}
	start := i + 1
	rest := data[start:]
	quote := archsimd.BroadcastUint8x32('"')
	bslash := archsimd.BroadcastUint8x32('\\')
	// Min/Equal in place of emulated unsigned Less — see StringAVX.
	ctrl := archsimd.BroadcastUint8x32(0x1F)
	j := 0
	for ; j+32 <= len(rest); j += 32 {
		v := archsimd.LoadUint8x32Slice(rest[j:])
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			return classifyStructural(data, rest, start, j+bits.TrailingZeros32(m))
		}
	}
	if j < len(rest) {
		v := archsimd.LoadUint8x32SlicePart(rest[j:])
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			if k := j + bits.TrailingZeros32(m); k < len(rest) {
				return classifyStructural(data, rest, start, k)
			}
		}
	}
	return "", 0, ErrUnterminated
}

// StringAVX512 is scan.String scanning 64 bytes/iteration (512-bit vectors).
func StringAVX512(data []byte, i int) (string, int, error) {
	if i >= len(data) || data[i] != '"' {
		return "", 0, ErrExpectString
	}
	start := i + 1
	rest := data[start:]
	quote := archsimd.BroadcastUint8x64('"')
	bslash := archsimd.BroadcastUint8x64('\\')
	space := archsimd.BroadcastUint8x64(0x20)
	j := 0
	// Mask.Or on 512-bit round-trips the vector domain (VPMOVM2B+VPORD+
	// VPMOVB2M); ToBits each mask (KMOVQ) and OR in scalar registers instead.
	for ; j+64 <= len(rest); j += 64 {
		v := archsimd.LoadUint8x64Slice(rest[j:])
		m := v.Equal(quote).ToBits() | v.Equal(bslash).ToBits() | v.Less(space).ToBits()
		if m != 0 {
			return classifyStructural(data, rest, start, j+bits.TrailingZeros64(m))
		}
	}
	if j < len(rest) {
		v := archsimd.LoadUint8x64SlicePart(rest[j:])
		m := v.Equal(quote).ToBits() | v.Equal(bslash).ToBits() | v.Less(space).ToBits()
		if m != 0 {
			if k := j + bits.TrailingZeros64(m); k < len(rest) {
				return classifyStructural(data, rest, start, k)
			}
		}
	}
	return "", 0, ErrUnterminated
}
