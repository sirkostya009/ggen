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

// structuralIndexAVX returns the index, relative to j, of the first '"',
// '\\' or control byte (< 0x20) in b[j:], or -1. It scans 16 bytes per step.
// The last partial vector reloads the last 16 bytes of b and shifts out the
// lanes before the cursor, so it never reads past len(b). Only a b shorter
// than one vector is checked one byte at a time.
func structuralIndexAVX(b []byte, j int) int {
	quote := archsimd.LoadUint8x16(VecQuote[:])
	bslash := archsimd.LoadUint8x16(VecBackslash[:])
	ctrl := archsimd.LoadUint8x16(VecCtrl[:])
	i := j
	for ; i+16 <= len(b); i += 16 {
		v := archsimd.LoadUint8x16(b[i:])
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			return i - j + bits.TrailingZeros16(m)
		}
	}
	if i < len(b) && len(b) >= 16 {
		v := archsimd.LoadUint8x16(b[len(b)-16:])
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits() >> uint(16-(len(b)-i))
		if m != 0 {
			return i - j + bits.TrailingZeros16(m)
		}
		return -1
	}
	for ; i < len(b); i++ {
		if c := b[i]; c == '"' || c == '\\' || c < 0x20 {
			return i - j
		}
	}
	return -1
}

// structuralIndexAVX2 is structuralIndexAVX at 32 bytes per step.
func structuralIndexAVX2(b []byte, j int) int {
	quote := archsimd.BroadcastUint8x32('"')
	bslash := archsimd.BroadcastUint8x32('\\')
	ctrl := archsimd.BroadcastUint8x32(0x1F)
	i := j
	for ; i+32 <= len(b); i += 32 {
		v := archsimd.LoadUint8x32(b[i:])
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			return i - j + bits.TrailingZeros32(m)
		}
	}
	if i < len(b) && len(b) >= 32 {
		v := archsimd.LoadUint8x32(b[len(b)-32:])
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits() >> uint(32-(len(b)-i))
		if m != 0 {
			return i - j + bits.TrailingZeros32(m)
		}
		return -1
	}
	for ; i < len(b); i++ {
		if c := b[i]; c == '"' || c == '\\' || c < 0x20 {
			return i - j
		}
	}
	return -1
}

// structuralIndexAVX512 is structuralIndexAVX at 64 bytes per step.
func structuralIndexAVX512(b []byte, j int) int {
	quote := archsimd.BroadcastUint8x64('"')
	bslash := archsimd.BroadcastUint8x64('\\')
	space := archsimd.BroadcastUint8x64(0x20)
	i := j
	for ; i+64 <= len(b); i += 64 {
		v := archsimd.LoadUint8x64(b[i:])
		m := v.Equal(quote).ToBits() | v.Equal(bslash).ToBits() | v.Less(space).ToBits()
		if m != 0 {
			return i - j + bits.TrailingZeros64(m)
		}
	}
	if i < len(b) && len(b) >= 64 {
		v := archsimd.LoadUint8x64(b[len(b)-64:])
		m := (v.Equal(quote).ToBits() | v.Equal(bslash).ToBits() | v.Less(space).ToBits()) >> uint(64-(len(b)-i))
		if m != 0 {
			return i - j + bits.TrailingZeros64(m)
		}
		return -1
	}
	for ; i < len(b); i++ {
		if c := b[i]; c == '"' || c == '\\' || c < 0x20 {
			return i - j
		}
	}
	return -1
}

// structuralIndexHighAVX is structuralIndexAVX that also reports whether any
// byte before the hit was >= 0x80. The string cores use that to skip UTF-8
// validation on pure ASCII text. In the vector that holds the hit only the
// lanes below it count, since the bytes after a closing quote belong to the
// next token. It is separate from structuralIndexAVX so the skip tiers do not
// pay for the extra OR.
func structuralIndexHighAVX(b []byte, j int) (int, bool) {
	quote := archsimd.LoadUint8x16(VecQuote[:])
	bslash := archsimd.LoadUint8x16(VecBackslash[:])
	ctrl := archsimd.LoadUint8x16(VecCtrl[:])
	ascii := archsimd.LoadUint8x16(vec7F[:])
	acc := archsimd.Uint8x16{}
	i := j
	for ; i+16 <= len(b); i += 16 {
		v := archsimd.LoadUint8x16(b[i:])
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			return i - j + bits.TrailingZeros16(m), acc.Min(ascii).Equal(acc).ToBits() != 0xFFFF ||
				^v.Min(ascii).Equal(v).ToBits()&(m-1)&^m != 0
		}
		acc = acc.Or(v)
	}
	hi := acc.Min(ascii).Equal(acc).ToBits() != 0xFFFF
	if i < len(b) && len(b) >= 16 {
		v := archsimd.LoadUint8x16(b[len(b)-16:])
		d := uint(16 - (len(b) - i))
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits() >> d
		h := ^v.Min(ascii).Equal(v).ToBits() >> d
		if m != 0 {
			return i - j + bits.TrailingZeros16(m), hi || h&(m-1)&^m != 0
		}
		return -1, hi || h != 0
	}
	var t byte
	for ; i < len(b); i++ {
		c := b[i]
		if c == '"' || c == '\\' || c < 0x20 {
			return i - j, hi || t&0x80 != 0
		}
		t |= c
	}
	return -1, hi || t&0x80 != 0
}

