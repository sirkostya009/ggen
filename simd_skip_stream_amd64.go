//go:build goexperiment.simd

// Vector skip functions for the Stream path. They step over a JSON value
// without decoding it. SIMD (vector) code processes 16, 32 or 64 bytes with
// one CPU instruction. AVX, AVX2 and AVX-512 are the x86 vector instruction
// sets for those widths. A lane is one byte slot inside a vector. A mask is
// one bit per lane, set where a test matched.
//
// A Stream holds a window, the part of the reader currently in its buffer,
// and a refill reads more into it. These functions follow the same rules as
// the stream string tiers. The refill and compaction bookkeeping is copied
// exactly from the scalar functions. Only the search inside each window
// changes. skipString* finds the next quote, backslash or control byte with
// structuralIndex*. The slow path of SkipSpace* tests whole vectors of
// whitespace instead of one byte at a time, which helps pretty-printed
// input. Each tier is a separate copy that calls its tier functions
// directly, with no function values. Generated stream decoders replace
// `= s.SkipValue()` and `= s.SkipSpace()` with a tier at generate time.

package ggen

import (
	"io"
	"math/bits"

	"simd/archsimd"
)

// SkipSpaceAVX is Stream.SkipSpace that skips whitespace runs 16 bytes at a
// time.
func (s *Stream) SkipSpaceAVX() error {
	if s.Pos < len(s.buf) && s.buf[s.Pos] > ' ' {
		return nil
	}
	return s.skipSpaceSlowAVX()
}

func (s *Stream) skipSpaceSlowAVX() error {
	sp := archsimd.BroadcastUint8x16(' ')
	tb := archsimd.BroadcastUint8x16('\t')
	nl := archsimd.BroadcastUint8x16('\n')
	cr := archsimd.BroadcastUint8x16('\r')
	i := s.Pos
	buf := s.buf
	for {
		for ; i+16 <= len(buf); i += 16 {
			v := archsimd.LoadUint8x16(buf[i:])
			m := v.Equal(sp).Or(v.Equal(tb)).Or(v.Equal(nl)).Or(v.Equal(cr)).ToBits()
			if m != 0xFFFF {
				s.Pos = i + bits.TrailingZeros16(^m)
				return nil
			}
		}
		for i < len(buf) {
			c := buf[i]
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				s.Pos = i
				return nil
			}
			i++
		}
		if err := s.ReadMore(i); err != nil {
			s.Pos = 0
			if err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		i = 0
		buf = s.buf
	}
}

// SkipSpaceAVX2 is Stream.SkipSpace that skips whitespace runs 32 bytes at a
// time.
func (s *Stream) SkipSpaceAVX2() error {
	if s.Pos < len(s.buf) && s.buf[s.Pos] > ' ' {
		return nil
	}
	return s.skipSpaceSlowAVX2()
}

func (s *Stream) skipSpaceSlowAVX2() error {
	sp := archsimd.BroadcastUint8x32(' ')
	tb := archsimd.BroadcastUint8x32('\t')
	nl := archsimd.BroadcastUint8x32('\n')
	cr := archsimd.BroadcastUint8x32('\r')
	i := s.Pos
	buf := s.buf
	for {
		for ; i+32 <= len(buf); i += 32 {
			v := archsimd.LoadUint8x32(buf[i:])
			m := v.Equal(sp).Or(v.Equal(tb)).Or(v.Equal(nl)).Or(v.Equal(cr)).ToBits()
			if m != 0xFFFFFFFF {
				s.Pos = i + bits.TrailingZeros32(^m)
				return nil
			}
		}
		for i < len(buf) {
			c := buf[i]
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				s.Pos = i
				return nil
			}
			i++
		}
		if err := s.ReadMore(i); err != nil {
			s.Pos = 0
			if err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		i = 0
		buf = s.buf
	}
}

// SkipSpaceAVX512 is Stream.SkipSpace that skips whitespace runs 64 bytes at
// a time.
func (s *Stream) SkipSpaceAVX512() error {
	if s.Pos < len(s.buf) && s.buf[s.Pos] > ' ' {
		return nil
	}
	return s.skipSpaceSlowAVX512()
}

