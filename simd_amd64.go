//go:build goexperiment.simd

// Fused single-pass string scanners over AVX / AVX2 / AVX-512. One vector
// pass per iteration classifies the closing '"', '\\', and control bytes
// (< 0x20) simultaneously — replacing String's three passes (two
// bytes.IndexByte + the SWAR ctrl pass).
//
// Emitted by ggen when invoked under GOEXPERIMENT=simd; the tier is fixed at
// generate time (-simd flag). No runtime feature probing — calling a tier the
// CPU lacks faults. Building this file requires GOEXPERIMENT=simd.
//
// Tail loads use Load*Part (zero-filled); padding zeroes register as
// control bytes, filtered by the k < len(rest) position check.

package ggen

import (
	"bytes"
	"math/bits"
	"simd/archsimd"
	"unsafe"
)

// classifyStructural finishes a fused scan: rest[k] is the first structural
// byte in the string body. Mirrors String's semantics exactly — alias
// return on '"' (validating UTF-8 when the accumulated lane OR saw a high
// byte; hasHigh may over-report bytes past k in the hit lane — utf8.Valid on
// the exact span settles it), stringSlow handoff on '\\' (scratch sized off
// the real unescaped closing quote via stringSpanEnd, as String does;
// validates its own output), and on a control byte the scalar error split:
// ErrUnterminated when the tail carries neither a closing quote nor a
// backslash (scalar returns it without the ctrl check), ErrBadString otherwise.
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
			return stringSlow(data, start, start+k, k+16, validate)
		}
		// closeIdx is the FIRST '"', possibly an escaped `\"`; size off the real
		// unescaped closing quote so an early escaped quote doesn't under-allocate
		// the scratch into the growth chain (matches String).
		return stringSlow(data, start, start+k, stringSpanEnd(data, start)-start, validate)
	default:
		p, err := ctrlHitPos(data, start, rest[k:])
		return "", p, err
	}
}

// classifyStructural64 is classifyStructural for the avx512 tier only, routing
// UTF-8 validation to the 64-lane validator (3× the 16-lane one from ~1 KB,
// −35% at 128 B; it falls back to validUTF8x16 below its own gate). It exists
// as a COPY because classifyStructural is shared by all three tiers and links
// into avx/avx2-tier binaries, which must contain no 512-bit code on a path
// they execute. Keep the two bodies in sync — the only difference is the
// validator call.
func classifyStructural64(data, rest []byte, start, k int, hasHigh, validate bool) (string, int, error) {
	switch rest[k] {
	case '"':
		// Pick the width HERE rather than letting validUTF8x64 delegate: a
		// short span would otherwise pay an extra (non-inlinable) frame just
		// to bounce to validUTF8x16, which measured +12% at a 16 B span.
		if hasHigh {
			span := rest[:k]
			ok := true
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
			return stringSlow(data, start, start+k, k+16, validate)
		}
		return stringSlow(data, start, start+k, stringSpanEnd(data, start)-start, validate)
	default:
		p, err := ctrlHitPos(data, start, rest[k:])
		return "", p, err
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

// ctrlHitPos pairs ctrlHitErr with the scalar-identical error position:
// ErrUnterminated ends at len(data) (ran off the end), ErrBadString at the
// span start — where scalar String's checkSpan/stringSlow-prefix arms report.
func ctrlHitPos(data []byte, start int, rest []byte) (int, error) {
	err := ctrlHitErr(rest)
	if err == ErrUnterminated {
		return len(data), err
	}
	return start, err
}

// StringAVX is String scanning 16 bytes/iteration (128-bit vectors).
func StringAVX(data []byte, i int, validate bool) (string, int, error) {
	if i >= len(data) || data[i] != '"' {
		return "", i, ErrExpectString
	}
	start := i + 1
	rest := data[start:]
	quote := archsimd.BroadcastUint8x16('"')
	bslash := archsimd.BroadcastUint8x16('\\')
	// v <= 0x1F ⇔ min(v, 0x1F) == v — VPMINUB+VPCMPEQB. Unsigned Less is
	// emulated below 512-bit (xor-0x80 re-broadcast every iteration).
	ctrl := archsimd.BroadcastUint8x16(0x1F)
	ascii := archsimd.BroadcastUint8x16(0x7F)
	// acc ORs every scanned lane; min(acc,0x7F)==acc iff no byte ≥ 0x80 —
	// one VPOR per lane so pure-ASCII spans skip the utf8.Valid walk.
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

// StringAVX2 is String scanning 32 bytes/iteration (256-bit vectors).
func StringAVX2(data []byte, i int, validate bool) (string, int, error) {
	if i >= len(data) || data[i] != '"' {
		return "", i, ErrExpectString
	}
	start := i + 1
	rest := data[start:]
	quote := archsimd.BroadcastUint8x32('"')
	bslash := archsimd.BroadcastUint8x32('\\')
	// Min/Equal in place of emulated unsigned Less — see StringAVX.
	ctrl := archsimd.BroadcastUint8x32(0x1F)
	ascii := archsimd.BroadcastUint8x32(0x7F)
	acc := archsimd.BroadcastUint8x32(0) // lane OR — see StringAVX
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

// StringAVX512 is String scanning 64 bytes/iteration (512-bit vectors).
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
	acc := archsimd.BroadcastUint8x64(0) // lane OR — see StringAVX
	j := 0
	// Mask.Or on 512-bit round-trips the vector domain (VPMOVM2B+VPORD+
	// VPMOVB2M); ToBits each mask (KMOVQ) and OR in scalar registers instead.
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
		v, _ := archsimd.LoadUint8x64Part(rest[j:])
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
