//go:build goexperiment.simd

// Fused SIMD skip tree for the Stream path. Same window discipline as the
// stream string tiers: refill/compaction bookkeeping is copied from the
// scalar tree bit-identically, only the per-window scans change —
// skipString* locates the next structural byte with structuralIndex*, and
// SkipSpace*'s slow path classifies whole lanes of whitespace instead of
// byte-stepping (pretty-printed streams). Per-tier copies with direct
// callees; no func-pointer dispatch. Generated stream decoders swap
// `= s.SkipValue()` / `= s.SkipSpace()` to a tier at generate time.

package scan

import (
	"io"
	"math/bits"
	"simd/archsimd"
)

// SkipSpaceAVX is Stream.SkipSpace with a vector whitespace-run skip.
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
			v := archsimd.LoadUint8x16Slice(buf[i:])
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

// SkipSpaceAVX2 is Stream.SkipSpace with a 32-byte vector run skip.
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
			v := archsimd.LoadUint8x32Slice(buf[i:])
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

// SkipSpaceAVX512 is Stream.SkipSpace with a 64-byte vector run skip.
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
			v := archsimd.LoadUint8x64Slice(buf[i:])
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

// skipStringStreamTail validates the escape at s.buf[bs] mid-skip, refilling
// as needed — the shared cold arm of the stream skipString tiers (same
// semantics as the scalar escape branch, incl. cursor rebase). Returns
// the updated (j, start) scan cursors.
func (s *Stream) skipStringStreamTail(start, bs int) (int, int, error) {
	if bs+1 >= len(s.buf) {
		if err := s.ReadMore(bs); err != nil {
			return 0, 0, notEOF(err, ErrBadString)
		}
		start = 0
		bs = 0
	}
	switch s.buf[bs+1] {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		return bs + 2, start, nil
	case 'u':
		for bs+6 > len(s.buf) {
			if err := s.ReadMore(bs); err != nil {
				return 0, 0, notEOF(err, ErrBadString)
			}
			start = 0
			bs = 0
		}
		if _, ok := parseHex4(s.buf[bs+2 : bs+6]); !ok {
			return 0, 0, ErrBadString
		}
		return bs + 6, start, nil
	default:
		return 0, 0, ErrBadString
	}
}

// skipStringAVX is Stream.skipString with the fused AVX window locate.
func (s *Stream) skipStringAVX() error {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return err
		}
		i = 0
	}
	if s.buf[i] != '"' {
		return ErrExpectString
	}
	start := i + 1
	j := start
	for {
		k := structuralIndexAVX(s.buf[j:])
		if k < 0 {
			// Skipped bytes are discardable — full compaction (see scalar).
			j = len(s.buf)
			err := s.ReadMore(j)
			j = 0
			start = 0
			if err != nil {
				return notEOF(err, ErrUnterminated)
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
			return ErrBadString
		}
	}
}

// skipStringAVX2 — see skipStringAVX.
func (s *Stream) skipStringAVX2() error {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return err
		}
		i = 0
	}
	if s.buf[i] != '"' {
		return ErrExpectString
	}
	start := i + 1
	j := start
	for {
		k := structuralIndexAVX2(s.buf[j:])
		if k < 0 {
			// Skipped bytes are discardable — full compaction (see scalar).
			j = len(s.buf)
			err := s.ReadMore(j)
			j = 0
			start = 0
			if err != nil {
				return notEOF(err, ErrUnterminated)
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
			return ErrBadString
		}
	}
}

// skipStringAVX512 — see skipStringAVX.
func (s *Stream) skipStringAVX512() error {
	i := s.Pos
	if i >= len(s.buf) {
		if err := s.ReadMore(i); err != nil {
			return err
		}
		i = 0
	}
	if s.buf[i] != '"' {
		return ErrExpectString
	}
	start := i + 1
	j := start
	for {
		k := structuralIndexAVX512(s.buf[j:])
		if k < 0 {
			// Skipped bytes are discardable — full compaction (see scalar).
			j = len(s.buf)
			err := s.ReadMore(j)
			j = 0
			start = 0
			if err != nil {
				return notEOF(err, ErrUnterminated)
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
			return ErrBadString
		}
	}
}

// skipNull consumes the "ull" tail of a null literal — shared by the
// SkipValue tiers (byte-identical to the scalar SkipValue's `n` arm).
func (s *Stream) skipNull() error {
	j := s.Pos
	for k := range 3 {
		pos := j + 1 + k
		if pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return notEOF(err, ErrBadLiteral)
			}
		}
		if s.buf[pos] != "ull"[k] {
			return ErrBadLiteral
		}
	}
	s.Pos = j + 4
	return nil
}

// SkipValueAVX is Stream.SkipValue over the fused AVX skip tree.
func (s *Stream) SkipValueAVX() error {
	return s.skipValueAVX(0)
}