func (s *Stream) skipSpaceSlowAVX512() error {
	sp := archsimd.BroadcastUint8x64(' ')
	tb := archsimd.BroadcastUint8x64('\t')
	nl := archsimd.BroadcastUint8x64('\n')
	cr := archsimd.BroadcastUint8x64('\r')
	i := s.Pos
	buf := s.buf
	for {
		for ; i+64 <= len(buf); i += 64 {
			v := archsimd.LoadUint8x64(buf[i:])
			m := v.Equal(sp).ToBits() | v.Equal(tb).ToBits() | v.Equal(nl).ToBits() | v.Equal(cr).ToBits()
			if m != ^uint64(0) {
				s.Pos = i + bits.TrailingZeros64(^m)
				return nil
			}
		}
		for i < len(buf) {
			c := buf[i]
			if c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				s.Pos = i
				return nil
			}
			i++
		}
		if err := s.ReadMore(i); err != nil {
			s.Pos = 0
			if err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		i = 0
		buf = s.buf
	}
}

// skipStringStreamTail validates the escape at s.buf[bs] while skipping a
// string, refilling as needed. Every stream skipString tier shares it for the
// rare escape case. It behaves like the scalar escape branch, including
// moving the cursors when a refill shifts the buffer. It returns the updated
// j and start cursors.
func (s *Stream) skipStringStreamTail(start, bs int) (int, int, error) {
	if bs+1 >= len(s.buf) {
		if err := s.ReadMore(bs); err != nil {
			// ReadMore moved the buffer so it starts at bs. A cut-short escape
			// reports the end of what arrived, like len(data) on the bytes path.
			s.Pos = len(s.buf)
			return 0, 0, NotEOF(err, ErrBadString)
		}
		start = 0
		bs = 0
	}
	switch s.buf[bs+1] {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		return bs + 2, start, nil
	case 'u':
		// Refill only while the tail can still become \uXXXX. See the
		// scalar version.
		for bs+6 > len(s.buf) {
			if !uEscapePrefix(s.buf[bs:]) {
				s.Pos = bs
				return 0, 0, ErrBadString
			}
			if err := s.ReadMore(bs); err != nil {
				s.Pos = len(s.buf)
				return 0, 0, NotEOF(err, ErrBadString)
			}
			start = 0
			bs = 0
		}
		if _, ok := parseHex4(s.buf[bs+2 : bs+6]); !ok {
			s.Pos = bs
			return 0, 0, ErrBadString
		}
		return bs + 6, start, nil
	default:
		s.Pos = bs
		return 0, 0, ErrBadString
	}
}

// skipStringAVX is Stream.skipString with the AVX window scan.
func (s *Stream) skipStringAVX() error {
	i := s.Pos
	if i >= len(s.buf) {
		err := s.ReadMore(i)
		i = 0
		if err != nil {
			s.Pos = i
			return NotEOF(err, ErrExpectString)
		}
	}
	if s.buf[i] != '"' {
		s.Pos = i
		return ErrExpectString
	}
	start := i + 1
	j := start
	for {
		k := structuralIndexAVX(s.buf[j:])
		if k < 0 {
			// Skipped bytes are not needed again, so drop the whole window
			// on refill. See the scalar version.
			j = len(s.buf)
			err := s.ReadMore(j)
			j = 0
			start = 0
			if err != nil {
				s.Pos = j
				return NotEOF(err, ErrUnterminated)
			}
			continue
		}
		switch s.buf[j+k] {
		case '"':
			s.Pos = j + k + 1
			return nil
		case '\\':
			nj, nstart, err := s.skipStringStreamTail(start, j+k)
			if err != nil {
				return err
			}
			j, start = nj, nstart
		default:
			// The scan stopped on the control byte itself, the position the
			// scalar skipString reports.
			s.Pos = j + k
			return ErrBadString
		}
	}
}

// skipStringAVX2 is skipStringAVX with the AVX2 window scan.
func (s *Stream) skipStringAVX2() error {
	i := s.Pos
	if i >= len(s.buf) {
		err := s.ReadMore(i)
		i = 0
		if err != nil {
			s.Pos = i
			return NotEOF(err, ErrExpectString)
		}
	}
	if s.buf[i] != '"' {
		s.Pos = i
		return ErrExpectString
	}
	start := i + 1
	j := start
	for {
		k := structuralIndexAVX2(s.buf[j:])
		if k < 0 {
			// Skipped bytes are not needed again, so drop the whole window
			// on refill. See the scalar version.
			j = len(s.buf)
			err := s.ReadMore(j)
			j = 0
			start = 0
			if err != nil {
				s.Pos = j
				return NotEOF(err, ErrUnterminated)
			}
			continue
		}
		switch s.buf[j+k] {
		case '"':
			s.Pos = j + k + 1
			return nil
		case '\\':
			nj, nstart, err := s.skipStringStreamTail(start, j+k)
			if err != nil {
				return err
			}
			j, start = nj, nstart
		default:
			// The scan stopped on the control byte itself, the position the
			// scalar skipString reports.
			s.Pos = j + k
			return ErrBadString
		}
	}
}

