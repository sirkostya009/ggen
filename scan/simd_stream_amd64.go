//go:build goexperiment.simd

// Fused SIMD string scans for the Stream path. The bytes-path precondition
// (whole string contiguous in input) doesn't hold across refills, but each
// buffered window IS contiguous — so the tier cores keep stringView's
// refill/compaction loop bit-identical and only swap the three-pass locate
// (IndexByte ×2 + SWAR ctrl) for one fused structural-index pass per window.
//
// Per tier: structuralIndex* (first '"', '\\', or ctrl byte, -1 when the
// window is clean) + a stringView* core + thin String*/StringView*/KeyView*
// shells mirroring the scalar trio's contracts (String = owned copy,
// StringView/KeyView = alias). Generated stream decoders call one tier
// directly (ggen -simd); no runtime probing. KeyView*'s scalar prelude is
// dropped — the fused pass replaces it; ggen keeps plain KeyView for
// all-short-key structs where the prelude wins (same gate as the bytes path).

package scan

import (
	"math/bits"
	"simd/archsimd"
	"strings"
	"unsafe"
)

// structuralIndexAVX returns the index of the first structural byte ('"',
// '\\', ctrl < 0x20) in b, or -1. 16 bytes/iteration; scalar tail.
func structuralIndexAVX(b []byte) int {
	quote := archsimd.BroadcastUint8x16('"')
	bslash := archsimd.BroadcastUint8x16('\\')
	ctrl := archsimd.BroadcastUint8x16(0x1F)
	j := 0
	for ; j+16 <= len(b); j += 16 {
		v := archsimd.LoadUint8x16(b[j:])
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			return j + bits.TrailingZeros16(m)
		}
	}
	for ; j < len(b); j++ {
		if c := b[j]; c == '"' || c == '\\' || c < 0x20 {
			return j
		}
	}
	return -1
}

// structuralIndexAVX2 is structuralIndexAVX at 32 bytes/iteration.
func structuralIndexAVX2(b []byte) int {
	quote := archsimd.BroadcastUint8x32('"')
	bslash := archsimd.BroadcastUint8x32('\\')
	ctrl := archsimd.BroadcastUint8x32(0x1F)
	j := 0
	for ; j+32 <= len(b); j += 32 {
		v := archsimd.LoadUint8x32(b[j:])
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			return j + bits.TrailingZeros32(m)
		}
	}
	for ; j < len(b); j++ {
		if c := b[j]; c == '"' || c == '\\' || c < 0x20 {
			return j
		}
	}
	return -1
}

// structuralIndexAVX512 is structuralIndexAVX at 64 bytes/iteration.
func structuralIndexAVX512(b []byte) int {
	quote := archsimd.BroadcastUint8x64('"')
	bslash := archsimd.BroadcastUint8x64('\\')
	space := archsimd.BroadcastUint8x64(0x20)
	j := 0
	for ; j+64 <= len(b); j += 64 {
		v := archsimd.LoadUint8x64(b[j:])
		m := v.Equal(quote).ToBits() | v.Equal(bslash).ToBits() | v.Less(space).ToBits()
		if m != 0 {
			return j + bits.TrailingZeros64(m)
		}
	}
	for ; j < len(b); j++ {
		if c := b[j]; c == '"' || c == '\\' || c < 0x20 {
			return j
		}
	}
	return -1
}

// structuralIndexHighAVX is structuralIndexAVX fused with a high-bit (≥ 0x80)
// accumulate over the scanned prefix (through the hit lane; may over-report
// bytes past the hit within that lane — the caller's utf8.Valid on the exact
// span settles it). Separate from structuralIndexAVX so the skip tiers don't
// pay the extra OR. high is used by the string cores to skip utf8.Valid on
// pure-ASCII spans.
func structuralIndexHighAVX(b []byte) (int, bool) {
	quote := archsimd.BroadcastUint8x16('"')
	bslash := archsimd.BroadcastUint8x16('\\')
	ctrl := archsimd.BroadcastUint8x16(0x1F)
	ascii := archsimd.BroadcastUint8x16(0x7F)
	acc := archsimd.BroadcastUint8x16(0)
	j := 0
	for ; j+16 <= len(b); j += 16 {
		v := archsimd.LoadUint8x16(b[j:])
		acc = acc.Or(v)
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			return j + bits.TrailingZeros16(m), acc.Min(ascii).Equal(acc).ToBits() != 0xFFFF
		}
	}
	hi := acc.Min(ascii).Equal(acc).ToBits() != 0xFFFF
	var t byte
	for ; j < len(b); j++ {
		c := b[j]
		if c == '"' || c == '\\' || c < 0x20 {
			return j, hi || t&0x80 != 0
		}
		t |= c
	}
	return -1, hi || t&0x80 != 0
}