// structuralIndexHighAVX2 is structuralIndexHighAVX at 32 bytes per step.
func structuralIndexHighAVX2(b []byte, j int) (int, bool) {
	quote := archsimd.BroadcastUint8x32('"')
	bslash := archsimd.BroadcastUint8x32('\\')
	ctrl := archsimd.BroadcastUint8x32(0x1F)
	ascii := archsimd.BroadcastUint8x32(0x7F)
	acc := archsimd.BroadcastUint8x32(0)
	i := j
	for ; i+32 <= len(b); i += 32 {
		v := archsimd.LoadUint8x32(b[i:])
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits()
		if m != 0 {
			return i - j + bits.TrailingZeros32(m), acc.Min(ascii).Equal(acc).ToBits() != 0xFFFFFFFF ||
				^v.Min(ascii).Equal(v).ToBits()&(m-1)&^m != 0
		}
		acc = acc.Or(v)
	}
	hi := acc.Min(ascii).Equal(acc).ToBits() != 0xFFFFFFFF
	if i < len(b) && len(b) >= 32 {
		v := archsimd.LoadUint8x32(b[len(b)-32:])
		d := uint(32 - (len(b) - i))
		m := v.Equal(quote).Or(v.Equal(bslash)).Or(v.Min(ctrl).Equal(v)).ToBits() >> d
		h := ^v.Min(ascii).Equal(v).ToBits() >> d
		if m != 0 {
			return i - j + bits.TrailingZeros32(m), hi || h&(m-1)&^m != 0
		}
		return -1, hi || h != 0
	}
	var t byte
	for ; i < len(b); i++ {
		c := b[i]
		if c == '"' || c == '\\' || c < 0x20 {
			return i - j, hi || t&0x80 != 0
		}
		t |= c
	}
	return -1, hi || t&0x80 != 0
}

// structuralIndexHighAVX512 is structuralIndexHighAVX at 64 bytes per step.
func structuralIndexHighAVX512(b []byte, j int) (int, bool) {
	quote := archsimd.BroadcastUint8x64('"')
	bslash := archsimd.BroadcastUint8x64('\\')
	space := archsimd.BroadcastUint8x64(0x20)
	high := archsimd.BroadcastUint8x64(0x80)
	acc := archsimd.BroadcastUint8x64(0)
	i := j
	for ; i+64 <= len(b); i += 64 {
		v := archsimd.LoadUint8x64(b[i:])
		m := v.Equal(quote).ToBits() | v.Equal(bslash).ToBits() | v.Less(space).ToBits()
		if m != 0 {
			return i - j + bits.TrailingZeros64(m), acc.Less(high).ToBits() != ^uint64(0) ||
				^v.Less(high).ToBits()&(m-1)&^m != 0
		}
		acc = acc.Or(v)
	}
	hi := acc.Less(high).ToBits() != ^uint64(0)
	if i < len(b) && len(b) >= 64 {
		v := archsimd.LoadUint8x64(b[len(b)-64:])
		d := uint(64 - (len(b) - i))
		m := (v.Equal(quote).ToBits() | v.Equal(bslash).ToBits() | v.Less(space).ToBits()) >> d
		h := ^v.Less(high).ToBits() >> d
		if m != 0 {
			return i - j + bits.TrailingZeros64(m), hi || h&(m-1)&^m != 0
		}
		return -1, hi || h != 0
	}
	var t byte
	for ; i < len(b); i++ {
		c := b[i]
		if c == '"' || c == '\\' || c < 0x20 {
			return i - j, hi || t&0x80 != 0
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
		k, hi := structuralIndexHighAVX(s.buf, j)
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
		k, hi := structuralIndexHighAVX2(s.buf, j)
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
		k, hi := structuralIndexHighAVX512(s.buf, j)
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