// skipStringAVX512 is skipStringAVX with the AVX-512 window scan.
func (s *Stream) skipStringAVX512() error {
	i := s.Pos
	if i >= len(s.buf) {
		err := s.ReadMore(i)
		i = 0
		if err != nil {
			s.Pos = i
			return NotEOF(err, ErrExpectString)
		}
	}
	if s.buf[i] != '"' {
		s.Pos = i
		return ErrExpectString
	}
	start := i + 1
	j := start
	for {
		k := structuralIndexAVX512(s.buf[j:])
		if k < 0 {
			// Skipped bytes are not needed again, so drop the whole window
			// on refill. See the scalar version.
			j = len(s.buf)
			err := s.ReadMore(j)
			j = 0
			start = 0
			if err != nil {
				s.Pos = j
				return NotEOF(err, ErrUnterminated)
			}
			continue
		}
		switch s.buf[j+k] {
		case '"':
			s.Pos = j + k + 1
			return nil
		case '\\':
			nj, nstart, err := s.skipStringStreamTail(start, j+k)
			if err != nil {
				return err
			}
			j, start = nj, nstart
		default:
			// The scan stopped on the control byte itself, the position the
			// scalar skipString reports.
			s.Pos = j + k
			return ErrBadString
		}
	}
}

// SkipValueAVX is Stream.SkipValue using the AVX skip functions.
func (s *Stream) SkipValueAVX() error {
	return s.skipValueAVX(0)
}

func (s *Stream) skipValueAVX(depth int) error {
	if err := s.SkipSpaceAVX(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return NotEOF(err, ErrUnexpectedEnd)
		}
		s.Pos = 0
	}
	switch s.buf[s.Pos] {
	case '"':
		return s.skipStringAVX()
	case 't':
		return s.skipLiteral("true", ErrBadBool)
	case 'f':
		return s.skipLiteral("false", ErrBadBool)
	case 'n':
		return s.skipLiteral("null", ErrBadLiteral)
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return s.skipNumber()
	case '[':
		s.Pos++
		return s.skipArrayAVX(depth + 1)
	case '{':
		s.Pos++
		return s.skipObjectAVX(depth + 1)
	}
	return ErrBadValue
}

func (s *Stream) skipArrayAVX(depth int) error {
	if depth > maxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpaceAVX(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		// Input that ends right after '[' reports ErrUnexpectedEnd, not
		// ErrBadArray. That matches the bytes skipArray, where the element
		// skipValue reports it.
		if err := s.ReadMore(s.Pos); err != nil {
			return NotEOF(err, ErrUnexpectedEnd)
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == ']' {
		s.Pos++
		return nil
	}
	for {
		if err := s.skipValueAVX(depth); err != nil {
			return err
		}
		if err := s.SkipSpaceAVX(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return NotEOF(err, ErrBadArray)
			}
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			continue
		}
		if s.buf[s.Pos] == ']' {
			s.Pos++
			return nil
		}
		return ErrBadArray
	}
}

func (s *Stream) skipObjectAVX(depth int) error {
	if depth > maxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpaceAVX(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return NotEOF(err, ErrBadObject)
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == '}' {
		s.Pos++
		return nil
	}
	for {
		if err := s.skipStringAVX(); err != nil {
			// skipString reports ErrExpectString only when the key does not
			// start with '"'. The bytes loop reports ErrBadObject there. See
			// Stream.skipObject.
			if err == ErrExpectString {
				return ErrBadObject
			}
			return err
		}
		if err := s.SkipSpaceAVX(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return NotEOF(err, ErrBadObject)
			}
		}
		if s.buf[s.Pos] != ':' {
			return ErrBadObject
		}
		s.Pos++
		if err := s.skipValueAVX(depth); err != nil {
			return err
		}
		if err := s.SkipSpaceAVX(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return NotEOF(err, ErrBadObject)
			}
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			// Skip whitespace between ',' and the next key, as the scalar
			// skipObject does.
			if err := s.SkipSpaceAVX(); err != nil {
				return err
			}
			continue
		}
		if s.buf[s.Pos] == '}' {
			s.Pos++
			return nil
		}
		return ErrBadObject
	}
}

// SkipValueAVX2 is Stream.SkipValue using the AVX2 skip functions.
func (s *Stream) SkipValueAVX2() error {
	return s.skipValueAVX2(0)
}

