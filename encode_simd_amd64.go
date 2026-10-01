//go:build goexperiment.simd

// Vector escape scans for AppendString and AppendStringNoHTML. SIMD (vector)
// code processes 16, 32 or 64 bytes with one CPU instruction. AVX, AVX2 and
// AVX-512 are the x86 vector instruction sets for those widths. A lane is one
// byte slot inside a vector. A mask is one bit per lane, set where a test
// matched.
//
// Each step tests a whole vector for every byte that needs escaping: '"',
// '\\', control bytes (< 0x20), and also '<', '>' and '&' in the HTML-safe
// versions. The loop then visits each set bit (m &= m-1 clears the lowest)
// and appends the clean bytes between hits in one append. The scalar versions
// check one byte at a time against a table instead.
//
// The caller contract is the same as the scalar pair. These append the
// escaped body and the closing '"'. The caller writes the opening '"'.
// Generated code calls these when ggen runs with the -simd flag, and the
// flag fixes the tier at generate time. Nothing probes the CPU at run time.
// Strings shorter than one vector go to the scalar version.
//
// The last bytes that do not fill a vector are scanned with vectors too. The
// code reloads the last full vector at s[len(s)-lane:], which is always in
// bounds because every function requires len(s) >= lane. Its mask is shifted
// right by lane-rem so the bits for bytes already written fall off.
// simdjson's string builder does the same. Only full-vector loads are used.
// Load*Part is a real function call, and its zero padding would look like
// control bytes and add escapes that are not there.
//
// Below 512 bits the CPU has no unsigned byte compare, so Less would be
// emulated with extra work every step. The 16- and 32-byte versions test
// control bytes with min(v, 0x1F) == v instead, which is two instructions.
// At 512 bits, Mask.Or moves masks back into vector registers and out again.
// So the 64-byte versions convert each compare to a uint64 with ToBits and OR
// those.

package ggen

import (
	"math/bits"
	"unsafe"

	"simd/archsimd"
)

// appendEscapedByte appends the JSON escape for c. Every tier shares it, and
// it runs only on bytes that need escaping. It matches the scalar switch
// exactly. The default arm writes \u00XX, which covers control bytes and the
// HTML escapes for '<', '>' and '&'.
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

// escapeTail finishes the last bytes that did not fill a vector and writes
// the closing quote. start is where the clean bytes not yet appended begin.
// m is the mask for the last full vector, reloaded at s[len(s)-lane:] and
// shifted right so bit 0 is s[j]. Bits for bytes before j were already
// written and are shifted out. The reload is always in bounds because every
// tier requires len(s) >= lane. So the tail needs neither a byte-by-byte
// table walk nor Load*Part, a real call whose zero padding would look like
// control bytes and add escapes that are not there.
func escapeTail(dst []byte, s string, start, j int, m uint64) []byte {
	for m != 0 {
		k := j + bits.TrailingZeros64(m)
		if start < k {
			dst = append(dst, s[start:k]...)
		}
		dst = appendEscapedByte(dst, s[k])
		start = k + 1
		m &= m - 1
	}
	if start < len(s) {
		dst = append(dst, s[start:]...)
	}
	return append(dst, '"')
}

