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
		v := archsimd.LoadUint8x16Slice(b[j:])
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
		v := archsimd.LoadUint8x32Slice(b[j:])
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
		v := archsimd.LoadUint8x64Slice(b[j:])
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

// stringViewAVX is (*Stream).stringView with the fused AVX locate. The
// refill/compaction bookkeeping mirrors the scalar loop exactly; only the
// window scan differs. Kept as three near-identical per-tier copies rather
// than a func-pointer core — the tier callee must be a direct call.
func (s *Stream) stringViewAVX() (v string, owned bool, err error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", false, err
		}
		i = 0
	}
	if s.buf[i] != '"' {
		return "", false, ErrExpectString
	}
	start := i + 1
	j := start
	for {
		k := structuralIndexAVX(s.buf[j:])
		if k < 0 {
			j = len(s.buf)
			err := s.ReadMore(start)
			j -= start
			start = 0
			if err != nil {
				return "", false, ErrUnterminated
			}
			continue
		}
		switch s.buf[j+k] {
		case '"':
			end := j + k
			s.Pos = end + 1
			return unsafe.String(unsafe.SliceData(s.buf[start:]), end-start), false, nil
		case '\\':
			v, err := s.stringSlow(start, j+k)
			return v, true, err
		default:
			return "", false, ErrBadString
		}
	}
}

// stringViewAVX2 — see stringViewAVX.
func (s *Stream) stringViewAVX2() (v string, owned bool, err error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", false, err
		}
		i = 0
	}
	if s.buf[i] != '"' {
		return "", false, ErrExpectString
	}
	start := i + 1
	j := start
	for {
		k := structuralIndexAVX2(s.buf[j:])
		if k < 0 {
			j = len(s.buf)
			err := s.ReadMore(start)
			j -= start
			start = 0
			if err != nil {
				return "", false, ErrUnterminated
			}
			continue
		}
		switch s.buf[j+k] {
		case '"':
			end := j + k
			s.Pos = end + 1
			return unsafe.String(unsafe.SliceData(s.buf[start:]), end-start), false, nil
		case '\\':
			v, err := s.stringSlow(start, j+k)
			return v, true, err
		default:
			return "", false, ErrBadString
		}
	}
}

// stringViewAVX512 — see stringViewAVX.
func (s *Stream) stringViewAVX512() (v string, owned bool, err error) {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return "", false, err
		}
		i = 0
	}
	if s.buf[i] != '"' {
		return "", false, ErrExpectString
	}
	start := i + 1
	j := start
	for {
		k := structuralIndexAVX512(s.buf[j:])
		if k < 0 {
			j = len(s.buf)
			err := s.ReadMore(start)
			j -= start
			start = 0
			if err != nil {
				return "", false, ErrUnterminated
			}
			continue
		}
		switch s.buf[j+k] {
		case '"':
			end := j + k
			s.Pos = end + 1
			return unsafe.String(unsafe.SliceData(s.buf[start:]), end-start), false, nil
		case '\\':
			v, err := s.stringSlow(start, j+k)
			return v, true, err
		default:
			return "", false, ErrBadString
		}
	}
}

// StringAVX is Stream.String over the fused AVX scan.
func (s *Stream) StringAVX() (string, error) {
	v, owned, err := s.stringViewAVX()
	if err != nil {
		return "", err
	}
	if owned {
		return v, nil
	}
	return strings.Clone(v), nil
}

// StringAVX2 is Stream.String over the fused AVX2 scan.
func (s *Stream) StringAVX2() (string, error) {
	v, owned, err := s.stringViewAVX2()
	if err != nil {
		return "", err
	}
	if owned {
		return v, nil
	}
	return strings.Clone(v), nil
}

// StringAVX512 is Stream.String over the fused AVX-512 scan.
func (s *Stream) StringAVX512() (string, error) {
	v, owned, err := s.stringViewAVX512()
	if err != nil {
		return "", err
	}
	if owned {
		return v, nil
	}
	return strings.Clone(v), nil
}

// StringViewAVX is Stream.StringView over the fused AVX scan.
func (s *Stream) StringViewAVX() (string, error) {
	v, _, err := s.stringViewAVX()
	return v, err
}

// StringViewAVX2 is Stream.StringView over the fused AVX2 scan.
func (s *Stream) StringViewAVX2() (string, error) {
	v, _, err := s.stringViewAVX2()
	return v, err
}

// StringViewAVX512 is Stream.StringView over the fused AVX-512 scan.
func (s *Stream) StringViewAVX512() (string, error) {
	v, _, err := s.stringViewAVX512()
	return v, err
}

// KeyViewAVX is Stream.KeyView over the fused AVX scan (no scalar prelude —
// the fused pass replaces it; alias contract identical).
func (s *Stream) KeyViewAVX() (string, error) {
	v, _, err := s.stringViewAVX()
	return v, err
}

// KeyViewAVX2 is Stream.KeyView over the fused AVX2 scan.
func (s *Stream) KeyViewAVX2() (string, error) {
	v, _, err := s.stringViewAVX2()
	return v, err
}

// KeyViewAVX512 is Stream.KeyView over the fused AVX-512 scan.
func (s *Stream) KeyViewAVX512() (string, error) {
	v, _, err := s.stringViewAVX512()
	return v, err
}
