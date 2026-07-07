//go:build goexperiment.simd

// Fused SIMD escape scans for AppendString / AppendStringNoHTML. One vector
// pass per 16/32/64 bytes classifies every byte needing escape ('"', '\\',
// ctrl < 0x20, plus '<' '>' '&' on the HTML variants); set bits are iterated
// (m &= m-1) with clean spans bulk-appended between them — replacing the
// per-byte [256]bool table walk.
//
// Same caller contract as the scalar pair: append escaped body + closing '"',
// caller writes the opening '"'. Emitted by ggen when invoked under
// GOEXPERIMENT=simd (see cli -simd); tier fixed at generate time, no runtime
// probing. The ≤ lane-1 byte tail runs the scalar table walk — full-lane
// vector loads only (Load*SlicePart is a real call, and its zero padding
// would classify as ctrl and emit spurious escapes).
//
// Unsigned Less is emulated below 512-bit (re-broadcasts 0x80 per iteration),
// so the 16/32-lane ctrl test uses min(v,0x1F)==v (VPMINUB+VPCMPEQB); 512-bit
// Mask.Or round-trips the vector domain, so the 64-lane variants ToBits each
// compare (KMOVQ) and OR in scalar registers.

package encode

import (
	"math/bits"
	"simd/archsimd"
	"unsafe"
)

// appendEscapedByte appends the JSON escape for c — the shared cold path of
// every tier. Mirrors the scalar switch exactly (incl. \u00XX for ctrl and
// <-style HTML escapes via the default arm).
func appendEscapedByte(dst []byte, c byte) []byte {
	switch c {
	case '"':
		return append(dst, '\\', '"')
	case '\\':
		return append(dst, '\\', '\\')
	case '\n':
		return append(dst, '\\', 'n')
	case '\r':
		return append(dst, '\\', 'r')
	case '\t':
		return append(dst, '\\', 't')
	case '\b':
		return append(dst, '\\', 'b')
	case '\f':
		return append(dst, '\\', 'f')
	default:
		const hex = "0123456789abcdef"
		return append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xf])
	}
}

func byteview(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}

