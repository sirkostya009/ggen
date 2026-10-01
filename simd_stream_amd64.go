//go:build goexperiment.simd

// Vector string scans for the Stream path. SIMD (vector) code processes 16,
// 32 or 64 bytes with one CPU instruction. AVX, AVX2 and AVX-512 are the x86
// vector instruction sets for those widths. A lane is one byte slot inside a
// vector. A mask is one bit per lane, set where a test matched.
//
// A Stream reads its input in pieces. The window is the part of the reader
// currently held in the Stream's buffer, and a refill reads more into it. A
// string may span several refills, so the bytes-path scanners, which need
// the whole string in one slice, do not apply. Each window is still one
// contiguous slice. So these cores keep stringView's refill and compaction
// loop exactly as it is and change only how a window is searched. The scalar
// loop uses IndexByte twice plus a control-byte check. These use one vector
// pass per window.
//
// Each tier has a structuralIndex* function, a stringView* core, and thin
// String*, StringView* and KeyView* wrappers with the same contracts as the
// scalar three. structuralIndex* finds the first '"', '\\' or control byte,
// or returns -1 when the window has none. String returns an owned copy.
// StringView and KeyView return a string that aliases the buffer, meaning it
// points into it with no copy. Generated stream decoders call one tier
// directly when ggen runs with -simd. Nothing probes the CPU at run time.
// KeyView* has no scalar short-key check up front, because the vector pass
// does that work. ggen keeps the plain KeyView for structs whose keys are
// all short, where that check is faster. The bytes path uses the same rule.

package ggen

import (
	"math/bits"
	"strings"
	"unsafe"

	"simd/archsimd"
)

// structuralIndexAVX returns the index of the first '"', '\\' or control
// byte (< 0x20) in b, or -1. It scans 16 bytes per step and checks the last
// partial block one byte at a time.
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

// structuralIndexAVX2 is structuralIndexAVX at 32 bytes per step.
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

// structuralIndexAVX512 is structuralIndexAVX at 64 bytes per step.
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

// structuralIndexHighAVX is structuralIndexAVX that also reports whether any
// scanned byte was >= 0x80. The string cores use that to skip UTF-8
// validation on pure ASCII text. The flag covers the whole vector that held
// the hit, so it can be set by bytes after the hit. The caller validates the
// exact span, which settles it. It is separate from structuralIndexAVX so the
// skip tiers do not pay for the extra OR.
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

// structuralIndexHighAVX2 is structuralIndexHighAVX at 32 bytes per step.
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

// structuralIndexHighAVX512 is structuralIndexHighAVX at 64 bytes per step.
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

// stringViewAVX is (*Stream).stringView with the AVX window scan. The refill
// and compaction bookkeeping and the error positions match the scalar loop
// exactly. Only the window scan differs. There are three near-identical
// copies, one per tier, instead of one core taking a function value, because
// the tier scan must be a direct call.
func (s *Stream) stringViewAVX(validate bool) (v string, owned bool, err error) {
	i := s.Pos
	if i >= len(s.buf) {
		err := s.ReadMore(i)
		i = 0
		if err != nil {
			s.Pos = i
			return "", false, NotEOF(err, ErrExpectString)
		}
	}
	if s.buf[i] != '"' {
		s.Pos = i
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
				s.Pos = len(s.buf)
				return "", false, NotEOF(err, ErrUnterminated)
			}
			continue
		}
		switch s.buf[j+k] {
		case '"':
			end := j + k
			// Validate the full span. A multi-byte rune may cross the window
			// cursor j, so validating each window alone would report false
			// errors. See the scalar stringView.
			if validate && sawHigh && !validUTF8x16(s.buf[start:end]) {
				s.Pos = start
				return "", false, ErrInvalidUTF8
			}
			s.Pos = end + 1
			return unsafe.String(unsafe.SliceData(s.buf[start:]), end-start), false, nil
		case '\\':
			v, err := s.stringSlow(start, j+k, validate)
			return v, true, err
		default:
			// The scan stopped on the control byte itself, the same
			// position the scalar stringView reports.
			s.Pos = j + k
			return "", false, ErrBadString
		}
	}
}