// structuralIndexHighAVX2 — see structuralIndexHighAVX.
func structuralIndexHighAVX2(b []byte) (int, bool) {
	quote := archsimd.BroadcastUint8x32('"')
	bslash := archsimd.BroadcastUint8x32('\\')
	ctrl := archsimd.BroadcastUint8x32(0x1F)
	ascii := archsimd.BroadcastUint8x32(0x7F)
	acc := archsimd.BroadcastUint8x32(0)
	j := 0
	for ; j+32 <= len(b); j += 32 {
		v := archsimd.LoadUint8x32(b[j:])
		acc = acc.Or(v)
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			return j + bits.TrailingZeros32(m), acc.Min(ascii).Equal(acc).ToBits() != 0xFFFFFFFF
		}
	}
	hi := acc.Min(ascii).Equal(acc).ToBits() != 0xFFFFFFFF
	var t byte
	for ; j < len(b); j++ {
		c := b[j]
		if c == '"' || c == '\\' || c < 0x20 {
			return j, hi || t&0x80 != 0
		}
		t |= c
	}
	return -1, hi || t&0x80 != 0
}

// structuralIndexHighAVX512 — see structuralIndexHighAVX.
func structuralIndexHighAVX512(b []byte) (int, bool) {
	quote := archsimd.BroadcastUint8x64('"')
	bslash := archsimd.BroadcastUint8x64('\\')
	space := archsimd.BroadcastUint8x64(0x20)
	high := archsimd.BroadcastUint8x64(0x80)
	acc := archsimd.BroadcastUint8x64(0)
	j := 0
	for ; j+64 <= len(b); j += 64 {
		v := archsimd.LoadUint8x64(b[j:])
		acc = acc.Or(v)
		m := v.Equal(quote).ToBits() | v.Equal(bslash).ToBits() | v.Less(space).ToBits()
		if m != 0 {
			return j + bits.TrailingZeros64(m), acc.Less(high).ToBits() != ^uint64(0)
		}
	}
	hi := acc.Less(high).ToBits() != ^uint64(0)
	var t byte
	for ; j < len(b); j++ {
		c := b[j]
		if c == '"' || c == '\\' || c < 0x20 {
			return j, hi || t&0x80 != 0
		}
		t |= c
	}
	return -1, hi || t&0x80 != 0
}

// stringViewAVX is (*Stream).stringView with the fused AVX locate. The
// refill/compaction bookkeeping mirrors the scalar loop exactly; only the
// window scan differs. Kept as three near-identical per-tier copies rather
// than a func-pointer core — the tier callee must be a direct call.
func (s *Stream) stringViewAVX(validate bool) (v string, owned bool, err error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", false, NotEOF(err, ErrExpectString)
		}
		i = 0
	}
	if s.buf[i] != '"' {
		return "", false, ErrExpectString
	}
	start := i + 1
	j := start
	sawHigh := false
	for {
		k, hi := structuralIndexHighAVX(s.buf[j:])
		sawHigh = sawHigh || hi
		if k < 0 {
			j = len(s.buf)
			err := s.ReadMore(start)
			j -= start
			start = 0
			if err != nil {
				return "", false, NotEOF(err, ErrUnterminated)
			}
			continue
		}
		switch s.buf[j+k] {
		case '"':
			end := j + k
			// Full-span check — a rune may straddle the window cursor j, so
			// per-window validation would false-error (see scalar stringView).
			if validate && sawHigh && !validUTF8x16(s.buf[start:end]) {
				return "", false, ErrInvalidUTF8
			}
			s.Pos = end + 1
			return unsafe.String(unsafe.SliceData(s.buf[start:]), end-start), false, nil
		case '\\':
			v, err := s.stringSlow(start, j+k, validate)
			return v, true, err
		default:
			return "", false, ErrBadString
		}
	}
}