// scalarEscapeTail finishes the final < lane bytes with the table walk and
// writes the closing quote. start is the beginning of the pending clean span.
func scalarEscapeTail(dst []byte, s string, start, j int, table *[256]bool) []byte {
	for ; j < len(s); j++ {
		if !table[s[j]] {
			continue
		}
		if start < j {
			dst = append(dst, s[start:j]...)
		}
		dst = appendEscapedByte(dst, s[j])
		start = j + 1
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}

// AppendStringNoHTMLAVX is AppendStringNoHTML scanning 16 bytes/iteration.
func AppendStringNoHTMLAVX(dst []byte, s string) []byte {
	if len(s) < 16 {
		return AppendStringNoHTML(dst, s)
	}
	q := archsimd.BroadcastUint8x16('"')
	bs := archsimd.BroadcastUint8x16('\\')
	ctrl := archsimd.BroadcastUint8x16(0x1F)
	start, j := 0, 0
	for ; j+16 <= len(s); j += 16 {
		v := archsimd.LoadUint8x16Slice(byteview(s)[j:])
		m := v.Equal(q).Or(v.Equal(bs)).Or(v.Min(ctrl).Equal(v)).ToBits()
		for m != 0 {
			k := j + bits.TrailingZeros16(m)
			if start < k {
				dst = append(dst, s[start:k]...)
			}
			dst = appendEscapedByte(dst, s[k])
			start = k + 1
			m &= m - 1
		}
	}
	return scalarEscapeTail(dst, s, start, j, &needEscapeNoHTML)
}

// AppendStringNoHTMLAVX2 is AppendStringNoHTML scanning 32 bytes/iteration.
func AppendStringNoHTMLAVX2(dst []byte, s string) []byte {
	if len(s) < 32 {
		return AppendStringNoHTML(dst, s)
	}
	q := archsimd.BroadcastUint8x32('"')
	bs := archsimd.BroadcastUint8x32('\\')
	ctrl := archsimd.BroadcastUint8x32(0x1F)
	start, j := 0, 0
	for ; j+32 <= len(s); j += 32 {
		v := archsimd.LoadUint8x32Slice(byteview(s)[j:])
		m := v.Equal(q).Or(v.Equal(bs)).Or(v.Min(ctrl).Equal(v)).ToBits()
		for m != 0 {
			k := j + bits.TrailingZeros32(m)
			if start < k {
				dst = append(dst, s[start:k]...)
			}
			dst = appendEscapedByte(dst, s[k])
			start = k + 1
			m &= m - 1
		}
	}
	return scalarEscapeTail(dst, s, start, j, &needEscapeNoHTML)
}

// AppendStringNoHTMLAVX512 is AppendStringNoHTML scanning 64 bytes/iteration.
func AppendStringNoHTMLAVX512(dst []byte, s string) []byte {
	if len(s) < 64 {
		return AppendStringNoHTML(dst, s)
	}
	q := archsimd.BroadcastUint8x64('"')
	bs := archsimd.BroadcastUint8x64('\\')
	space := archsimd.BroadcastUint8x64(0x20)
	start, j := 0, 0
	for ; j+64 <= len(s); j += 64 {
		v := archsimd.LoadUint8x64Slice(byteview(s)[j:])
		m := v.Equal(q).ToBits() | v.Equal(bs).ToBits() | v.Less(space).ToBits()
		for m != 0 {
			k := j + bits.TrailingZeros64(m)
			if start < k {
				dst = append(dst, s[start:k]...)
			}
			dst = appendEscapedByte(dst, s[k])
			start = k + 1
			m &= m - 1
		}
	}
	return scalarEscapeTail(dst, s, start, j, &needEscapeNoHTML)
}

// AppendStringAVX is AppendString (HTML-safe) scanning 16 bytes/iteration.
func AppendStringAVX(dst []byte, s string) []byte {
	if len(s) < 16 {
		return AppendString(dst, s)
	}
	q := archsimd.BroadcastUint8x16('"')
	bs := archsimd.BroadcastUint8x16('\\')
	ctrl := archsimd.BroadcastUint8x16(0x1F)
	lt := archsimd.BroadcastUint8x16('<')
	gt := archsimd.BroadcastUint8x16('>')
	amp := archsimd.BroadcastUint8x16('&')
	start, j := 0, 0
	for ; j+16 <= len(s); j += 16 {
		v := archsimd.LoadUint8x16Slice(byteview(s)[j:])
		m := v.Equal(q).Or(v.Equal(bs)).Or(v.Min(ctrl).Equal(v)).
			Or(v.Equal(lt)).Or(v.Equal(gt)).Or(v.Equal(amp)).ToBits()
		for m != 0 {
			k := j + bits.TrailingZeros16(m)
			if start < k {
				dst = append(dst, s[start:k]...)
			}
			dst = appendEscapedByte(dst, s[k])
			start = k + 1
			m &= m - 1
		}
	}
	return scalarEscapeTail(dst, s, start, j, &needEscapeHTML)
}

// AppendStringAVX2 is AppendString (HTML-safe) scanning 32 bytes/iteration.
func AppendStringAVX2(dst []byte, s string) []byte {
	if len(s) < 32 {
		return AppendString(dst, s)
	}
	q := archsimd.BroadcastUint8x32('"')
	bs := archsimd.BroadcastUint8x32('\\')
	ctrl := archsimd.BroadcastUint8x32(0x1F)
	lt := archsimd.BroadcastUint8x32('<')
	gt := archsimd.BroadcastUint8x32('>')
	amp := archsimd.BroadcastUint8x32('&')
	start, j := 0, 0
	for ; j+32 <= len(s); j += 32 {
		v := archsimd.LoadUint8x32Slice(byteview(s)[j:])
		m := v.Equal(q).Or(v.Equal(bs)).Or(v.Min(ctrl).Equal(v)).
			Or(v.Equal(lt)).Or(v.Equal(gt)).Or(v.Equal(amp)).ToBits()
		for m != 0 {
			k := j + bits.TrailingZeros32(m)
			if start < k {
				dst = append(dst, s[start:k]...)
			}
			dst = appendEscapedByte(dst, s[k])
			start = k + 1
			m &= m - 1
		}
	}
	return scalarEscapeTail(dst, s, start, j, &needEscapeHTML)
}

// AppendStringAVX512 is AppendString (HTML-safe) scanning 64 bytes/iteration.
func AppendStringAVX512(dst []byte, s string) []byte {
	if len(s) < 64 {
		return AppendString(dst, s)
	}
	q := archsimd.BroadcastUint8x64('"')
	bs := archsimd.BroadcastUint8x64('\\')
	space := archsimd.BroadcastUint8x64(0x20)
	lt := archsimd.BroadcastUint8x64('<')
	gt := archsimd.BroadcastUint8x64('>')
	amp := archsimd.BroadcastUint8x64('&')
	start, j := 0, 0
	for ; j+64 <= len(s); j += 64 {
		v := archsimd.LoadUint8x64Slice(byteview(s)[j:])
		m := v.Equal(q).ToBits() | v.Equal(bs).ToBits() | v.Less(space).ToBits() |
			v.Equal(lt).ToBits() | v.Equal(gt).ToBits() | v.Equal(amp).ToBits()
		for m != 0 {
			k := j + bits.TrailingZeros64(m)
			if start < k {
				dst = append(dst, s[start:k]...)
			}
			dst = appendEscapedByte(dst, s[k])
			start = k + 1
			m &= m - 1
		}
	}
	return scalarEscapeTail(dst, s, start, j, &needEscapeHTML)
}