func (s *Stream) skipValueAVX2(depth int) error {
	if err := s.SkipSpaceAVX2(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return NotEOF(err, ErrUnexpectedEnd)
		}
		s.Pos = 0
	}
	switch s.buf[s.Pos] {
	case '"':
		return s.skipStringAVX2()
	case 't':
		return s.skipLiteral("true", ErrBadBool)
	case 'f':
		return s.skipLiteral("false", ErrBadBool)
	case 'n':
		return s.skipLiteral("null", ErrBadLiteral)
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return s.skipNumber()
	case '[':
		s.Pos++
		return s.skipArrayAVX2(depth + 1)
	case '{':
		s.Pos++
		return s.skipObjectAVX2(depth + 1)
	}
	return ErrBadValue
}

func (s *Stream) skipArrayAVX2(depth int) error {
	if depth > maxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpaceAVX2(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		// Input that ends right after '[' reports ErrUnexpectedEnd, not
		// ErrBadArray. That matches the bytes skipArray, where the element
		// skipValue reports it.
		if err := s.ReadMore(s.Pos); err != nil {
			return NotEOF(err, ErrUnexpectedEnd)
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == ']' {
		s.Pos++
		return nil
	}
	for {
		if err := s.skipValueAVX2(depth); err != nil {
			return err
		}
		if err := s.SkipSpaceAVX2(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return NotEOF(err, ErrBadArray)
			}
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			continue
		}
		if s.buf[s.Pos] == ']' {
			s.Pos++
			return nil
		}
		return ErrBadArray
	}
}

func (s *Stream) skipObjectAVX2(depth int) error {
	if depth > maxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpaceAVX2(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return NotEOF(err, ErrBadObject)
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == '}' {
		s.Pos++
		return nil
	}
	for {
		if err := s.skipStringAVX2(); err != nil {
			// skipString reports ErrExpectString only when the key does not
			// start with '"'. The bytes loop reports ErrBadObject there. See
			// Stream.skipObject.
			if err == ErrExpectString {
				return ErrBadObject
			}
			return err
		}
		if err := s.SkipSpaceAVX2(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return NotEOF(err, ErrBadObject)
			}
		}
		if s.buf[s.Pos] != ':' {
			return ErrBadObject
		}
		s.Pos++
		if err := s.skipValueAVX2(depth); err != nil {
			return err
		}
		if err := s.SkipSpaceAVX2(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return NotEOF(err, ErrBadObject)
			}
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			// Skip whitespace between ',' and the next key, as the scalar
			// skipObject does.
			if err := s.SkipSpaceAVX2(); err != nil {
				return err
			}
			continue
		}
		if s.buf[s.Pos] == '}' {
			s.Pos++
			return nil
		}
		return ErrBadObject
	}
}

// SkipValueAVX512 is Stream.SkipValue using the AVX-512 skip functions.
func (s *Stream) SkipValueAVX512() error {
	return s.skipValueAVX512(0)
}

func (s *Stream) skipValueAVX512(depth int) error {
	if err := s.SkipSpaceAVX512(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return NotEOF(err, ErrUnexpectedEnd)
		}
		s.Pos = 0
	}
	switch s.buf[s.Pos] {
	case '"':
		return s.skipStringAVX512()
	case 't':
		return s.skipLiteral("true", ErrBadBool)
	case 'f':
		return s.skipLiteral("false", ErrBadBool)
	case 'n':
		return s.skipLiteral("null", ErrBadLiteral)
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return s.skipNumber()
	case '[':
		s.Pos++
		return s.skipArrayAVX512(depth + 1)
	case '{':
		s.Pos++
		return s.skipObjectAVX512(depth + 1)
	}
	return ErrBadValue
}

// CaptureValueAVX is Stream.CaptureValue using the bytes-path AVX skip.
// CaptureValueAVX2 and CaptureValueAVX512 do the same for their tiers. The
// vector skip finds where the value ends. Capturing is still just growing
// the buffer until the value fits and slicing it. See Stream.CaptureValue.
func (s *Stream) CaptureValueAVX() ([]byte, error) {
	start := s.Pos
	eof := false
	for {
		end, err := SkipValueAVX(s.buf, start)
		if err == nil {
			if end < len(s.buf) || eof {
				s.Pos = end
				return s.buf[start:end], nil
			}
			if c := s.buf[SkipSpace(s.buf, start)]; c != '-' && (c < '0' || c > '9') {
				s.Pos = end
				return s.buf[start:end], nil
			}
		}
		if eof {
			s.Pos = end
			return nil, err
		}
		// A failure at a byte inside the window, not at its end, is final.
		// See Stream.CaptureValue. Without this, a live reader that sent a
		// complete but malformed value would block in Read forever. The
		// tier skip reports the same failure position as the scalar skip,
		// which a parity test checks. ErrMaxDepth is final wherever it
		// happens.
		if err != nil && (end < len(s.buf) || err == ErrMaxDepth) {
			s.Pos = end
			return nil, err
		}
		if e := s.ReadMore(start); e != nil {
			if e != io.ErrUnexpectedEOF {
				s.Pos = 0
				return nil, e
			}
			eof = true
		}
		start = 0
	}
}