func (s *Stream) skipValueAVX(depth int) error {
	if err := s.SkipSpaceAVX(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return ErrUnexpectedEnd
		}
		s.Pos = 0
	}
	switch s.buf[s.Pos] {
	case '"':
		return s.skipStringAVX()
	case 't', 'f':
		_, err := s.Bool()
		return err
	case 'n':
		return s.skipNull()
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
	if depth > MaxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpaceAVX(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return notEOF(err, ErrBadArray)
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
				return notEOF(err, ErrBadArray)
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
	if depth > MaxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpaceAVX(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return notEOF(err, ErrBadObject)
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == '}' {
		s.Pos++
		return nil
	}
	for {
		if err := s.skipStringAVX(); err != nil {
			return err
		}
		if err := s.SkipSpaceAVX(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return notEOF(err, ErrBadObject)
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
				return notEOF(err, ErrBadObject)
			}
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			// Separator WS before the next key — mirrors the scalar fix.
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

// SkipValueAVX2 is Stream.SkipValue over the fused AVX2 skip tree.
func (s *Stream) SkipValueAVX2() error {
	return s.skipValueAVX2(0)
}

func (s *Stream) skipValueAVX2(depth int) error {
	if err := s.SkipSpaceAVX2(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return ErrUnexpectedEnd
		}
		s.Pos = 0
	}
	switch s.buf[s.Pos] {
	case '"':
		return s.skipStringAVX2()
	case 't', 'f':
		_, err := s.Bool()
		return err
	case 'n':
		return s.skipNull()
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
	if depth > MaxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpaceAVX2(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return notEOF(err, ErrBadArray)
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
				return notEOF(err, ErrBadArray)
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
	if depth > MaxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpaceAVX2(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return notEOF(err, ErrBadObject)
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == '}' {
		s.Pos++
		return nil
	}
	for {
		if err := s.skipStringAVX2(); err != nil {
			return err
		}
		if err := s.SkipSpaceAVX2(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return notEOF(err, ErrBadObject)
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
				return notEOF(err, ErrBadObject)
			}
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			// Separator WS before the next key — mirrors the scalar fix.
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

// SkipValueAVX512 is Stream.SkipValue over the fused AVX-512 skip tree.
func (s *Stream) SkipValueAVX512() error {
	return s.skipValueAVX512(0)
}

func (s *Stream) skipValueAVX512(depth int) error {
	if err := s.SkipSpaceAVX512(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return ErrUnexpectedEnd
		}
		s.Pos = 0
	}
	switch s.buf[s.Pos] {
	case '"':
		return s.skipStringAVX512()
	case 't', 'f':
		_, err := s.Bool()
		return err
	case 'n':
		return s.skipNull()
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

// CaptureValueAVX / AVX2 / AVX512 are Stream.CaptureValue over the fused
// bytes-path skip tiers — the value is located by the vector skip, capture is
// still just grow-and-slice. See Stream.CaptureValue.
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
			s.Pos = start
			return nil, err
		}
		// A failure at a byte strictly inside the window is final — see
		// Stream.CaptureValue; without this a live reader that delivered a
		// complete malformed value blocks in Read forever.
		if at, aerr := skipValueAt(s.buf, start); aerr != nil && at < len(s.buf) {
			s.Pos = start
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
			s.Pos = start
			return nil, err
		}
		// A failure at a byte strictly inside the window is final — see
		// Stream.CaptureValue; without this a live reader that delivered a
		// complete malformed value blocks in Read forever.
		if at, aerr := skipValueAt(s.buf, start); aerr != nil && at < len(s.buf) {
			s.Pos = start
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
			s.Pos = start
			return nil, err
		}
		// A failure at a byte strictly inside the window is final — see
		// Stream.CaptureValue; without this a live reader that delivered a
		// complete malformed value blocks in Read forever.
		if at, aerr := skipValueAt(s.buf, start); aerr != nil && at < len(s.buf) {
			s.Pos = start
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
	if depth > MaxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpaceAVX512(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return notEOF(err, ErrBadArray)
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
				return notEOF(err, ErrBadArray)
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
	if depth > MaxDepth {
		return ErrMaxDepth
	}
	if err := s.SkipSpaceAVX512(); err != nil {
		return err
	}
	if s.Pos >= len(s.buf) {
		if err := s.ReadMore(s.Pos); err != nil {
			return notEOF(err, ErrBadObject)
		}
		s.Pos = 0
	}
	if s.buf[s.Pos] == '}' {
		s.Pos++
		return nil
	}
	for {
		if err := s.skipStringAVX512(); err != nil {
			return err
		}
		if err := s.SkipSpaceAVX512(); err != nil {
			return err
		}
		if s.Pos >= len(s.buf) {
			if err := s.ReadMore(0); err != nil {
				return notEOF(err, ErrBadObject)
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
				return notEOF(err, ErrBadObject)
			}
		}
		if s.buf[s.Pos] == ',' {
			s.Pos++
			// Separator WS before the next key — mirrors the scalar fix.
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