// stringViewAVX2 is stringViewAVX with the AVX2 window scan.
func (s *Stream) stringViewAVX2(validate bool) (v string, owned bool, err error) {
	i := s.Pos
	if i >= len(s.buf) {
		err := s.ReadMore(i)
		i = 0
		if err != nil {
			s.Pos = i
			return "", false, NotEOF(err, ErrExpectString)
		}
	}
	if s.buf[i] != '"' {
		s.Pos = i
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
				s.Pos = len(s.buf)
				return "", false, NotEOF(err, ErrUnterminated)
			}
			continue
		}
		switch s.buf[j+k] {
		case '"':
			end := j + k
			if validate && sawHigh && !validUTF8x16(s.buf[start:end]) {
				s.Pos = start
				return "", false, ErrInvalidUTF8
			}
			s.Pos = end + 1
			return unsafe.String(unsafe.SliceData(s.buf[start:]), end-start), false, nil
		case '\\':
			v, err := s.stringSlow(start, j+k, validate)
			return v, true, err
		default:
			// The scan stopped on the control byte itself, the same
			// position the scalar stringView reports.
			s.Pos = j + k
			return "", false, ErrBadString
		}
	}
}

// stringViewAVX512 is stringViewAVX with the AVX-512 window scan.
func (s *Stream) stringViewAVX512(validate bool) (v string, owned bool, err error) {
	i := s.Pos
	if i >= len(s.buf) {
		err := s.ReadMore(i)
		i = 0
		if err != nil {
			s.Pos = i
			return "", false, NotEOF(err, ErrExpectString)
		}
	}
	if s.buf[i] != '"' {
		s.Pos = i
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
				s.Pos = len(s.buf)
				return "", false, NotEOF(err, ErrUnterminated)
			}
			continue
		}
		switch s.buf[j+k] {
		case '"':
			end := j + k
			// The 64-byte validator is safe here, because this core is only
			// used by the avx512 tier. The width is picked here so short spans
			// skip an extra call frame. See classifyStructural64.
			if validate && sawHigh {
				span := s.buf[start:end]
				var ok bool
				if len(span) < utf8x64MinLen {
					ok = validUTF8x16(span)
				} else {
					ok = validUTF8x64(span)
				}
				if !ok {
					s.Pos = start
					return "", false, ErrInvalidUTF8
				}
			}
			s.Pos = end + 1
			return unsafe.String(unsafe.SliceData(s.buf[start:]), end-start), false, nil
		case '\\':
			v, err := s.stringSlow(start, j+k, validate)
			return v, true, err
		default:
			// The scan stopped on the control byte itself, the same
			// position the scalar stringView reports.
			s.Pos = j + k
			return "", false, ErrBadString
		}
	}
}

// StringAVX is Stream.String using the AVX scan.
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

// StringAVX2 is Stream.String using the AVX2 scan.
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

// StringAVX512 is Stream.String using the AVX-512 scan.
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

// StringViewAVX is Stream.StringView using the AVX scan.
func (s *Stream) StringViewAVX(validate bool) (string, error) {
	v, _, err := s.stringViewAVX(validate)
	return v, err
}

// StringViewAVX2 is Stream.StringView using the AVX2 scan.
func (s *Stream) StringViewAVX2(validate bool) (string, error) {
	v, _, err := s.stringViewAVX2(validate)
	return v, err
}

// StringViewAVX512 is Stream.StringView using the AVX-512 scan.
func (s *Stream) StringViewAVX512(validate bool) (string, error) {
	v, _, err := s.stringViewAVX512(validate)
	return v, err
}

// KeyViewAVX is Stream.KeyView using the AVX scan. It has no scalar
// short-key check up front, since the vector pass replaces it. The returned
// string aliases the buffer, the same as KeyView.
func (s *Stream) KeyViewAVX(validate bool) (string, error) {
	v, _, err := s.stringViewAVX(validate)
	return v, err
}

// KeyViewAVX2 is Stream.KeyView using the AVX2 scan.
func (s *Stream) KeyViewAVX2(validate bool) (string, error) {
	v, _, err := s.stringViewAVX2(validate)
	return v, err
}

// KeyViewAVX512 is Stream.KeyView using the AVX-512 scan.
func (s *Stream) KeyViewAVX512(validate bool) (string, error) {
	v, _, err := s.stringViewAVX512(validate)
	return v, err
}