func (s *Stream) CaptureValueAVX2() ([]byte, error) {
	start := s.Pos
	eof := false
	for {
		end, err := SkipValueAVX2(s.buf, start)
		if err == nil {
			if end < len(s.buf) || eof {
				s.Pos = end
				return s.buf[start:end], nil
			}
			if c := s.buf[SkipSpace(s.buf, start)]; c != '-' && (c < '0' || c > '9') {
				s.Pos = end
				return s.buf[start:end], nil
			}
		}
		if eof {
			s.Pos = end
			return nil, err
		}
		// A failure at a byte inside the window, not at its end, is final.
		// See Stream.CaptureValue. Without this, a live reader that sent a
		// complete but malformed value would block in Read forever. The
		// tier skip reports the same failure position as the scalar skip,
		// which a parity test checks. ErrMaxDepth is final wherever it
		// happens.
		if err != nil && (end < len(s.buf) || err == ErrMaxDepth) {
			s.Pos = end
			return nil, err
		}
		if e := s.ReadMore(start); e != nil {
			if e != io.ErrUnexpectedEOF {
				s.Pos = 0
				return nil, e
			}
			eof = true
		}
		start = 0
	}
}

func (s *Stream) CaptureValueAVX512() ([]byte, error) {
	start := s.Pos
	eof := false
	for {
		end, err := SkipValueAVX512(s.buf, start)
		if err == nil {
			if end < len(s.buf) || eof {
				s.Pos = end
				return s.buf[start:end], nil
			}
			if c := s.buf[SkipSpace(s.buf, start)]; c != '-' && (c < '0' || c > '9') {
				s.Pos = end
				return s.buf[start:end], nil
			}
		}
		if eof {
			s.Pos = end
			return nil, err
		}
		// A failure at a byte inside the window, not at its end, is final.
		// See Stream.CaptureValue. Without this, a live reader that sent a
		// complete but malformed value would block in Read forever. The
		// tier skip reports the same failure position as the scalar skip,
		// which a parity test checks. ErrMaxDepth is final wherever it
		// happens.
		if err != nil && (end < len(s.buf) || err == ErrMaxDepth) {
			s.Pos = end
			return nil, err
		}
		if e := s.ReadMore(start); e != nil {
			if e != io.ErrUnexpectedEOF {
				s.Pos = 0
				return nil, e
			}
			eof = true
		}
		start = 0
	}
}

func (s *Stream) skipArrayAVX512(depth int) error {
	if depth > maxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpaceAVX512(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		// Input that ends right after '[' reports ErrUnexpectedEnd, not
		// ErrBadArray. That matches the bytes skipArray, where the element
		// skipValue reports it.
		if err := s.ReadMore(s.Pos); err != nil {
			return NotEOF(err, ErrUnexpectedEnd)
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == ']' {
		s.Pos++
		return nil
	}
	for {
		if err := s.skipValueAVX512(depth); err != nil {
			return err
		}
		if err := s.SkipSpaceAVX512(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return NotEOF(err, ErrBadArray)
			}
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			continue
		}
		if s.buf[s.Pos] == ']' {
			s.Pos++
			return nil
		}
		return ErrBadArray
	}
}

func (s *Stream) skipObjectAVX512(depth int) error {
	if depth > maxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpaceAVX512(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return NotEOF(err, ErrBadObject)
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == '}' {
		s.Pos++
		return nil
	}
	for {
		if err := s.skipStringAVX512(); err != nil {
			// skipString reports ErrExpectString only when the key does not
			// start with '"'. The bytes loop reports ErrBadObject there. See
			// Stream.skipObject.
			if err == ErrExpectString {
				return ErrBadObject
			}
			return err
		}
		if err := s.SkipSpaceAVX512(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return NotEOF(err, ErrBadObject)
			}
		}
		if s.buf[s.Pos] != ':' {
			return ErrBadObject
		}
		s.Pos++
		if err := s.skipValueAVX512(depth); err != nil {
			return err
		}
		if err := s.SkipSpaceAVX512(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return NotEOF(err, ErrBadObject)
			}
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			// Skip whitespace between ',' and the next key, as the scalar
			// skipObject does.
			if err := s.SkipSpaceAVX512(); err != nil {
				return err
			}
			continue
		}
		if s.buf[s.Pos] == '}' {
			s.Pos++
			return nil
		}
		return ErrBadObject
	}
}