// AppendStringNoHTMLAVX is AppendStringNoHTML scanning 16 bytes per step.
func AppendStringNoHTMLAVX(dst []byte, s string) []byte {
	if len(s) < 16 {
		return AppendStringNoHTML(dst, s)
	}
	q := archsimd.BroadcastUint8x16('"')
	bs := archsimd.BroadcastUint8x16('\\')
	ctrl := archsimd.BroadcastUint8x16(0x1F)
	start, j := 0, 0
	for ; j+16 <= len(s); j += 16 {
		v := archsimd.LoadUint8x16(byteview(s)[j:])
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
	var tm uint64
	if rem := len(s) - j; rem > 0 {
		v := archsimd.LoadUint8x16(byteview(s)[len(s)-16:])
		tm = uint64(v.Equal(q).Or(v.Equal(bs)).Or(v.Min(ctrl).Equal(v)).ToBits()) >> (16 - uint(rem))
	}
	return escapeTail(dst, s, start, j, tm)
}

// AppendStringNoHTMLAVX2 is AppendStringNoHTML scanning 32 bytes per step.
func AppendStringNoHTMLAVX2(dst []byte, s string) []byte {
	if len(s) < 32 {
		return AppendStringNoHTML(dst, s)
	}
	q := archsimd.BroadcastUint8x32('"')
	bs := archsimd.BroadcastUint8x32('\\')
	ctrl := archsimd.BroadcastUint8x32(0x1F)
	start, j := 0, 0
	for ; j+32 <= len(s); j += 32 {
		v := archsimd.LoadUint8x32(byteview(s)[j:])
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
	var tm uint64
	if rem := len(s) - j; rem > 0 {
		v := archsimd.LoadUint8x32(byteview(s)[len(s)-32:])
		tm = uint64(v.Equal(q).Or(v.Equal(bs)).Or(v.Min(ctrl).Equal(v)).ToBits()) >> (32 - uint(rem))
	}
	return escapeTail(dst, s, start, j, tm)
}

// AppendStringNoHTMLAVX512 is AppendStringNoHTML scanning 64 bytes per step.
func AppendStringNoHTMLAVX512(dst []byte, s string) []byte {
	if len(s) < 64 {
		return AppendStringNoHTML(dst, s)
	}
	q := archsimd.BroadcastUint8x64('"')
	bs := archsimd.BroadcastUint8x64('\\')
	space := archsimd.BroadcastUint8x64(0x20)
	start, j := 0, 0
	for ; j+64 <= len(s); j += 64 {
		v := archsimd.LoadUint8x64(byteview(s)[j:])
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
	var tm uint64
	if rem := len(s) - j; rem > 0 {
		v := archsimd.LoadUint8x64(byteview(s)[len(s)-64:])
		tm = (v.Equal(q).ToBits() | v.Equal(bs).ToBits() | v.Less(space).ToBits()) >> (64 - uint(rem))
	}
	return escapeTail(dst, s, start, j, tm)
}

// AppendStringAVX is the HTML-safe AppendString, 16 bytes per step.
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
		v := archsimd.LoadUint8x16(byteview(s)[j:])
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
	var tm uint64
	if rem := len(s) - j; rem > 0 {
		v := archsimd.LoadUint8x16(byteview(s)[len(s)-16:])
		tm = uint64(v.Equal(q).Or(v.Equal(bs)).Or(v.Min(ctrl).Equal(v)).
			Or(v.Equal(lt)).Or(v.Equal(gt)).Or(v.Equal(amp)).ToBits()) >> (16 - uint(rem))
	}
	return escapeTail(dst, s, start, j, tm)
}

// AppendStringAVX2 is the HTML-safe AppendString, 32 bytes per step.
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
		v := archsimd.LoadUint8x32(byteview(s)[j:])
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
	var tm uint64
	if rem := len(s) - j; rem > 0 {
		v := archsimd.LoadUint8x32(byteview(s)[len(s)-32:])
		tm = uint64(v.Equal(q).Or(v.Equal(bs)).Or(v.Min(ctrl).Equal(v)).
			Or(v.Equal(lt)).Or(v.Equal(gt)).Or(v.Equal(amp)).ToBits()) >> (32 - uint(rem))
	}
	return escapeTail(dst, s, start, j, tm)
}

// AppendStringAVX512 is the HTML-safe AppendString, 64 bytes per step.
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
		v := archsimd.LoadUint8x64(byteview(s)[j:])
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
	var tm uint64
	if rem := len(s) - j; rem > 0 {
		v := archsimd.LoadUint8x64(byteview(s)[len(s)-64:])
		tm = (v.Equal(q).ToBits() | v.Equal(bs).ToBits() | v.Less(space).ToBits() |
			v.Equal(lt).ToBits() | v.Equal(gt).ToBits() | v.Equal(amp).ToBits()) >> (64 - uint(rem))
	}
	return escapeTail(dst, s, start, j, tm)
}