// stringViewAVX2 — see stringViewAVX.
func (s *Stream) stringViewAVX2(validate bool) (v string, owned bool, err error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", false, NotEOF(err, ErrExpectString)
		}
		i = 0
	}
	if s.buf[i] != '"' {
		return "", false, ErrExpectString
	}
	start := i + 1
	j := start
	sawHigh := false
	for {
		k, hi := structuralIndexHighAVX2(s.buf[j:])
		sawHigh = sawHigh || hi
		if k < 0 {
			j = len(s.buf)
			err := s.ReadMore(start)
			j -= start
			start = 0
			if err != nil {
				return "", false, NotEOF(err, ErrUnterminated)
			}
			continue
		}
		switch s.buf[j+k] {
		case '"':
			end := j + k
			if validate && sawHigh && !validUTF8x16(s.buf[start:end]) {
				return "", false, ErrInvalidUTF8
			}
			s.Pos = end + 1
			return unsafe.String(unsafe.SliceData(s.buf[start:]), end-start), false, nil
		case '\\':
			v, err := s.stringSlow(start, j+k, validate)
			return v, true, err
		default:
			return "", false, ErrBadString
		}
	}
}

// stringViewAVX512 — see stringViewAVX.
func (s *Stream) stringViewAVX512(validate bool) (v string, owned bool, err error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", false, NotEOF(err, ErrExpectString)
		}
		i = 0
	}
	if s.buf[i] != '"' {
		return "", false, ErrExpectString
	}
	start := i + 1
	j := start
	sawHigh := false
	for {
		k, hi := structuralIndexHighAVX512(s.buf[j:])
		sawHigh = sawHigh || hi
		if k < 0 {
			j = len(s.buf)
			err := s.ReadMore(start)
			j -= start
			start = 0
			if err != nil {
				return "", false, NotEOF(err, ErrUnterminated)
			}
			continue
		}
		switch s.buf[j+k] {
		case '"':
			end := j + k
			// 64-lane validator — this core IS the avx512 tier, so 512-bit
			// code is safe here. Width picked at the call site so short spans
			// skip the extra frame (see classifyStructural64).
			if validate && sawHigh {
				span := s.buf[start:end]
				ok := true
				if len(span) < utf8x64MinLen {
					ok = validUTF8x16(span)
				} else {
					ok = validUTF8x64(span)
				}
				if !ok {
					return "", false, ErrInvalidUTF8
				}
			}
			s.Pos = end + 1
			return unsafe.String(unsafe.SliceData(s.buf[start:]), end-start), false, nil
		case '\\':
			v, err := s.stringSlow(start, j+k, validate)
			return v, true, err
		default:
			return "", false, ErrBadString
		}
	}
}

// StringAVX is Stream.String over the fused AVX scan.
func (s *Stream) StringAVX(validate bool) (string, error) {
	v, owned, err := s.stringViewAVX(validate)
	if err != nil {
		return "", err
	}
	if owned {
		return v, nil
	}
	return strings.Clone(v), nil
}

// StringAVX2 is Stream.String over the fused AVX2 scan.
func (s *Stream) StringAVX2(validate bool) (string, error) {
	v, owned, err := s.stringViewAVX2(validate)
	if err != nil {
		return "", err
	}
	if owned {
		return v, nil
	}
	return strings.Clone(v), nil
}

// StringAVX512 is Stream.String over the fused AVX-512 scan.
func (s *Stream) StringAVX512(validate bool) (string, error) {
	v, owned, err := s.stringViewAVX512(validate)
	if err != nil {
		return "", err
	}
	if owned {
		return v, nil
	}
	return strings.Clone(v), nil
}

// StringViewAVX is Stream.StringView over the fused AVX scan.
func (s *Stream) StringViewAVX(validate bool) (string, error) {
	v, _, err := s.stringViewAVX(validate)
	return v, err
}

// StringViewAVX2 is Stream.StringView over the fused AVX2 scan.
func (s *Stream) StringViewAVX2(validate bool) (string, error) {
	v, _, err := s.stringViewAVX2(validate)
	return v, err
}

// StringViewAVX512 is Stream.StringView over the fused AVX-512 scan.
func (s *Stream) StringViewAVX512(validate bool) (string, error) {
	v, _, err := s.stringViewAVX512(validate)
	return v, err
}

// KeyViewAVX is Stream.KeyView over the fused AVX scan (no scalar prelude —
// the fused pass replaces it; alias contract identical).
func (s *Stream) KeyViewAVX(validate bool) (string, error) {
	v, _, err := s.stringViewAVX(validate)
	return v, err
}

// KeyViewAVX2 is Stream.KeyView over the fused AVX2 scan.
func (s *Stream) KeyViewAVX2(validate bool) (string, error) {
	v, _, err := s.stringViewAVX2(validate)
	return v, err
}

// KeyViewAVX512 is Stream.KeyView over the fused AVX-512 scan.
func (s *Stream) KeyViewAVX512(validate bool) (string, error) {
	v, _, err := s.stringViewAVX512(validate)
	return v, err
}
